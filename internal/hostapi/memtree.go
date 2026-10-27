// memtree.go is the builder: one worker per project store turns the log into
// a binary tree of 512-byte one-line summaries and keeps the view, the
// sawtooth list of lines a turn reads. Its tree state is owned by its worker
// goroutine; appends reach it through Wake, finished model calls through
// results, and a reset through RebuildQueues. No lease: two windows race on
// INSERT OR IGNORE and on view merges skipped for children already gone, so
// a race wastes one cheap call and nothing else.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

const (
	// nodeBytes is a node's whole budget: 512 bytes is a dense paragraph.
	nodeBytes = 512
	// The view grows one line per message to viewCeiling, then one batch
	// merges the most due pairs down to viewFloor.
	viewCeiling = 128_000
	viewFloor   = 64_000
	// The compaction view is the second sawtooth, an octave below.
	cvCeiling = 32_000
	cvFloor   = 16_000
	// buildTryMax is the same-conversation tries a line earns, the shortest
	// kept; buildSlots is the model calls one builder runs at once.
	buildTryMax = 5
	buildSlots  = 8
	// buildMaxOutput is a builder call's reply budget when the model names
	// none: a reasoning model spends output thinking before it writes the
	// line, so the old 2048 starved it into empty replies.
	buildMaxOutput = 8192
	// leafWaitLines: a leaf builds once fewer than this many lines before it
	// are still unbuilt, so the placeholder tail stays about this long.
	leafWaitLines = 8
	// syncBatch bounds one read of the appends a wake stands for.
	syncBatch = 1000
)

// placeholder is the line a message shows until its node is built.
const placeholder = "(not summarized yet: zoom it)"

// errEmptyReply is a compaction that came back with no line: the model spent
// its output on reasoning or a cap cut it, so the same conversation answers
// the same way and no retry can fix it - the node parks on its first one.
var errEmptyReply = errors.New("empty compaction reply")

// errWiped: the log shrank under the builder's cursor - another window's
// /reset. The run loop answers it with a rebuild.
var errWiped = errors.New("the chat was reset in another window")

// nodeID names one tree node: node(l,i) summarizes the 2^l messages from
// i*2^l on. The model reads it as "id+n" and zooms it open.
type nodeID struct {
	l, i int64
}

// first is the node's first message, its key in the view.
func (n nodeID) first() int64 { return n.i << n.l }

// span is how many messages the node covers.
func (n nodeID) span() int64 { return int64(1) << n.l }

// last is the node's last message.
func (n nodeID) last() int64 { return n.first() + n.span() - 1 }

// id renders the node as the view and zoom name it: "40+8".
func (n nodeID) id() string {
	return strconv.FormatInt(n.first(), 10) + "+" + strconv.FormatInt(n.span(), 10)
}

// key is the node's stable item identity across rebuilds.
func (n nodeID) key() string {
	return strconv.FormatInt(n.l, 10) + ":" + strconv.FormatInt(n.i, 10)
}

// args is the node as a /memories subcommand's arguments: "3 12".
func (n nodeID) args() string {
	return strconv.FormatInt(n.l, 10) + " " + strconv.FormatInt(n.i, 10)
}

// viewLine is one line of a view: a node and the bytes its text fills, the
// placeholder's when the node is not built yet.
type viewLine struct {
	id   nodeID
	size int
}

// viewSize is how many bytes a view's lines fill.
func viewSize(view []viewLine) int {
	var n int
	for _, ln := range view {
		n += ln.size
	}
	return n
}

// cmpDue orders two mergeable pairs most due first: how long ago each ended,
// measured in its own line size, the older pair on ties. Exact rational
// compare, no floats.
func cmpDue(t, lastA, spanA, lastB, spanB int64) int {
	if c := cmp.Compare((t-lastA)*spanB, (t-lastB)*spanA); c != 0 {
		return -c
	}
	return cmp.Compare(lastA, lastB)
}

// nextDueMerge picks the view's most due sibling pair and returns the index
// of its first line: due = (t - last) / 2^l, the pair's own line size as the
// measure, the oldest of equal pairs. size answers for built parents; a pair
// whose parent has no line yet cannot merge. Pure: the push-equivalence test
// drives it against the rollback push, the batches against real sizes.
func nextDueMerge(view []viewLine, t int64, size func(nodeID) (int, bool)) (int, bool) {
	best, found := 0, false
	var bestLast, bestSpan int64
	for k := range len(view) - 1 {
		a, c := view[k].id, view[k+1].id
		if a.l != c.l || c.i != a.i+1 || a.i%2 != 0 {
			continue
		}
		parent := nodeID{a.l + 1, a.i / 2}
		if _, ok := size(parent); !ok {
			continue
		}
		last, span := c.last(), a.span()
		if !found || cmpDue(t, last, span, bestLast, bestSpan) < 0 {
			best, bestLast, bestSpan, found = k, last, span, true
		}
	}
	return best, found
}

// mergeDown merges view in place, one most due pair at a time whose parent
// has a line, until stop(view) or no pair is left, and returns the view it
// made and the parents that landed. Pure.
func mergeDown(view []viewLine, t int64, size func(nodeID) (int, bool), stop func([]viewLine) bool) ([]viewLine, []nodeID) {
	var merged []nodeID
	for !stop(view) {
		k, ok := nextDueMerge(view, t, size)
		if !ok {
			break
		}
		parent := nodeID{view[k].id.l + 1, view[k].id.i / 2}
		n, _ := size(parent)
		// the parent takes the first child's place: its ord is the first
		// child's first message
		view[k] = viewLine{parent, n}
		view = slices.Delete(view, k+1, k+2)
		merged = append(merged, parent)
	}
	return view, merged
}

// viewMerges converts merged parents into the store's merge shape.
func viewMerges(parents []nodeID) []store.ViewMerge {
	out := make([]store.ViewMerge, len(parents))
	for i, p := range parents {
		out[i] = store.ViewMerge{L: p.l, I: p.i}
	}
	return out
}

// buildResult is one finished build job: a leaf's line, or a merge's.
type buildResult struct {
	leaf   bool
	seq    int64  // a leaf's message
	parent nodeID // a merge's parent
	text   string
	err    error
}

// memTree is the builder worker over one project store.
type memTree struct {
	h       *Host
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{} // closed when the worker exits
	calls   sync.WaitGroup
	wake    chan struct{} // size 1: an append landed
	rebld   chan struct{} // size 1: the queues must rebuild
	results chan buildResult

	callMu     sync.Mutex
	flight     int                // model calls in flight
	leafFlight int                // leaf calls in flight: the unbuilt lines a leaf waits behind
	held       bool               // dispatch paused (a reset quiesces); guarded by callMu
	burst      chan struct{}      // closed when the burst's first response starts
	force      chan chan struct{} // the overflow guard's one batch, acked when it ran
	ready      atomic.Bool        // the boot gate: no dispatch before the config step ran

	// owned by the worker goroutine
	msgs       int64          // messages in the log: seqs 0..msgs-1, dense
	seen       int64          // the last seq the worker synced
	built      map[nodeID]int // built nodes and their line sizes
	view       []viewLine     // the view, oldest first
	viewBytes  int
	leaves     []int64    // pending leaves, in message order
	leafRetry  []int64    // failed leaves, retried when fresh rows land
	merges     []nodeID   // pending merges, as the parents that would land
	mergeRetry []nodeID   // failed merges, retried when fresh rows land
	draining   bool       // a batch left the view above the floor
	cv         []viewLine // the compaction view, the second sawtooth
	cvBytes    int

	// health is what /health reads of the queues; the worker rewrites it
	// each round, so a reader never takes the worker's state.
	health builderHealth
}

// builderHealth is the builder's queue depth, unbuilt lines and failed
// builds, what /health's builder row says.
type builderHealth struct {
	unbuilt, failed, queued atomic.Int64
}

// quiesceWait bounds a reset's wait for the in-flight builds: a provider
// call wedged past it must refuse the reset, never wedge it forever.
var quiesceWait = 15 * time.Second

// newMemTree starts the builder worker on ctx; the caller's Close ends it.
func newMemTree(ctx context.Context, h *Host) *memTree {
	ctx, cancel := context.WithCancel(ctx)
	b := &memTree{
		h:       h,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
		rebld:   make(chan struct{}, 1),
		force:   make(chan chan struct{}, 1),
		results: make(chan buildResult),
		built:   map[nodeID]int{},
	}
	h.GoSafe("memtree", b.run)
	return b
}

// Wake tells the builder an append landed. Never blocks: the next round
// syncs from the store, which is the truth.
func (b *memTree) Wake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// RebuildQueues tells the builder the log moved under it (a reset wiped it):
// queues and the view reload from the store on the next round.
func (b *memTree) RebuildQueues() {
	select {
	case b.rebld <- struct{}{}:
	default:
	}
}

// MergeOnce merges one batch now, whatever the view's size, and returns
// once it ran: the overflow guard's retry rides it when the provider
// refused the context as too large, and must send the merged view, not the
// bytes that were refused.
func (b *memTree) MergeOnce() {
	ack := make(chan struct{})
	select {
	case b.force <- ack:
	case <-b.ctx.Done():
		return
	}
	select {
	case <-ack:
	case <-b.ctx.Done():
	}
}

// quiesce stops dispatch and holds the fence: no build dispatches after it,
// and every build dispatched before it has landed, so a reset's wipe runs
// under no build. resume releases it. It returns an error when the fence
// cannot hold - the host is closing, or an in-flight build outlived
// quiesceWait (a wedged provider call refuses the reset instead of wedging
// it) - and releases the gate itself then, so the caller must not cut.
func (b *memTree) quiesce() error {
	b.callMu.Lock()
	b.held = true
	b.callMu.Unlock()
	done := make(chan struct{})
	b.h.GoSafe("memtree quiesce", func() {
		b.calls.Wait()
		close(done)
	})
	timer := time.NewTimer(quiesceWait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		b.callMu.Lock()
		b.held = false
		b.callMu.Unlock()
		return fmt.Errorf("the builder's in-flight calls did not settle within %s", quiesceWait)
	case <-b.ctx.Done():
		return errors.New("zemi is closing") // the process is going down with the builds
	}
	// the zero result walks the fence: results are received in the order
	// they were sent, so once the worker takes it, every build dispatched
	// before the gate has applied
	b.send(buildResult{})
	return nil
}

func (b *memTree) resume() {
	b.callMu.Lock()
	b.held = false
	b.callMu.Unlock()
	b.RebuildQueues()
}

// release opens the boot gate and wakes the worker: the config step ran, so
// the builds it held dispatch now. Idempotent, and a reload replays it.
func (b *memTree) release() {
	if b.ready.CompareAndSwap(false, true) {
		b.Wake()
	}
}

// Close ends the builder: the context cancels, in-flight model calls abort,
// and Close returns once every builder goroutine exited.
func (b *memTree) Close() {
	b.cancel()
	<-b.done
	b.calls.Wait()
}

func (b *memTree) run() {
	defer close(b.done)
	for b.ctx.Err() == nil {
		if err := b.rebuild(); err == nil {
			break
		} else {
			// the store hiccuped at boot: the rebuild retries on the next
			// wake with its state still empty, so no half-built round runs
			b.h.Log.Warn("memtree boot failed", "origin", "agent", "error", err)
			select {
			case <-b.ctx.Done():
				return
			case <-b.wake:
			case <-b.rebld:
			}
		}
	}
	for {
		if err := b.syncNew(); err != nil {
			if errors.Is(err, errWiped) {
				// the chat was wiped: start over from its log
				if err := b.rebuild(); err != nil {
					b.h.Log.Error("memtree rebuild failed", "origin", "agent", "error", err)
				}
			} else {
				b.h.Log.Warn("memtree sync failed", "origin", "agent", "error", err)
			}
		}
		b.dispatch()
		b.batch(false)
		select {
		case <-b.ctx.Done():
			return
		case <-b.wake:
		case <-b.rebld:
			if err := b.rebuild(); err != nil {
				b.h.Log.Error("memtree rebuild failed", "origin", "agent", "error", err)
			}
		case ack := <-b.force:
			b.batch(true)
			close(ack)
		case r := <-b.results:
			b.apply(r)
		}
		b.reportHealth()
	}
}

// reportHealth rewrites what /health reads of the queues (worker only).
func (b *memTree) reportHealth() {
	unbuilt := 0
	for _, ln := range b.view {
		if _, ok := b.built[ln.id]; !ok {
			unbuilt++
		}
	}
	b.health.unbuilt.Store(int64(unbuilt))
	b.health.failed.Store(int64(len(b.leafRetry) + len(b.mergeRetry)))
	b.health.queued.Store(int64(len(b.leaves) + len(b.merges)))
}

// rebuild reloads everything from the store: at boot, and after a reset wipe
// moved the log under the builder.
func (b *memTree) rebuild() error {
	count, err := b.h.DB.LogCount()
	if err != nil {
		return err
	}
	nodes, err := b.h.DB.BuiltNodes()
	if err != nil {
		return err
	}
	b.built = make(map[nodeID]int, len(nodes))
	for _, n := range nodes {
		b.built[nodeID{n.L, n.I}] = n.Size
	}
	if err := b.resyncView(); err != nil {
		return err
	}
	b.msgs, b.seen = count, count-1
	b.leaves = b.leaves[:0]
	for seq := range count {
		if _, ok := b.built[nodeID{0, seq}]; !ok {
			b.leaves = append(b.leaves, seq)
		}
	}
	b.merges = b.merges[:0]
	for id := range b.built {
		if id.i%2 != 0 {
			continue // one pass per pair, from its first child
		}
		if _, ok := b.built[nodeID{id.l, id.i + 1}]; !ok {
			continue
		}
		parent := nodeID{id.l + 1, id.i / 2}
		if !b.isBuilt(parent) {
			b.merges = append(b.merges, parent)
		}
	}
	// the call queue runs most due first; later eligibles append as they come
	slices.SortFunc(b.merges, func(a, c nodeID) int {
		return cmpDue(b.msgs, a.last(), a.span()/2, c.last(), c.span()/2)
	})
	b.leafRetry, b.mergeRetry = nil, nil
	b.draining = b.viewBytes > viewCeiling
	return b.loadCV()
}

func (b *memTree) isBuilt(id nodeID) bool {
	_, ok := b.built[id]
	return ok
}

func (b *memTree) sizeOf(id nodeID) (int, bool) {
	n, ok := b.built[id]
	return n, ok
}

// resyncView reloads the view mirror from the store: a batch wrote it, and
// another window may have merged past this one's back.
func (b *memTree) resyncView() error {
	lines, err := b.h.DB.LoadView()
	if err != nil {
		return err
	}
	b.view = b.view[:0]
	b.viewBytes = 0
	for _, ln := range lines {
		size := len(placeholder)
		if n, ok := b.built[nodeID{ln.L, ln.I}]; ok {
			size = n
		}
		b.view = append(b.view, viewLine{nodeID{ln.L, ln.I}, size})
		b.viewBytes += size
	}
	return nil
}

// syncNew learns the appends that landed since the last round: their view
// lines, their leaves, the template-built ones' sizes. Failed builds rejoin
// their queues here, per their streak. A log that shrank under b.seen is
// another window's /reset: the wiped seqs are new rows now, so rebuild.
func (b *memTree) syncNew() error {
	fresh := false
	for {
		rows, err := b.h.DB.LogFrom(b.seen+1, syncBatch)
		if err != nil {
			return err
		}
		if count, err := b.h.DB.LogCount(); err == nil && count < b.msgs {
			return errWiped
		} else if err != nil {
			return err
		}
		fresh = fresh || len(rows) > 0
		for _, e := range rows {
			b.seen = e.Seq
			b.msgs = e.Seq + 1
			line := viewLine{id: nodeID{0, e.Seq}, size: len(placeholder)}
			switch e.Kind {
			case kindTool, kindEcho:
				// template-built in the append transaction: take its line
				n, err := b.h.DB.NodeAt(0, e.Seq)
				if errors.Is(err, store.ErrNotFound) {
					// the row was rewound between the read and here
				} else if err != nil {
					return err
				} else {
					b.built[line.id] = n.Size
					line.size = n.Size
					b.queueMerge(line.id)
				}
			default:
				b.leaves = append(b.leaves, e.Seq)
			}
			b.addLine(line)
		}
		if len(rows) < syncBatch {
			b.requeue(fresh)
			return nil
		}
	}
}

// requeue moves failed builds back into their queues when fresh rows landed -
// a failed call tries again at the next message, never in a timer loop - and
// drops nodes the log no longer holds.
func (b *memTree) requeue(fresh bool) {
	if !fresh {
		return
	}
	keep := b.leafRetry[:0]
	for _, seq := range b.leafRetry {
		if seq >= b.msgs { // the row is gone (rewound)
			continue
		}
		b.leaves = append(b.leaves, seq)
	}
	b.leafRetry = keep
	keepM := b.mergeRetry[:0]
	for _, parent := range b.mergeRetry {
		if (parent.i+1)*(1<<parent.l) > b.msgs { // a half is gone (rewound)
			continue
		}
		b.merges = append(b.merges, parent)
	}
	b.mergeRetry = keepM
}

// addLine appends one line to the view and the compaction view: a new
// message's leaf, a placeholder until its node lands.
func (b *memTree) addLine(line viewLine) {
	b.view = append(b.view, line)
	b.viewBytes += line.size
	b.cv = append(b.cv, line)
	b.cvBytes += line.size
}

// dispatch fills the build slots: leaves first, then merges whose halves are
// both built. A quiesce holds it: no call dispatches into a reset's wipe.
// The boot gate holds it too: no call dispatches before the config step ran,
// when no provider exists yet to answer one.
func (b *memTree) dispatch() {
	if b.heldNow() || !b.ready.Load() {
		return
	}
	for b.slots() < buildSlots {
		if b.dispatchLeaf() {
			continue
		}
		if b.dispatchMerge() {
			continue
		}
		return
	}
}

// heldNow reads the quiesce gate; takeSlot checks it again under the same
// lock, so a gate set after the check cannot lose the call it decided.
func (b *memTree) heldNow() bool {
	b.callMu.Lock()
	defer b.callMu.Unlock()
	return b.held
}

func (b *memTree) slots() int {
	b.callMu.Lock()
	defer b.callMu.Unlock()
	return b.flight
}

func (b *memTree) leafSlots() int {
	b.callMu.Lock()
	defer b.callMu.Unlock()
	return b.leafFlight
}

func (b *memTree) dispatchLeaf() bool {
	// the 8-line rule: the unbuilt lines before the queue's front are
	// exactly its leaf calls in flight
	if len(b.leaves) == 0 || b.leafSlots() >= leafWaitLines {
		return false
	}
	seq := b.leaves[0]
	e, err := b.h.DB.LogAt(seq)
	if errors.Is(err, store.ErrNotFound) {
		b.leaves = b.leaves[1:]
		return true // the row is gone (rewound): nothing to summarize
	}
	if err != nil {
		return false // a transient store error: the queue waits for the next round
	}
	b.leaves = b.leaves[1:]
	if len(e.Text) <= nodeBytes {
		// a short message is its own node verbatim, no model call
		b.insertBuilt(nodeID{0, seq}, e.Text)
		return true
	}
	b.takeLeaf() // synchronous: the 8-line gate counts it from now
	// the prompt reads the compaction view, worker-owned state: build it
	// here, before the call goroutine exists
	prompt := b.leafPrompt(seq, e.Kind, e.Text)
	first, wait, ok := b.takeSlot()
	if !ok { // quiesced: the resume's rebuild requeues the leaf
		b.endLeaf()
		return false
	}
	b.h.GoSafe("memtree leaf", func() {
		defer b.calls.Done()
		defer b.endLeaf()
		text, err := b.build([]fantasy.Message{agent.UserMessage(prompt)}, first, wait)
		b.send(buildResult{leaf: true, seq: seq, text: text, err: err})
	})
	return true
}

func (b *memTree) takeLeaf() {
	b.callMu.Lock()
	b.leafFlight++
	b.callMu.Unlock()
}

func (b *memTree) endLeaf() {
	b.callMu.Lock()
	b.leafFlight--
	b.callMu.Unlock()
}

func (b *memTree) dispatchMerge() bool {
	for len(b.merges) > 0 {
		parent := b.merges[0]
		a, errA := b.h.DB.NodeAt(parent.l-1, parent.i*2)
		c, errC := b.h.DB.NodeAt(parent.l-1, parent.i*2+1)
		if errA != nil || errC != nil {
			if !errors.Is(errA, store.ErrNotFound) && !errors.Is(errC, store.ErrNotFound) {
				return false // a transient store error: the queue waits for the next round
			}
			b.merges = b.merges[1:]
			continue // a half is gone (rewound): the pair is dead
		}
		b.merges = b.merges[1:]
		// the children's newline join is the parent when it fits: no call
		if join := a.Text + "\n" + c.Text; len(join) <= nodeBytes {
			b.insertBuilt(parent, join)
			return true
		}
		// the join's prompt reads the compaction view, worker-owned state:
		// build it here, before the call goroutine exists
		prompt := b.mergePrompt(a, c)
		first, wait, ok := b.takeSlot()
		if !ok { // quiesced: the resume's rebuild requeues the pair
			return false
		}
		b.h.GoSafe("memtree merge", func() {
			defer b.calls.Done()
			text, err := b.build([]fantasy.Message{agent.UserMessage(prompt)}, first, wait)
			b.send(buildResult{parent: parent, text: text, err: err})
		})
		return true
	}
	return false
}

// send hands a finished job to the worker; a Close that beat it drops it.
func (b *memTree) send(r buildResult) {
	select {
	case b.results <- r:
	case <-b.ctx.Done():
	}
}

// apply installs a finished build. A failure holds its place in the retry
// list: per the recipe, the call tries again at the next message - fresh
// rows requeue it, and the retry rides no timer. The view self-heals.
func (b *memTree) apply(r buildResult) {
	if r == (buildResult{}) {
		return // the quiesce fence's zero result
	}
	id := nodeID{0, r.seq}
	if !r.leaf {
		id = r.parent
	}
	if r.err != nil {
		if b.ctx.Err() == nil { // a Close's failures are not build failures
			b.h.RecordErrorOnce("chat builder", "", "build of "+id.id()+" failed: "+r.err.Error())
		}
		if r.leaf {
			b.leafRetry = append(b.leafRetry, r.seq)
		} else {
			b.mergeRetry = append(b.mergeRetry, r.parent)
		}
		return
	}
	if r.leaf {
		b.insertBuilt(nodeID{0, r.seq}, r.text)
		return
	}
	b.insertBuilt(r.parent, r.text)
}

// insertBuilt records a node the store accepted and moves its view line off
// the placeholder. A window that built it first wins; this one adopts its
// size. The merge the node completes joins the queue: both its halves are
// built now.
func (b *memTree) insertBuilt(id nodeID, text string) {
	inserted, err := b.h.DB.InsertNode(id.l, id.i, text)
	if err != nil {
		b.h.Log.Error("memtree node write failed", "origin", "agent", "error", err)
		return
	}
	size := len(text)
	if !inserted {
		n, err := b.h.DB.NodeAt(id.l, id.i)
		if err != nil {
			return
		}
		size = n.Size
	} else {
		b.queueMerge(id)
	}
	b.built[id] = size
	if id.l == 0 {
		b.setLineSize(nodeID{0, id.i}, size)
	}
}

// queueMerge queues the pair id completes, when its other half is built and
// the parent is not. Each parent enqueues once, at its second half.
func (b *memTree) queueMerge(id nodeID) {
	parent := nodeID{id.l + 1, (id.i &^ 1) / 2}
	if b.isBuilt(parent) || !b.isBuilt(nodeID{id.l, id.i ^ 1}) {
		return
	}
	b.merges = append(b.merges, parent)
}

// setLineSize moves one view line off the placeholder in the view and the
// compaction view, newest first: the line sits at the tail.
func (b *memTree) setLineSize(id nodeID, size int) {
	for _, at := range []struct {
		lines *[]viewLine
		bytes *int
	}{{&b.view, &b.viewBytes}, {&b.cv, &b.cvBytes}} {
		lines := *at.lines
		for k := len(lines) - 1; k >= 0; k-- {
			ln := lines[k]
			if ln.id == id {
				*at.bytes += size - ln.size
				lines[k].size = size
				break
			}
			if ln.id.first() < id.first() {
				break // the line merged away or the list never held it
			}
		}
	}
}

// batch merges the view when it is due: past the ceiling one batch merges
// the most due pairs down to the floor; a batch that could not reach the
// floor keeps merging what new parents allow at each round. The compaction
// view re-merges when the chat's view does. force merges regardless of the
// ceiling, what the overflow guard's retry needs.
func (b *memTree) batch(force bool) {
	if b.viewBytes > viewCeiling {
		b.draining = true
	}
	if b.viewBytes <= viewFloor {
		b.draining = false
	}
	if !b.draining && !force {
		return
	}
	_, plan := mergeDown(slices.Clone(b.view), b.msgs, b.sizeOf, func(v []viewLine) bool {
		return viewSize(v) <= viewFloor
	})
	if len(plan) == 0 {
		return
	}
	if _, err := b.h.DB.MergeViewLines(viewMerges(plan)); err != nil {
		b.h.Log.Error("memtree view merge failed", "origin", "agent", "error", err)
		return
	}
	if err := b.resyncView(); err != nil {
		b.h.Log.Error("memtree view reload failed", "origin", "agent", "error", err)
	}
	b.cvBatch(true)
}

// cvBatch re-merges the compaction view down to its floor, once it passes
// its ceiling or when the chat's view merged, and saves it.
func (b *memTree) cvBatch(force bool) {
	if !force && b.cvBytes <= cvCeiling {
		return
	}
	cv, plan := mergeDown(slices.Clone(b.cv), b.msgs, b.sizeOf, func(v []viewLine) bool {
		return viewSize(v) <= cvFloor
	})
	if len(plan) > 0 {
		b.cv, b.cvBytes = cv, viewSize(cv)
	}
	b.saveCV()
}

func (b *memTree) saveCV() {
	pairs := make([][]int64, 0, len(b.cv))
	for _, ln := range b.cv {
		pairs = append(pairs, []int64{ln.id.l, ln.id.i})
	}
	raw, err := json.Marshal(pairs, json.Deterministic(true))
	if err != nil {
		b.h.Log.Warn("memtree compaction view not saved", "origin", "agent", "error", err)
		return
	}
	if err := b.h.DB.SetKV(store.CVKey, string(raw)); err != nil {
		b.h.Log.Warn("memtree compaction view not saved", "origin", "agent", "error", err)
	}
}

func (b *memTree) loadCV() error {
	raw, err := b.h.DB.GetKV(store.CVKey)
	if errors.Is(err, store.ErrNotFound) {
		b.cv, b.cvBytes = nil, 0
		return nil
	}
	if err != nil {
		return err
	}
	var pairs [][]int64
	if err := json.Unmarshal([]byte(raw), &pairs); err != nil {
		// a value nothing can read is not kept: the next saveCV writes a
		// fresh one, and the view starts over instead of stopping every
		// build's context at a phantom line
		b.h.Log.Warn("memtree compaction view unreadable", "origin", "agent", "error", err)
		if _, err := b.h.DB.DelKV(store.CVKey); err != nil {
			b.h.Log.Warn("memtree compaction view not reset", "origin", "agent", "error", err)
		}
		b.cv, b.cvBytes = nil, 0
		return nil
	}
	b.cv, b.cvBytes = b.cv[:0], 0
	dropped := 0
	for _, p := range pairs {
		if len(p) != 2 || p[0] < 0 || p[1] < 0 || (p[1]+1)*(1<<p[0]) > b.msgs {
			// a line past the log's end is a cut's leftover: its node can
			// never build, so the view drops it rather than stop at it
			dropped++
			continue
		}
		id := nodeID{p[0], p[1]}
		size := len(placeholder)
		if n, ok := b.built[id]; ok {
			size = n
		}
		b.cv = append(b.cv, viewLine{id, size})
		b.cvBytes += size
	}
	if dropped > 0 {
		b.h.Log.Warn("memtree compaction view dropped lines", "origin", "agent", "lines", dropped)
	}
	return nil
}

// chatContext renders the compaction view as the <chat> a build call reads:
// the lines that end before end - no hindsight past the node's own range -
// and none past the first unbuilt one.
func (b *memTree) chatContext(end int64) string {
	var w strings.Builder
	for _, ln := range b.cv {
		if ln.id.last() >= end {
			break
		}
		n, err := b.h.DB.NodeAt(ln.id.l, ln.id.i)
		if err != nil {
			break // the first unbuilt line: no call ever sees a placeholder
		}
		fmt.Fprintf(&w, "%s|%s\n", ln.id.id(), strings.ReplaceAll(n.Text, "\n", " "))
	}
	return strings.TrimRight(w.String(), "\n")
}

// leafPrompt is the compaction task for one message, the compaction view
// before it.
func (b *memTree) leafPrompt(seq int64, kind, text string) string {
	var w strings.Builder
	if chat := b.chatContext(seq); chat != "" {
		fmt.Fprintf(&w, "<chat>\n%s\n</chat>\n\n", chat)
	}
	fmt.Fprintf(&w, "Compaction: compress message %d into one line of at most %d bytes\n(about 70 words), the length of this ruler:\n%s\n<input>\n%s: %s\n</input>",
		seq, nodeBytes, ruler, kind, text)
	return w.String()
}

// mergePrompt is the compaction task for two adjacent lines, the compaction
// view before it.
func (b *memTree) mergePrompt(a, c store.Node) string {
	var w strings.Builder
	if chat := b.chatContext(nodeID{c.L, c.I}.last() + 1); chat != "" {
		fmt.Fprintf(&w, "<chat>\n%s\n</chat>\n\n", chat)
	}
	fmt.Fprintf(&w, "Compaction: merge lines %s and %s, adjacent, into one line of at most\n%d bytes (about 70 words), the length of this ruler:\n%s\n<chat> may hold their messages, %d to %d, in more detail: take details\nof them from there too.\n<input>\n%s\n%s\n</input>",
		nodeID{a.L, a.I}.id(), nodeID{c.L, c.I}.id(), nodeBytes, ruler,
		nodeID{a.L, a.I}.first(), nodeID{c.L, c.I}.last(), a.Text, c.Text)
	return w.String()
}

// ruler shows the length the model cannot count.
var ruler = strings.Repeat("-", nodeBytes)

// tooLongNote is the same-conversation correction an over-long line earns.
func tooLongNote(reply string) string {
	return fmt.Sprintf("Too long: your line is %d bytes, over the %d-byte limit. Write\nthe whole line again for the same <input>, cutting just enough of the\nleast valuable items to fit before this cut:\n%s| <- LIMIT",
		len(reply), nodeBytes, clipBytes(reply, nodeBytes))
}

func assistantMessage(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

// build runs one compaction: up to buildTryMax tries in one conversation, the
// shortest fitting line kept. retry is nil on purpose: the retry path stomps
// the shared h.agents.retryAt a turn is counting down, and a builder burst
// would stomp it constantly. first and wait come from the slot takeCall
// dispatch already made synchronously.
func (b *memTree) build(convo []fantasy.Message, first bool, wait chan struct{}) (string, error) {
	defer b.endCall(first)
	id := b.h.modelRef("compact")
	model, err := b.h.resolveTurnModel(id)
	if err != nil {
		return "", err
	}
	if !first {
		if err := b.waitBurst(wait); err != nil {
			return "", err
		}
	}
	if first {
		model = burstModel{LanguageModel: model, started: sync.OnceFunc(b.openBurst)}
	}
	maxOut := cmp.Or(b.h.maxOutput(id), buildMaxOutput)
	// a builder call runs at the model's own lowest effort, when it lists	// levels: its default effort is what starved the line into reasoning. A
	// model that lists no levels gets the force-off on an openai_compat
	// gateway: effort low plus the no-thinking body field - reasoning models
	// spend the whole output cap on thinking and return no line otherwise
	// (measured on acme-gateway and DeepSeek: effort low alone turns thinking
	// ON on DeepSeek, the thinking field alone does nothing on the gateway).
	effort, suppress := "low", false
	opts := fantasy.ProviderOptions(nil)
	if m, ok := b.h.modelSpec(id); ok && len(m.Efforts) > 0 {
		effort = b.h.Providers.Effort(agent.ModelID(id), "low")
		opts = b.h.Providers.CallOptions(agent.ModelID(id), effort, store.MainSessionID)
	} else if prov, _, ok := agent.ModelID(id).Split(); ok {
		if spec, ok := b.h.Providers.Lookup(prov); ok {
			switch spec.Kind {
			case agent.KindOpenAICompat:
				suppress = true
				opts = openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
					ReasoningEffort: new(openai.ReasoningEffort(effort)),
					ExtraBody:       map[string]any{"thinking": map[string]string{"type": "disabled"}},
				})
			default:
				opts = b.h.Providers.CallOptions(agent.ModelID(id), effort, store.MainSessionID)
			}
		}
	}
	var best string
	// resampled: a clean finish with no line is not deterministic the way the
	// output cap is - the model said nothing this sample and can say the line
	// on the next one, so the reply gets one fresh sample before parking.
	resampled := false
	for range buildTryMax {
		res, err := fantasy.NewAgent(model, fantasy.WithSystemPrompt(chatSystem)).Stream(b.ctx, fantasy.AgentStreamCall{
			Messages:        convo,
			MaxOutputTokens: &maxOut,
			StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
			ProviderOptions: opts,
			Headers:         agent.SessionHeaders(store.MainSessionID),
		})
		if err != nil {
			return "", err
		}
		b.h.bill(store.MainSessionID, id, res.TotalUsage)
		reply := strings.TrimSpace(concatText(res.Response.Content))
		switch {
		case reply == "":
			// a cap burn is the reasoning budget, not the line: the model
			// spent the whole reply thinking, so a fresh sample at the same
			// cap burns again but a doubled cap leaves room for the line
			if res.Response.FinishReason == fantasy.FinishReasonLength && maxOut < 4*buildMaxOutput {
				maxOut *= 2
				continue
			}
			if res.Response.FinishReason == fantasy.FinishReasonLength {
				if suppress {
					return "", fmt.Errorf("%w at the output cap even with reasoning off - the model ignores the suppression; pick a plain model for models.compact", errEmptyReply)
				}
				return "", fmt.Errorf("%w at the output cap - lower the model's reasoning effort or raise its output cap", errEmptyReply)
			}
			if resampled {
				return "", fmt.Errorf("%w - the model returned no line twice", errEmptyReply)
			}
			resampled = true
		case len(reply) <= nodeBytes:
			return reply, nil
		default:
			if best == "" || len(reply) < len(best) {
				best = reply
			}
			convo = append(convo, assistantMessage(reply), agent.UserMessage(tooLongNote(reply)))
		}
	}
	return best, nil
}

// takeSlot admits one build to the shared slots synchronously, so dispatch
// and the quiesce fence count the call from the moment dispatch decides it,
// and reports the burst a non-first call waits behind.
func (b *memTree) takeSlot() (first bool, wait chan struct{}, ok bool) {
	b.callMu.Lock()
	defer b.callMu.Unlock()
	if b.held { // quiesced for a reset: no call dispatches into the wipe
		return false, nil, false
	}
	b.calls.Add(1) // the fence counts the call from here, before the spawn
	b.flight++
	if b.flight == 1 {
		b.burst = make(chan struct{})
		first = true
	}
	return first, b.burst, true
}

// waitBurst holds a non-first call until the burst's first response started:
// two calls writing one cache entry pay for it twice (the spec's
// prefix-write wait).
func (b *memTree) waitBurst(wait chan struct{}) error {
	if wait == nil { // the burst already opened
		return nil
	}
	select {
	case <-wait:
		return nil
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
}

func (b *memTree) endCall(first bool) {
	b.callMu.Lock()
	b.flight--
	if first && b.burst != nil {
		// the first call ended without a response: the burst opens anyway
		close(b.burst)
		b.burst = nil
	}
	b.callMu.Unlock()
}

// openBurst opens the burst: the first call's response has started, so the
// cache entry it writes is readable and the rest may run.
func (b *memTree) openBurst() {
	b.callMu.Lock()
	if ch := b.burst; ch != nil {
		b.burst = nil
		close(ch)
	}
	b.callMu.Unlock()
}

// burstModel signals a call's response start: the request's cache entry is
// written once the provider begins to reply.
type burstModel struct {
	fantasy.LanguageModel
	started func()
}

func (m burstModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	res, err := m.LanguageModel.Stream(ctx, call)
	if err == nil {
		m.started()
	}
	return res, err
}
