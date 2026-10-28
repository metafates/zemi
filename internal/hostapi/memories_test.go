// memories_test.go pins /memories: the live view one row per line, the
// drill-in a digest's l opens, the placeholders unbuilt leaves carry, and
// the jump a leaf's anchor names in the chat.

package hostapi

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// seedView writes a view the browser reads: six messages, the first two
// merged into node(1,0), the next two into node(1,1), the last two leaves,
// the last one unbuilt. The log rows are long, so the builder's leaf builds
// fail (no model) and the view stays as seeded.
func seedView(t *testing.T, h *Host) {
	t.Helper()
	for range 6 {
		if _, err := h.DB.AppendLog(store.KindUser, strings.Repeat("row ", 200), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if _, err := h.DB.InsertNode(0, int64(i), "msg "+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.InsertNode(1, 0, "msgs 0-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.InsertNode(1, 1, "msgs 2-3"); err != nil {
		t.Fatal(err)
	}
	if n, err := h.DB.MergeViewLines([]store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}}); err != nil || n != 2 {
		t.Fatalf("the merges: %d applied, %v", n, err)
	}
}

// lines is the rows the overlay lists, in order.
func lines(o Overlay) []string {
	out := make([]string, len(o.Items))
	for i, it := range o.Items {
		out[i] = it.Text
	}
	return out
}

// seedMemories boots a host with the seeded view and a fake screen.
func seedMemories(t *testing.T) (*Host, *fakeUI) {
	t.Helper()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	seedView(t, h)
	return h, ui
}

// seedMemoriesDeep seeds sixteen messages as one level-3 line over the tail's
// leaves, so the browser can zoom three levels deep.
func seedMemoriesDeep(t *testing.T) (*Host, *fakeUI) {
	t.Helper()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	for range 16 {
		if _, err := h.DB.AppendLog(store.KindUser, strings.Repeat("row ", 200), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 16 {
		if _, err := h.DB.InsertNode(0, int64(i), "msg "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	for j := range 8 {
		if _, err := h.DB.InsertNode(1, int64(j), "msgs "+strconv.Itoa(2*j)+"-"+strconv.Itoa(2*j+1)); err != nil {
			t.Fatal(err)
		}
	}
	for k := range 4 {
		if _, err := h.DB.InsertNode(2, int64(k), "msgs "+strconv.Itoa(4*k)+"-"+strconv.Itoa(4*k+3)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.InsertNode(3, 0, "msgs 0-7"); err != nil {
		t.Fatal(err)
	}
	// the merges climb: pairs first, then the level-2 parents, then the root
	merges := []store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}, {L: 2, I: 0}, {L: 1, I: 2}, {L: 1, I: 3}, {L: 2, I: 1}, {L: 3, I: 0}}
	if n, err := h.DB.MergeViewLines(merges); err != nil || n != 7 {
		t.Fatalf("the merges: %d applied, %v", n, err)
	}
	return h, ui
}

func TestMemoriesZoom(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)

	if err := h.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	o := h.memoriesOverlay()
	want := []string{"msgs 0-1", "msgs 2-3", "msg 4", "(not summarized yet: zoom it)"}
	if got := lines(o); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the view rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if o.Items[0].Run != "memories zoom 1 0" || o.Items[2].Run != "memories view 0 4" {
		t.Errorf("run = %q, leaf = %q", o.Items[0].Run, o.Items[2].Run)
	}
	if o.Items[0].Tag != "0+2" || o.Items[0].Copy != "0+2" {
		t.Errorf("tags: key %q copy %q", o.Items[0].Tag, o.Items[0].Copy)
	}
	if o.Items[0].Keys[1].Key != "v" || o.Items[0].Keys[1].Run != "memories view 1 0" {
		t.Errorf("v = %+v", o.Items[0].Keys[1])
	}
	if len(o.Items[0].Keys) != 2 {
		t.Errorf("extra keys on the root rows: %+v", o.Items[0].Keys)
	}

	// l drills into the line's two children as an overlay of its own, the
	// root list unchanged under it
	if err := h.MemoriesCommand("zoom 1 0"); err != nil {
		t.Fatal(err)
	}
	z, ok := h.memoriesZoomOverlay(nodeID{1, 0})
	if !ok {
		t.Fatal("the drill-in refused")
	}
	if z.ID != "memories zoom 1 0" || z.Title != "Memories / 0+2" || z.Body != "msgs 0-1" {
		t.Fatalf("the drill-in = %s %q body %q", z.ID, z.Title, z.Body)
	}
	if got := lines(z); strings.Join(got, "|") != "msg 0|msg 1" {
		t.Fatalf("the drill-in rows: %v", got)
	}
	if !slices.Contains(h.msgLevels, "memories zoom 1 0") {
		t.Errorf("the level is untracked: %v", h.msgLevels)
	}
	if got := h.memoriesOverlay().Items[0].Text; got != "msgs 0-1" {
		t.Errorf("the root row moved under the drill-in: %q", got)
	}

	// the flush rebuild reaches the drill-in level by its id
	lv, ok := h.messagesLevel("memories zoom 1 0", pickTr{})
	if !ok || lv.ID != "memories zoom 1 0" || len(lv.Items) != 2 {
		t.Fatalf("the tracked level rebuilt as %v ok=%v", lv.ID, ok)
	}

	// a leaf has no children, and the view's span reaches down, never up or
	// past the log
	for _, c := range []struct{ args, why string }{
		{"0 4", "a leaf"},
		{"3 1", "past the log"},
		{"2 0", "above the view"},
	} {
		if err := h.MemoriesCommand("zoom " + c.args); err == nil {
			t.Errorf("zooming %s ran (%s)", c.args, c.why)
		}
	}
}

// TestMemoriesZoomDeep: the drill-in climbs down three levels from the view,
// each level its own overlay naming the line it opened; a node above the
// view's coarsest line has nothing under it to open, and a node the log no
// longer reaches closes its level on the rebuild.
func TestMemoriesZoomDeep(t *testing.T) {
	t.Parallel()
	h, _ := seedMemoriesDeep(t)

	if err := h.MemoriesCommand("zoom 3 0"); err != nil {
		t.Fatal(err)
	}
	if err := h.MemoriesCommand("zoom 2 0"); err != nil {
		t.Fatal(err)
	}
	if err := h.MemoriesCommand("zoom 1 0"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   nodeID
		want string
	}{
		{nodeID{3, 0}, "msgs 0-3|msgs 4-7"},
		{nodeID{2, 0}, "msgs 0-1|msgs 2-3"},
		{nodeID{1, 0}, "msg 0|msg 1"},
	} {
		z, ok := h.memoriesZoomOverlay(c.id)
		if !ok {
			t.Fatalf("the drill-in for %s refused", c.id.id())
		}
		if got := strings.Join(lines(z), "|"); got != c.want {
			t.Errorf("zoom %s rows: %s, want %s", c.id.id(), got, c.want)
		}
		if z.Title != "Memories / "+c.id.id() {
			t.Errorf("zoom %s title %q", c.id.id(), z.Title)
		}
		if want := map[string]string{"0+8": "memories zoom 2 0", "0+4": "memories zoom 1 0", "0+2": "memories view 0 0"}[c.id.id()]; z.Items[0].Run != want {
			t.Errorf("zoom %s child run %q, want %q", c.id.id(), z.Items[0].Run, want)
		}
	}

	// a node the view's coarsest line does not reach has nothing under it
	if err := h.MemoriesCommand("zoom 9 0"); err == nil {
		t.Error("zooming above the view ran")
	}
	// the rebuild a flush runs: a level the log still reaches rebuilds, one
	// past the log's end closes
	if _, ok := h.messagesLevel("memories zoom 3 0", pickTr{}); !ok {
		t.Error("the log's level did not rebuild")
	}
	if lv, ok := h.messagesLevel("memories zoom 4 2", pickTr{}); ok || lv.ID != "memories zoom 4 2" {
		t.Errorf("a level past the log rebuilt as %v ok=%v, want closed", lv.ID, ok)
	}
	if _, ok := h.messagesLevel("memories", pickTr{}); !ok {
		t.Error("the browser did not rebuild")
	}
}

// TestMemoriesWindowReach: a line the chat window no longer holds says so;
// one it reaches reads clean, and every leaf's enter reads its messages -
// the browser is an overview and never moves the chat.
func TestMemoriesWindowReach(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)
	rows, err := h.DB.LogFrom(0, 6)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows)) // window seqs 0..5

	o := h.memoriesOverlay()
	for _, c := range []struct {
		line, run string
	}{
		{"msgs 0-1", "memories zoom 1 0"},
		{"msgs 2-3", "memories zoom 1 1"},
		{"msg 4", "memories view 0 4"},
		{"(not summarized yet: zoom it)", "memories view 0 5"},
	} {
		i := 0
		for ; i < len(o.Items) && o.Items[i].Text != c.line; i++ {
		}
		if i == len(o.Items) {
			t.Fatalf("no row %q", c.line)
		}
		if it := o.Items[i]; it.Desc != "" || it.Run != c.run || it.Verb != VerbOpen {
			t.Errorf("%s = %+v, want %s opened", c.line, it, c.run)
		}
	}
	// a drill-in's leaf children read their own messages
	z, ok := h.memoriesZoomOverlay(nodeID{1, 1})
	if !ok {
		t.Fatal("the drill-in refused")
	}
	if z.Items[0].Run != "memories view 0 2" || z.Items[1].Run != "memories view 0 3" {
		t.Errorf("children = %+v, %+v", z.Items[0], z.Items[1])
	}

	// the window cut down to its tail: the merged lines past it say so, the
	// tail's own lines read clean
	h.SetTranscript(logRows(rows[4:]))
	o = h.memoriesOverlay()
	if it := o.Items[0]; it.Desc != "older than the chat window" || it.Run != "memories zoom 1 0" {
		t.Errorf("a merged line past the window = %+v", it)
	}
	if it := o.Items[2]; it.Desc != "" || it.Run != "memories view 0 4" {
		t.Errorf("the tail's leaf = %+v", it)
	}
	// a leaf past the window still reads its messages
	h.SetTranscript(logRows(rows[5:]))
	o = h.memoriesOverlay()
	if it := o.Items[2]; it.Desc != "older than the chat window" ||
		it.Run != "memories view 0 4" || it.Verb != VerbOpen {
		t.Errorf("a leaf past the window = %+v", it)
	}
}

// TestMemoriesFoot: the footer reads the view's size, its lines and levels,
// and the unbuilt count.
func TestMemoriesFoot(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)
	o := h.memoriesOverlay()
	// three built lines ("msgs 0-1", "msgs 2-3", "msg 4") at 8+8+5 bytes,
	// one placeholder at its own length, across levels 0 and 1
	if !strings.Contains(o.Foot, "4 lines") || !strings.Contains(o.Foot, "2 levels") || !strings.Contains(o.Foot, "1 unbuilt") {
		t.Errorf("foot = %q", o.Foot)
	}
	if !strings.Contains(o.Foot, "B") {
		t.Errorf("foot names no byte size: %q", o.Foot)
	}
}

// TestMemoriesRange: v reads the messages a line covers, kind-headed, oldest
// first; a line past the log is refused.
func TestMemoriesRange(t *testing.T) {
	t.Parallel()
	h, ui := seedMemories(t)
	if err := h.MemoriesCommand("view 1 0"); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if o.ID != "memories range 1 0" || o.Title != "Memories / 0+2" {
		t.Fatalf("range view = %s %q", o.ID, o.Title)
	}
	// each row's head carries its date, the one place a non-coder reaches one
	if !strings.HasPrefix(o.Body, "user 20") || !strings.Contains(o.Body, ": "+strings.Repeat("row ", 200)) {
		t.Errorf("body = %q", clipRunes(o.Body, 120))
	}
	if err := h.MemoriesCommand("view 4 1"); err == nil {
		t.Error("a line past the log opened a view")
	}
}

// TestSeqZeroIsARealMessage: the log's first message is seq 0, not a
// sentinel - the stamping names a live row with it, the /memories row reads
// it in-window and offers its preview, zoom opens it, nothing older is
// offered past it.
func TestSeqZeroIsARealMessage(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	// the first turn: the live row lands before its log write
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "first words"})
	if _, err := h.DB.AppendLog(store.KindUser, "first words", "", "", ""); err != nil {
		t.Fatal(err)
	}
	h.stampLogSeqs()
	if tr := h.Session().Transcript; tr[0].Seq != 0 {
		t.Fatalf("the first turn's row carries seq %d, want the log's 0", tr[0].Seq)
	}
	// the stamping cursor sits past it
	h.tMu.RLock()
	from := h.stampFrom
	h.tMu.RUnlock()
	if from != 1 {
		t.Fatalf("the stamping resumes from %d, want 1", from)
	}

	// the browser's line reads as on the screen, its window at the log's 0
	o := h.memoriesOverlay()
	if len(o.Items) != 1 || o.Items[0].Desc != "" {
		t.Fatalf("the view lists %+v, want one line the window reaches", o.Items)
	}
	if o.Items[0].Run != "memories view 0 0" || o.Items[0].Verb != VerbOpen {
		t.Fatalf("the view's leaf = %+v, want the preview opened", o.Items[0])
	}
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	list := ui.overlays()[len(ui.overlays())-1]
	if list.Items[0].Anchor != 1 {
		t.Fatalf("the picker's first row = %+v, want the row anchored at 1", list.Items[0])
	}
	if _, ok := h.messagesOlder(); ok {
		t.Error("a page loaded before the log's first message")
	}

	// zoom opens message 0 whole
	if res := zoomCall(t, h, `{"id":0}`); !strings.Contains(res.Content, "first words") {
		t.Fatalf("zoom(0) = %q", res.Content)
	}
}

// TestStampLogSeqs: a real turn stamps the rows it landed in the transcript
// with the log seqs they were written at, so a browser row can jump the chat
// to them.
func TestStampLogSeqs(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	replyStub(t, h, "pong")
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if err := h.AgentSend("again"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	tr := h.Session().Transcript
	count, err := h.DB.LogCount()
	if err != nil {
		t.Fatal(err)
	}
	if count < 4 { // two prompts, two replies
		t.Fatalf("the turns wrote %d log rows", count)
	}
	logged := map[int64]bool{}
	for seq := range count {
		logged[seq] = true
	}
	stamped := 0
	for _, m := range tr {
		if m.Seq == NoSeq {
			if stampable(m) {
				t.Errorf("row %q stayed unstamped", clipRunes(m.Text, 30))
			}
			continue
		}
		stamped++
		if !logged[m.Seq] {
			t.Errorf("row %q carries seq %d, which the log holds no message for", clipRunes(m.Text, 30), m.Seq)
		}
	}
	if stamped < 4 { // every message named its row, the log's own 0 included
		t.Fatalf("%d rows stamped", stamped)
	}
	// the newest message's line reads as on the screen
	if lo := h.chatWindowLo(); lo != 0 {
		t.Errorf("the window starts at %d, want the log's 0", lo)
	}
}

// TestStampLogSeqsMismatch: a transcript row whose log write failed pairs
// with nothing - the row draws unstamped and the log's rows pair with the
// rows that did land.
func TestStampLogSeqsMismatch(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	if _, err := h.DB.AppendLog(store.KindUser, "first", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.AppendLog(store.KindUser, "later", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// the log holds two rows; the window's first row's write failed. Both
	// rows drew live, so neither names a message yet.
	h.tMu.Lock()
	h.session = &Session{Transcript: []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "lost", Seq: NoSeq},
		{Role: fantasy.MessageRoleUser, Text: "later", Seq: NoSeq},
	}}
	h.tMu.Unlock()
	h.stampLogSeqs()
	tr := h.Session().Transcript
	if tr[0].Seq != -1 {
		t.Errorf("the row with no log message carries %d, want the -1 mark", tr[0].Seq)
	}
	if tr[1].Seq != 1 {
		t.Errorf("later stamped %d, want 1", tr[1].Seq)
	}
}
