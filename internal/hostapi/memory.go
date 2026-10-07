// memory.go is the memory host API (DESIGN §6.7, §7, §10.4): the
// zemi.memory namespace, the Go-owned /memory command, and the
// proposal queue behind the autonomy gate. Proposals
// persist in their scope's store until the user accepts or rejects them.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// memoryAutoApplyCap bounds model-proposed memories stored without review
// per session (DESIGN §10.4).
const memoryAutoApplyCap = 20

// The memory tool's refusal of the reserved file, which the ask reason from
// permmode.go's protectedWhy has to say the same way.
const memOwnedReason = "the memory tool owns this file"

// memoryDB returns the store owning a scope: user scope in the global DB,
// project scope in the project DB (DESIGN §7).
func (h *Host) memoryDB(scope store.Scope) *store.DB {
	if scope == store.ScopeUser {
		return h.GlobalDB
	}
	return h.DB
}

// partition is the brain partition owning a scope (DESIGN §7): user scope in the
// user partition, project scope in the private one.
func (h *Host) partition(scope store.Scope) brain.Partition {
	user, private := h.brainParts()
	if scope == store.ScopeUser {
		return user
	}
	return private
}

// storeMemory runs the §10.4 intake policy over what session sid wrote;
// user config before the first send is its own provenance.
func (h *Host) storeMemory(sid string, scope store.Scope, origin store.Origin, text, quote string) (*learn.Result, error) {
	if sid == "" {
		if origin != store.OriginUser {
			return nil, learn.ErrNoProvenance
		}
		sid = "user"
	}
	return learn.Intake(scope, origin, text, quote, sid)
}

// queueProposal persists a policy-clean memory for review in its scope's
// store, reporting whether it added one. The id is assigned here so review
// decisions travel by id.
func (h *Host) queueProposal(res *learn.Result) bool {
	if dup, _ := h.duplicateMemory(res.Memory, res.Supersedes); dup != nil {
		h.Log.Debug("duplicate proposal dropped", "origin", "memory")
		return false
	}
	if err := h.addProposal(memoryProposal{Memory: res.Memory, Supersedes: res.Supersedes}); err != nil {
		h.Log.Warn("proposal dropped", "origin", "memory", "error", err)
		return false
	}
	return true
}

// addProposal persists a proposal in its scope's store as it is.
func (h *Host) addProposal(p memoryProposal) error {
	if p.ID == "" {
		p.ID = store.ShortID()
	}
	db := h.memoryDB(p.Scope)
	if db == nil {
		return errNoMemoryStore
	}
	payload, err := json.Marshal(p, json.Deterministic(true))
	if err == nil {
		err = db.AddProposal(&store.Proposal{ID: p.ID, Payload: string(payload)})
	}
	if err == nil {
		h.refreshPending()
	}
	return err
}

// queueForget proposes deleting a stored memory, for the reflector to retire
// one that stopped being true. Deleting loses data, so it always waits for
// review, whatever the autonomy.
func (h *Host) queueForget(sid, id string) (added bool, err error) {
	_, old, err := h.locateMemory(id)
	if err != nil {
		return false, err
	}
	for _, p := range h.memoryProposals() {
		if p.Forget && p.Supersedes == old.ID {
			return false, nil
		}
	}
	p := memoryProposal{Memory: *old, Supersedes: old.ID, Forget: true}
	p.ID, p.ProvenanceSessionID = "", sid
	err = h.addProposal(p)
	return err == nil, err
}

// applyProposal stores a proposal under auto autonomy and spends one unit
// of the per-session auto-apply budget; the ids the budget cut name
// themselves in the answer (DESIGN §7).
func (h *Host) applyProposal(res *learn.Result) ([]string, error) {
	cut, err := h.storeProposal(res)
	if err != nil {
		return nil, err
	}
	// a detached reflector stores a closed session's memories; the live
	// session's budget is not theirs to spend
	h.spendAutoBudget(res.Memory.ProvenanceSessionID)
	h.Notify(slog.LevelInfo, "Memory saved", res.Memory.Text+cutNote(cut)+redactNote(res.Scrubbed))
	return cut, nil
}

// cutNote names the learned lines the budget archived (DESIGN §7): their text,
// quote and provenance live on in the ledger and /memory lists them.
func cutNote(cut []string) string {
	if len(cut) == 0 {
		return ""
	}
	return fmt.Sprintf("; %d oldest learned %s cut to the ledger to fit: %s",
		len(cut), Plural(len(cut), "line", "lines"), strings.Join(cut, ", "))
}

// memoryOutcome is what became of a proposed memory: stored under ID,
// queued for review (Queued says why, "" for the autonomy setting alone),
// refused as a near-duplicate of a live or pending one, or dropped because
// its quote is not in the session. Cut names the learned lines the budget
// archived to make room.
type memoryOutcome struct {
	ID        string
	Queued    string
	Duplicate *store.Memory
	DupQueued bool // Duplicate is a pending proposal, which replaces cannot name
	Dropped   bool
	Cut       []string
}

// String is the memory tool's answer: the id and what to do next.
func (o memoryOutcome) String() string {
	switch {
	case o.ID != "":
		return "saved as " + o.ID + cutNote(o.Cut) + "; shows from the next session"
	case o.Duplicate != nil && o.DupQueued:
		return fmt.Sprintf("already queued for the user's review as %s: %q", o.Duplicate.ID, o.Duplicate.Text)
	case o.Duplicate != nil:
		return fmt.Sprintf("near-duplicate of %[1]s (%s): %q; to keep both facts, pass replaces=%[1]s with one line that says both", o.Duplicate.ID, o.Duplicate.Scope, o.Duplicate.Text)
	case o.Dropped:
		return "dropped: quote not found in this session's messages"
	case o.Queued != "":
		return "queued for the user's review (" + o.Queued + ")"
	}
	return "queued for the user's review"
}

func redactNote(scrubbed bool) string {
	if scrubbed {
		return " (secrets redacted)"
	}
	return ""
}

var (
	errDuplicateMemory = errors.New("already remembered")
	errNoMemoryStore   = errors.New("memory store unavailable")
	// usage errors: a row another window already took is nothing to act on
	errNoMemory        = Usagef("no such memory")
	errUnknownProposal = Usagef("no such proposal")
)

// duplicateMemory finds a live memory in either scope, or a pending
// proposal (queued), that says what m says; m itself and the row it
// replaces are no candidates. The user's own text is refused only when it
// repeats one word for word: learn.Similar would refuse a distinct fact.
// NOTE: scans both scopes; fine for the dozens of rows a budget holds.
func (h *Host) duplicateMemory(m store.Memory, replaces string) (dup *store.Memory, queued bool) {
	same := learn.Similar
	if m.Origin == store.OriginUser {
		same = strings.EqualFold
	}
	live, _ := h.memoryList(false)
	for _, o := range live {
		if o.ID != m.ID && o.ID != replaces && same(o.Text, m.Text) {
			return o, false
		}
	}
	for _, o := range h.memoryProposals() {
		if !o.Forget && o.ID != m.ID && same(o.Text, m.Text) {
			return &o.Memory, true
		}
	}
	return nil, false
}

// storeProposal applies a policy-clean memory to its partition's
// memories.md, its add entry in the ledger first (DESIGN §7); the ids the budget
// cut come back for the tool result to name.
func (h *Host) storeProposal(res *learn.Result) (cut []string, err error) {
	m := &res.Memory
	if dup, _ := h.duplicateMemory(*m, res.Supersedes); dup != nil {
		return nil, fmt.Errorf("%w: %q", errDuplicateMemory, dup.Text)
	}
	replaces := res.Supersedes
	var oldP brain.Partition
	if replaces != "" {
		p, _, err := h.locateMemory(replaces)
		if err != nil {
			return nil, err
		}
		if p.Kind != h.partition(m.Scope).Kind {
			// the replaced memory lives in the other partition (it moved
			// there, or the proposal predates the swap): the new line lands
			// here without replaces and its supersede entry goes there (DESIGN §7),
			// after the add so a budget refusal leaves the old line alone
			oldP, replaces = p, ""
		}
	}
	l, cut, err := brain.Apply(h.partition(m.Scope), *m, replaces, h.now())
	if err != nil {
		return nil, err
	}
	if !oldP.Zero() {
		if err := brain.Supersede(oldP, m.ProvenanceSessionID, res.Supersedes, l.ID, h.now()); err != nil {
			return nil, err
		}
	}
	m.ID = l.ID
	h.EmitAsync("memory.written", map[string]any{
		"id": m.ID, "scope": m.Scope, "origin": m.Origin, "text": m.Text,
	})
	return cut, nil
}

// Remember stores a user-typed /memory <note> text (origin user, project
// scope): the keystroke bypasses the autonomy gate (DESIGN §10.4), and an
// overflow that would cut a line the user saved is refused, since the user
// is there to make room.
func (h *Host) Remember(text string) error {
	if strings.TrimSpace(text) == "" {
		return Usagef("usage: /memory <note>")
	}
	res, err := h.storeMemory(h.AgentSessionID(), store.ScopeProject, store.OriginUser, text, "")
	if err != nil {
		return err
	}
	cut, err := h.storeProposal(res)
	if err != nil {
		if uc, ok := errors.AsType[brain.UserCutError](err); ok {
			return userCutFull(res.Memory.Scope, uc)
		}
		return err
	}
	h.Notify(slog.LevelInfo, "Memory saved for this project",
		"press s on it in /memory to keep it for every project"+cutNote(cut)+redactNote(res.Scrubbed))
	return nil
}

// memoryCommand dispatches /memory: no argument opens the list, and
// forget <id> asks there first (AskFirst). The list's actions on a row end
// in --yes, a form no note takes, so any other text is a note to keep.
func (h *Host) memoryCommand(args string) error {
	f := strings.Fields(args)
	switch {
	case len(f) == 0:
		return h.openMemory("")
	case len(f) == 2 && f[0] == "forget":
		_, m, err := h.locateMemory(f[1])
		if err != nil {
			return err
		}
		return AskFirst("memory forget "+m.ID+" --yes", "", h.openMemory)
	case len(f) == 2 && f[0] == "undo" && f[1] == "--yes":
		return h.undoMemory()
	case len(f) != 3 || f[2] != "--yes":
		if f[0] == "forget" {
			return Usagef("usage: /memory forget <id> [--yes]")
		}
		return h.Remember(args)
	}
	id := f[1]
	switch f[0] {
	case "forget":
		p, m, err := h.locateMemory(id)
		if err != nil {
			return err
		}
		if err := brain.Forget(p, cmp.Or(h.AgentSessionID(), "user"), m.ID, h.now()); err != nil {
			return err
		}
		h.pushUndo(func() error { return h.restoreMemory(p, m) })
		h.Notify(slog.LevelInfo, "Memory forgotten", m.Text)
		return nil
	case "accept":
		undo := h.undoKeep(id)
		if _, err := h.acceptProposal(id); err != nil {
			return err
		}
		h.pushUndo(undo)
		return nil
	case "reject":
		p := h.takeProposal(id)
		if p == nil {
			return fmt.Errorf("%w %q", errUnknownProposal, id)
		}
		h.pushUndo(func() error { return h.addProposal(*p) })
		return nil
	case "move":
		return h.moveRow(id)
	case "edit":
		ui, err := h.Screen()
		if err != nil {
			return err
		}
		h.GoSafe("edit memory", func() { h.editMemory(ui, id) }) // the editor blocks
		return nil
	}
	return h.Remember(args)
}

// openMemory opens /memory with nothing to undo; confirm arms the action
// that runs it (Overlay.Confirm).
func (h *Host) openMemory(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	h.memUndo.Lock()
	h.memUndo.steps = nil
	h.memUndo.Unlock()
	o := h.memoryOverlay()
	o.Confirm = confirm
	ui.OpenOverlay(o)
	return nil
}

// memoryOverlay is /memory, a toggle list: an action applies at once and
// the list rebuilds, and u undoes it while there is one to undo.
func (h *Host) memoryOverlay() Overlay {
	o := Overlay{
		ID: "memory", Title: "Memory", Origin: lua.OriginBuiltin, Items: h.memoryItems(),
		Reload: func() ([]any, [][]Segment) { return h.memoryItems(), nil },
		Empty:  "Nothing remembered yet. Type /memory <note> to save one.",
	}
	h.memUndo.Lock()
	if len(h.memUndo.steps) > 0 {
		o.Keys = []OverlayKey{{Key: "u", Desc: "undo", Run: "memory undo --yes"}}
	}
	h.memUndo.Unlock()
	return o
}

// pushUndo records the reverse of a /memory action and rebuilds the whole
// list, since Reload leaves its keys as they were.
func (h *Host) pushUndo(step func() error) {
	h.memUndo.Lock()
	h.memUndo.steps = append(h.memUndo.steps, step)
	h.memUndo.Unlock()
	h.refreshMemory()
}

// undoMemory reverses the newest /memory action not undone yet.
func (h *Host) undoMemory() error {
	u := &h.memUndo
	u.Lock()
	n := len(u.steps)
	if n == 0 {
		u.Unlock()
		return Usagef("nothing to undo")
	}
	step := u.steps[n-1]
	u.steps = u.steps[:n-1]
	u.Unlock()
	defer h.refreshMemory()
	return step()
}

func (h *Host) refreshMemory() {
	if ui, err := h.Screen(); err == nil {
		ui.RefreshOverlay("memory", h.memoryOverlay)
	}
}

// undoKeep returns the reverse of keeping suggestion id, taken before it is
// kept: the stored line goes, the memory it replaced or forgot comes back
// as it was, age included, and the suggestion is queued again.
func (h *Host) undoKeep(id string) func() error {
	props := h.memoryProposals()
	i := slices.IndexFunc(props, func(p memoryProposal) bool { return p.ID == id })
	if i < 0 {
		return nil
	}
	p := props[i]
	oldP, old, _ := h.locateMemory(p.Supersedes)
	if old != nil && old.SupersededBy != "" && old.SupersededBy != p.ID {
		old = nil // a later replacement superseded it again; nothing to bring back
	}
	return func() error {
		if !p.Forget {
			if pP, m, err := h.locateMemory(p.ID); err == nil {
				if err := brain.Forget(pP, cmp.Or(h.AgentSessionID(), "user"), m.ID, h.now()); err != nil && !errors.Is(err, brain.ErrNoMemory) {
					return err
				}
			}
		}
		if old != nil {
			if err := h.restoreMemory(oldP, old); err != nil {
				return err
			}
		}
		return h.addProposal(p)
	}
}

// restoreMemory puts m's line back whole, its age included: the reverse of
// a removal. No ledger entry - the trail's forget explains the absence it
// had, and the restored hash matches the entry that wrote the line (DESIGN §10.4).
func (h *Host) restoreMemory(p brain.Partition, m *store.Memory) error {
	return brain.Restore(p, "", []brain.Line{{
		ID: m.ID, Origin: m.Origin, Date: time.UnixMilli(m.UpdatedAt).UTC(), Text: m.Text,
	}}, nil, h.now())
}

// moveRow moves suggestion or memory id to the other scope. A suggestion
// the other scope already holds stays where it was.
func (h *Host) moveRow(id string) error {
	props := h.memoryProposals()
	if i := slices.IndexFunc(props, func(p memoryProposal) bool { return p.ID == id }); i >= 0 {
		if props[i].Forget {
			return Usagef("a forget suggestion stays with its memory")
		}
		p := h.takeProposal(id)
		if p == nil {
			return fmt.Errorf("%w %q", errUnknownProposal, id)
		}
		to := otherScope(p.Scope)
		moved := p.Memory
		moved.Scope = to
		if !h.queueProposal(&learn.Result{Memory: moved, Supersedes: p.Supersedes}) {
			if err := h.addProposal(*p); err != nil {
				return err
			}
			return Usagef("%s memory already has that text", scopeWords[to])
		}
		h.pushUndo(func() error {
			h.takeProposal(id)
			return h.addProposal(*p)
		})
		return nil
	}
	db, m, err := h.locateMemory(id)
	if err != nil {
		return err
	}
	to := otherScope(m.Scope)
	if err := h.moveMemory(m.ID, m.Scope, to); err != nil {
		return err
	}
	h.pushUndo(func() error {
		// a move that merged into a copy left nothing under the id
		if err := brain.Forget(h.partition(to), "", m.ID, h.now()); err != nil && !errors.Is(err, brain.ErrNoMemory) {
			return err
		}
		return h.restoreMemory(db, m)
	})
	return nil
}

func otherScope(s store.Scope) store.Scope {
	if s == store.ScopeUser {
		return store.ScopeProject
	}
	return store.ScopeUser
}

// scopeWords name a scope in /memory: its section, a suggestion's desc and
// the move action.
var scopeWords = map[store.Scope]string{store.ScopeProject: "this project", store.ScopeUser: "everywhere"}

// editMemory opens a memory or a pending suggestion in the user's editor
// and saves what comes back through rewriteMemory; a failure is a toast.
func (h *Host) editMemory(ui UIHooks, id string) {
	scrubbed, err := func() (bool, error) {
		text, err := h.editableMemory(id)
		if err != nil {
			return false, err
		}
		dir, err := h.TempDir()
		if err != nil {
			return false, err
		}
		f, err := os.CreateTemp(dir, "memory-*.md")
		if err != nil {
			return false, err
		}
		defer os.Remove(f.Name())
		_, err = f.WriteString(text + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = ui.EditFile(context.Background(), EditRequest{Path: f.Name(), Title: "the memory"})
		}
		if err != nil {
			return false, err
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			return false, err
		}
		return h.rewriteMemory(id, string(b))
	}()
	switch {
	case err != nil:
		h.Notify(slog.LevelError, "Could not edit the memory", err.Error())
	case scrubbed:
		h.Notify(slog.LevelInfo, "Memory updated", "secrets redacted")
	}
}

// editableMemory is the raw text of a pending suggestion or stored memory.
func (h *Host) editableMemory(id string) (string, error) {
	for _, p := range h.memoryProposals() {
		if p.ID == id {
			return p.Text, nil
		}
	}
	_, m, err := h.locateMemory(id)
	if err != nil {
		return "", err
	}
	return m.Text, nil
}

// rewriteMemory saves the user's edit of a memory: intake runs again as
// user origin with the same scope and quote, and the memory keeps its
// id. An edited suggestion is stored as the user's own. Empty or unchanged
// text changes nothing, and so does a refusal; only an edit that grows past
// a full scope is refused, so a scope already over can shrink. A saved edit
// is on the /memory undo stack like the list's actions.
func (h *Host) rewriteMemory(id, text string) (scrubbed bool, err error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	for _, p := range h.memoryProposals() {
		if p.ID != id || p.Forget {
			continue
		}
		res, err := h.storeMemory(h.AgentSessionID(), p.Scope, store.OriginUser, text, p.Quote)
		if err != nil || res.Memory.Text == p.Text {
			return false, err
		}
		res.Memory.ID, res.Supersedes = p.ID, p.Supersedes
		if h.scopeOver(res) && len(res.Memory.Text) > len(p.Text) {
			return false, errScopeFull(p.Scope)
		}
		undo := h.undoKeep(id)
		if h.takeProposal(id) == nil {
			return false, fmt.Errorf("%w %q", errUnknownProposal, id)
		}
		if _, err := h.storeProposal(res); err != nil {
			if qerr := h.addProposal(p); qerr != nil {
				h.Log.Warn("proposal dropped", "origin", "memory", "error", qerr)
			}
			return false, err
		}
		h.pushUndo(undo)
		return res.Scrubbed, nil
	}
	db, m, err := h.locateMemory(id)
	if err != nil {
		return false, err
	}
	res, err := h.storeMemory(h.AgentSessionID(), m.Scope, store.OriginUser, text, m.Quote)
	if err != nil || res.Memory.Text == m.Text {
		return false, err
	}
	res.Memory.ID = m.ID
	// a shorter edit is always taken, so a scope already over can shrink
	if h.scopeOver(res) && len(res.Memory.Text) > len(m.Text) {
		return false, errScopeFull(m.Scope)
	}
	if dup, _ := h.duplicateMemory(res.Memory, ""); dup != nil {
		return false, fmt.Errorf("%w: %q", errDuplicateMemory, dup.Text)
	}
	if _, err := brain.Edit(db, h.AgentSessionID(), m.ID, res.Memory.Text, store.OriginUser, h.now()); err != nil {
		return false, err
	}
	h.pushUndo(func() error { // the old line back whole, its age included
		return brain.Restore(db, "", []brain.Line{{
			ID: m.ID, Origin: m.Origin, Date: time.UnixMilli(m.UpdatedAt).UTC(), Text: m.Text,
		}}, []string{m.ID}, h.now())
	})
	return res.Scrubbed, nil
}

// takeProposal removes one pending memory proposal by id and returns it,
// or nil when no scope holds it.
func (h *Host) takeProposal(id string) *memoryProposal {
	for _, db := range []*store.DB{h.DB, h.GlobalDB} {
		if db == nil {
			continue
		}
		p, err := db.TakeProposal(id)
		if err != nil {
			continue
		}
		h.refreshPending()
		var m memoryProposal
		if json.Unmarshal([]byte(p.Payload), &m) != nil {
			return nil
		}
		return &m
	}
	return nil
}

// moveMemory moves a memory to the other scope: both partitions lock in
// the fixed user-then-private order, the move-out and move-in entries go
// into both ledgers (DESIGN §7), and a failed move, or a full destination, leaves
// it where it was.
func (h *Host) moveMemory(id string, from, to store.Scope) error {
	mergedInto, err := brain.Move(h.partition(from), h.partition(to), cmp.Or(h.AgentSessionID(), "user"), id, h.now())
	if err != nil || mergedInto == "" {
		return err
	}
	// pending forgets and replacements follow the text to the copy it merged into
	for _, p := range h.memoryProposals() {
		if p.Supersedes == id && h.takeProposal(p.ID) != nil {
			p.Supersedes = mergedInto
			if err := h.addProposal(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// acceptProposal stores a pending proposal, or deletes the memory a forget
// proposal names (one already gone counts as done, one rewritten since drops
// the proposal). A full scope or a store failure other than a duplicate puts
// it back in the queue, so the decision can be made again.
func (h *Host) acceptProposal(id string) (*memoryProposal, error) {
	p := h.takeProposal(id)
	if p == nil {
		return nil, fmt.Errorf("%w %q", errUnknownProposal, id)
	}
	var err error
	if p.Forget {
		// looked up in both scopes: the memory may have moved since
		db, m, lerr := h.locateMemory(p.Supersedes)
		switch {
		case errors.Is(lerr, errNoMemory):
		case lerr != nil:
			err = lerr
		case m.Text != p.Text:
			// the review showed the old text: forgetting the new one needs a new proposal
			return nil, errors.New("the memory changed since zemi proposed forgetting it")
		default:
			err = brain.Forget(db, cmp.Or(h.AgentSessionID(), "user"), p.Supersedes, h.now())
		}
	} else if p.Path != "" {
		err = h.acceptFileProposal(p)
	} else {
		// the budget cuts learned lines to make room; a cut that
		// would take a user line puts the proposal back
		_, err = h.storeProposal(&learn.Result{Memory: p.Memory, Supersedes: p.Supersedes})
	}
	if err != nil {
		if uc, ok := errors.AsType[brain.UserCutError](err); ok {
			err = userCutFull(p.Memory.Scope, uc)
		}
		if !errors.Is(err, errDuplicateMemory) {
			if qerr := h.addProposal(*p); qerr != nil {
				h.Log.Warn("proposal dropped", "origin", "memory", "error", qerr)
			}
		}
		return nil, err
	}
	return p, nil
}

// memoryProposal is a queued proposal's payload. Forget proposes deleting
// the memory Supersedes names; the embedded memory is a copy of it. A
// file-mode proposal writes the brain file Path with Content instead (DESIGN §10.4).
type memoryProposal struct {
	store.Memory
	Supersedes string `json:",omitempty"`
	Forget     bool   `json:",omitzero"`
	Path       string `json:",omitempty"`
	Content    string `json:",omitempty"`
}

// memoryProposals lists pending memory proposals from both scopes.
func (h *Host) memoryProposals() []memoryProposal {
	var out []memoryProposal
	for _, db := range []*store.DB{h.DB, h.GlobalDB} {
		if db == nil {
			continue
		}
		rows, err := db.Proposals()
		if err != nil {
			h.Log.Warn("proposal list failed", "origin", "memory", "error", err)
			continue
		}
		for _, p := range rows {
			var m memoryProposal
			if json.Unmarshal([]byte(p.Payload), &m) == nil {
				out = append(out, m)
			}
		}
	}
	return out
}

// refreshPending recounts what awaits review for the statusline badge.
func (h *Host) refreshPending() {
	h.pendingMemory.Store(int32(len(h.memoryProposals())))
	h.refresh()
}

// memoryItems lists the suggestions, then the memories of this project and
// of everywhere, newest first; each section title counts its rows, and a
// scope's names its share of brain.ScopeBudget. A row is one line: the
// preview and the detail hold the whole text. The trail of cut and
// superseded lines is one collapsed row at the end (DESIGN §10.4).
func (h *Host) memoryItems() []any {
	props := h.memoryProposals()
	live, _ := h.memoryList(false)
	count, used := map[store.Scope]int{}, map[store.Scope]int{}
	for _, m := range live {
		count[m.Scope]++
		used[m.Scope] += brain.LineSize(m.Text)
	}
	items := make([]any, 0, len(props)+len(live)+1)
	for _, p := range props {
		if p.Path != "" {
			item := map[string]any{
				"id": p.ID, "text": clipRunes("write "+filepath.Base(p.Path), 40),
				"desc": fmt.Sprintf("Suggested by zemi (%d)", len(props)), "section": scopeWords[p.Scope],
				"preview": p.Content, "copy": p.Content,
				"fields": fieldList("path", p.Path, "bytes", strconv.Itoa(len(p.Content))),
			}
			item["actions"] = []any{
				map[string]any{"text": "Accept", "hotkey": "a", "run": "memory accept " + p.ID + " --yes"},
				map[string]any{"text": "Drop", "hotkey": "d", "run": "memory reject " + p.ID + " --yes"},
			}
			items = append(items, item)
			continue
		}
		label, replaces := p.Text, ""
		if p.Forget {
			label = "forget: " + label
		} else if p.Supersedes != "" {
			// the list shows no ids, so the row names the memory by its text
			replaces = "a memory since deleted"
			if _, old, err := h.locateMemory(p.Supersedes); err == nil {
				replaces = old.Text
				if old.Scope != p.Scope {
					replaces += " (" + scopeWords[old.Scope] + ")"
				}
			}
		}
		item := memoryItem(p.Memory, label, fmt.Sprintf("Suggested by zemi (%d)", len(props)), scopeWords[p.Scope], replaces)
		// accept or drop the proposal, whatever it suggests: a forget one says
		// so in its label, so no letter carries two meanings
		item["actions"] = append([]any{
			map[string]any{"text": "Accept", "hotkey": "a", "run": "memory accept " + p.ID + " --yes"},
			map[string]any{"text": "Drop", "hotkey": "d", "run": "memory reject " + p.ID + " --yes"},
		}, moveAndEdit(p.Memory, p.Forget)...)
		items = append(items, item)
	}
	for _, m := range live {
		desc := map[store.Origin]string{store.OriginUser: "by you", store.OriginLearned: "learned"}[m.Origin] + " · " + ago(m.UpdatedAt)
		section := fmt.Sprintf("%s (%d) · %.1f/%d KB", sourceSection(scopeWords[m.Scope]), count[m.Scope], float64(used[m.Scope])/1024, brain.ScopeBudget/1024)
		item := memoryItem(*m, m.Text, section, desc, "")
		item["actions"] = append([]any{map[string]any{"text": "Forget", "hotkey": "f", "run": "memory forget " + m.ID + " --yes"}}, moveAndEdit(*m, false)...)
		items = append(items, item)
	}
	if trail := h.memoryTrail(live); len(trail) > 0 {
		var b strings.Builder
		for i, t := range trail {
			if i >= 40 {
				fmt.Fprintf(&b, "... %d more", len(trail)-40)
				break
			}
			b.WriteString(t.String() + "\n")
		}
		items = append(items, map[string]any{
			"text":    fmt.Sprintf("%d removed from memories.md", len(trail)),
			"desc":    "cut by the budget or superseded; the ledger keeps their text",
			"section": "Trail", "preview": strings.TrimRight(b.String(), "\n"),
			"fields": fieldList("count", strconv.Itoa(len(trail))),
		})
	}
	return items
}

// memoryTrail is the ids the ledgers show superseded or budget-cut, plus
// the frozen table's superseded rows, none of them live (DESIGN §7);
// each carries its origin and last-write date.
func (h *Host) memoryTrail(live []*store.Memory) []brain.Line {
	liveIDs := map[string]bool{}
	for _, m := range live {
		liveIDs[m.ID] = true
	}
	var out []brain.Line
	seen := map[string]bool{}
	for _, scope := range store.Scopes {
		p := h.partition(scope)
		if p.Zero() {
			continue
		}
		lg, err := p.OpenLedger()
		if err != nil {
			continue
		}
		latest, err := lg.Latest()
		_ = lg.Close()
		if err != nil {
			continue
		}
		for _, op := range latest {
			if (op.Op != brain.OpSupersede && op.Op != brain.OpArchive) || liveIDs[op.ID] || seen[op.ID] {
				continue
			}
			seen[op.ID] = true
			// an id the ledger alone held has no text to show
			if op.Text == "" {
				continue
			}
			out = append(out, brain.Line{ID: op.ID, Origin: op.Origin, Date: dayOf(op.TS), Text: op.Text})
		}
	}
	frozen, _ := h.memoryList(true)
	for _, m := range frozen {
		if m.SupersededBy != "" && !liveIDs[m.ID] && !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, brain.Line{ID: m.ID, Origin: m.Origin, Date: dayOf(m.UpdatedAt), Text: m.Text})
		}
	}
	slices.SortFunc(out, func(a, b brain.Line) int { return b.Date.Compare(a.Date) })
	return out
}

// dayOf is the date a ts carries in a line, the line format's granularity.
func dayOf(ts int64) time.Time {
	return time.UnixMilli(ts).UTC().Truncate(24 * time.Hour)
}

// memoryItem is m's row under section: label cut to one line, its whole
// text and quote in the preview, and its facts in the detail.
func memoryItem(m store.Memory, label, section, desc, replaces string) map[string]any {
	preview := m.Text
	if m.Quote != "" {
		preview += "\n\"" + m.Quote + "\""
	}
	updated := ""
	if m.UpdatedAt != 0 {
		updated = ago(m.UpdatedAt)
	}
	return map[string]any{
		"id": m.ID, "text": clipRunes(label, 40), "desc": desc, "section": section, "preview": preview, "copy": m.Text,
		"fields": fieldList("text", m.Text, "quote", m.Quote, "replaces", replaces, "scope", scopeWords[m.Scope],
			"origin", map[store.Origin]string{store.OriginUser: "saved by you", store.OriginLearned: "learned"}[m.Origin], "updated", updated),
	}
}

// moveAndEdit are the actions of a row that can move and be edited; a
// forget suggestion does neither.
func moveAndEdit(m store.Memory, forget bool) []any {
	if forget {
		return nil
	}
	return []any{
		map[string]any{"text": map[store.Scope]string{store.ScopeProject: "Use everywhere", store.ScopeUser: "Only this project"}[m.Scope], "hotkey": "s", "run": "memory move " + m.ID + " --yes"},
		map[string]any{"text": openInEditorLabel, "hotkey": "o", "run": "memory edit " + m.ID + " --yes"},
	}
}

// locateMemory resolves an id over the partitions' files and ledgers (DESIGN §7),
// the frozen table last: the pre-migration rows keep resolving there,
// quote and provenance intact, for /memory and the replaces it names.
func (h *Host) locateMemory(id string) (brain.Partition, *store.Memory, error) {
	user, private := h.brainParts()
	for _, p := range []brain.Partition{private, user} {
		if p.Zero() {
			continue
		}
		m, ok, err := h.partitionMemory(p, id)
		if err != nil {
			return brain.Partition{}, nil, err
		}
		if ok {
			return p, m, nil
		}
	}
	for _, db := range []*store.DB{h.DB, h.GlobalDB} {
		if db == nil {
			continue
		}
		m, err := db.GetMemory(id)
		if err == nil {
			return h.partition(m.Scope), m, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return brain.Partition{}, nil, err
		}
	}
	return brain.Partition{}, nil, fmt.Errorf("%w %q", errNoMemory, id)
}

// partitionMemory is id's memory as one partition holds it: the live line,
// else the id's latest trail, quote and stamps from the ledger. A trail
// ending in a move-out is not a hit: the id moved to the other partition.
func (h *Host) partitionMemory(p brain.Partition, id string) (*store.Memory, bool, error) {
	lg, err := p.OpenLedger()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = lg.Close() }()
	line, _, err := brain.Locate(p, lg, id)
	if err != nil {
		if errors.Is(err, brain.ErrNoMemory) {
			return nil, false, nil
		}
		return nil, false, err
	}
	m := &store.Memory{
		ID: id, Scope: p.Kind.Scope(), Text: line.Text, Origin: line.Origin,
		CreatedAt: line.Date.UnixMilli(), UpdatedAt: line.Date.UnixMilli(),
	}
	if trail, err := lg.ByID(id); err == nil {
		for _, op := range trail {
			if op.Quote != "" {
				m.Quote = op.Quote
			}
			if op.SupersededBy != "" {
				m.SupersededBy = op.SupersededBy
			}
			if op.CreatedAt != 0 {
				m.CreatedAt, m.UpdatedAt = op.CreatedAt, op.UpdatedAt
			}
		}
		if len(trail) > 0 && trail[len(trail)-1].Op == brain.OpMoveOut {
			return nil, false, nil
		}
	}
	return m, true, nil
}

// memoryList merges both scopes, project first, each newest first by last
// write (DESIGN §7): the partitions' memories.md lines, quote and stamps from the
// ledger trail. includeSuperseded adds the frozen table's rows the files do
// not hold - the pre-migration trail /memory's collapsed group shows, and
// the ids a stored context row or a pending proposal still names. A failing
// partition is logged and skipped; its error comes back with the rows.
func (h *Host) memoryList(includeSuperseded bool) ([]*store.Memory, error) {
	var out []*store.Memory
	var errs []error
	live := map[string]bool{}
	for _, scope := range []store.Scope{store.ScopeProject, store.ScopeUser} {
		p := h.partition(scope)
		if p.Zero() {
			continue
		}
		lines, err := brain.Lines(p)
		if err != nil {
			h.Log.Warn("list failed", "origin", "memory", "scope", scope, "error", err)
			errs = append(errs, err)
			continue
		}
		var stamps map[string]brain.Stamp
		if lg, err := p.OpenLedger(); err == nil {
			stamps, _ = lg.Stamps()
			_ = lg.Close()
		}
		for _, l := range lines {
			live[l.ID] = true
			// the line's date is the last write the store holds (DESIGN §7); the
			// ledger refines quote and provenance beside it
			m := &store.Memory{ID: l.ID, Scope: scope, Text: l.Text, Origin: l.Origin, UpdatedAt: l.Date.UnixMilli()}
			if s, ok := stamps[l.ID]; ok {
				m.Quote, m.CreatedAt, m.ProvenanceSessionID = s.Quote, s.First, s.Session
			}
			out = append(out, m)
		}
	}
	if includeSuperseded {
		for _, db := range []*store.DB{h.DB, h.GlobalDB} {
			if db == nil {
				continue
			}
			for _, scope := range store.Scopes {
				rows, err := db.ListMemory(scope, true)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				for _, m := range rows {
					if !live[m.ID] {
						out = append(out, m)
					}
				}
			}
		}
	}
	return out, errors.Join(errs...)
}

// memoryCommands is /memory.
func (h *Host) memoryCommands() []*Command {
	return []*Command{{
		Name: "memory", NArgs: NArgsAny, Desc: "review what zemi remembers; /memory <note> keeps one",
		GoFn: (*Host).memoryCommand,
	}}
}

// zemi.memory namespace

func (h *Host) memoryNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	memoryToLua := func(L *glua.LState, rows []*store.Memory) glua.LValue {
		out := make([]any, 0, len(rows))
		for _, m := range rows {
			out = append(out, map[string]any{
				"id": m.ID, "scope": m.Scope, "origin": m.Origin, "text": m.Text, "quote": m.Quote,
			})
		}
		return lua.FromGo(L, out)
	}

	// add(text, opts): the model/Lua path (§10.4 intake and autonomy gate).
	// Returns { stored = true, id }, { queued = true, reason }, { duplicate
	// = id }, { dropped = true } or nil, err.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		opts := L.OptTable(2, L.NewTable())
		o := h.RT.CurrentOrigin(L)
		if o.Rank() <= lua.OriginProject.Rank() {
			L.RaiseError("zemi.memory.add: %s code may not add memories", o)
		}
		scope := cmp.Or(store.Scope(rawString(L, opts, "scope")), store.ScopeProject)
		if !slices.Contains(store.Scopes, scope) {
			L.RaiseError("zemi.memory.add: scope must be project or user")
		}
		// user config writes the user's own memory; the model's Lua tool
		// runs as user origin but its notes are the model's
		memOrigin := store.OriginLearned
		if tool, _ := h.modelLuaRun(L); !tool && o == lua.OriginUser {
			memOrigin = store.OriginUser
		}
		res, err := h.proposeMemory(h.AgentSessionID(), memOrigin, scope, text, rawString(L, opts, "quote"), rawString(L, opts, "replaces"))
		if err != nil {
			return luaResult(L, err)
		}
		out := L.NewTable()
		switch {
		case res.ID != "":
			out.RawSetString("stored", glua.LTrue)
			out.RawSetString("id", glua.LString(res.ID))
		case res.Duplicate != nil:
			out.RawSetString("duplicate", glua.LString(res.Duplicate.ID))
		case res.Dropped:
			out.RawSetString("dropped", glua.LTrue)
		default:
			out.RawSetString("queued", glua.LTrue)
			out.RawSetString("reason", glua.LString(res.Queued))
		}
		L.Push(out)
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows, err := h.memoryList(L.OptBool(1, false))
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(memoryToLua(L, rows))
		return 1
	}))

	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		h.refuseBelowUser(L, "memory.del")
		var err error
		if p, m, lerr := h.locateMemory(id); lerr == nil {
			err = brain.Forget(p, cmp.Or(h.AgentSessionID(), "user"), m.ID, h.now())
		} else {
			err = lerr
		}
		return luaResult(L, err)
	}))

	tb.RawSetString("scope", enumTable(L, store.Scopes...))
	return tb
}

// proposeMemory runs a memory written in session sid (the memory tool,
// zemi.memory.add) through intake and the autonomy gate; replaces names
// the memory it supersedes. A subagent passes its parent, which a
// background run may outlive.
func (h *Host) proposeMemory(sid string, origin store.Origin, scope store.Scope, text, quote, replaces string) (memoryOutcome, error) {
	if !slices.Contains(store.Scopes, scope) {
		return memoryOutcome{}, errors.New("scope must be project or user")
	}
	res, err := h.storeMemory(sid, scope, origin, text, quote)
	if err != nil {
		return memoryOutcome{}, err
	}
	if replaces != "" {
		_, old, err := h.locateMemory(replaces)
		if err != nil {
			return memoryOutcome{}, err
		}
		res.Supersedes = old.ID
	}
	return h.gateMemory(res)
}

// memoryTool is the memory tool that run p called from session sid: text
// supersedes what replaces names, empty text with replaces forgets it, and
// a non-empty path writes a private-partition brain file (DESIGN §10.4).
func (h *Host) memoryTool(p *tools.Pipeline, sid string, call tools.MemoryCall) (string, error) {
	if call.Path != "" {
		return h.memoryFileTool(sid, call.Path, call.Content)
	}
	if strings.TrimSpace(call.Text) == "" {
		if call.Replaces == "" {
			return "", errors.New("text is empty; pass replaces=<id> to drop a memory, or path to write a brain file")
		}
		return h.forgetMemory(p, sid, call.Replaces)
	}
	out, err := h.proposeMemory(sid, store.OriginLearned, call.Scope, call.Text, call.Quote, call.Replaces)
	if err != nil {
		return "", err
	}
	msg := out.String()
	if out.Dropped || out.Duplicate != nil {
		// nothing was stored, so the call answers an error: the row and the
		// model both read a refusal as one
		return "", errors.New(msg)
	}
	return msg, nil
}

// memoryFileTool is the memory tool's file mode (DESIGN §10.4): a freeform file in
// the private partition only, its frontmatter through the injected-surface
// intake, the overwrite gated on the session's write set, settling under
// memory.autonomy like a learned one-liner; everything else queues as a
// proposal whose accept writes the file.
func (h *Host) memoryFileTool(sid, path, content string) (string, error) {
	user, private := h.brainParts()
	if private.Zero() {
		return "", errors.New("this project has no brain; trusted sessions get one")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	inUser := !user.Zero() && h.underBrainDir(user.Dir, abs)
	inPrivate := h.underBrainDir(private.Dir, abs)
	if !inUser && !inPrivate {
		return "", fmt.Errorf("the memory tool writes files in its private brain, %s, only", private.Dir)
	}
	if filepath.Base(abs) == brain.MemFile {
		return "", errors.New(memOwnedReason)
	}
	if inUser {
		return "", errors.New("user-brain files are written with the file tools; those writes ask")
	}
	name := filepath.Base(abs)
	if !strings.HasSuffix(name, ".md") {
		return "", errors.New("brain files are .md")
	}
	if len(content) > brain.ContentCap {
		return "", fmt.Errorf("content is over the %d-byte cap; split or shorten it", brain.ContentCap)
	}
	if _, scrubbed := learn.ScrubSecrets(content); scrubbed {
		return "", errors.New("a brain file cannot hold a secret; name the env var that holds it instead")
	}
	// injected-surface intake (DESIGN §10.4): the frontmatter fields get what a
	// one-liner's text gets, the lazy-read body exempt
	fm, ferr := brain.ParseFrontmatter(content)
	reason := ""
	switch {
	case ferr != nil:
		reason = "the frontmatter does not parse"
	case multiLine(fm.Description) || multiLine(fm.ReadIf):
		reason = "multi-line frontmatter"
	case learn.InstructionShaped(fm.Description) || learn.InstructionShaped(fm.ReadIf):
		reason = "instruction-shaped frontmatter"
	case brain.Oversized(fm.Description) || brain.Oversized(fm.ReadIf):
		reason = fmt.Sprintf("the description or read_if is over %d characters", brain.FieldCap)
	}
	if reason == "" {
		var want *string
		reason, want = h.overwriteGate(private, abs, name)
		if reason == "" && !h.memoryAuto(sid) {
			reason = "memory.autonomy is not auto"
		}
		if reason == "" && !h.autoApplyBudgetLeft(sid) {
			reason = "this session's auto budget is spent"
		}
		if reason != "" {
			return h.queueFileProposal(sid, private, name, content, reason)
		}
		if err := brain.WriteFile(private, sid, name, []byte(content), want, h.now()); err != nil {
			if errors.Is(err, brain.ErrStale) {
				return h.queueFileProposal(sid, private, name, content, "the file changed while the write waited")
			}
			return "", err
		}
	} else {
		return h.queueFileProposal(sid, private, name, content, reason)
	}
	h.markBrainWrite(abs, []byte(content))
	h.spendAutoBudget(sid)
	out := "wrote " + name + "; injects from the next session"
	if note := h.brainLintNote(private, name); note != "" {
		out += "; " + note
	}
	return out, nil
}

// multiLine reports a frontmatter field that would not render as one line.
func multiLine(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 127 })
}

// underBrainDir reports whether abs sits directly in the partition's
// directory, symlinks resolved.
func (h *Host) underBrainDir(dir, abs string) bool {
	real := perm.CanonPath(abs)
	root := perm.CanonPath(dir)
	if real == "" || root == "" {
		return false
	}
	rel, ok := perm.Under(root, real)
	return ok && rel != "." && !strings.Contains(rel, string(filepath.Separator))
}

// overwriteGate is the row-15 gate in one read: why an overwrite queues
// ("" when the session may apply) and the hash the apply's write expects,
// the write-set entry for an overwrite, "" for a creation (DESIGN §10.4).
func (h *Host) overwriteGate(private brain.Partition, abs, name string) (string, *string) {
	empty := ""
	prior, err := os.ReadFile(filepath.Join(private.Dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", &empty
	}
	if err != nil {
		return err.Error(), &empty
	}
	entry, ours := h.brainWriteOf(abs)
	switch {
	case !ours:
		return "the file was not written by this session; an overwrite asks", &empty
	case entry != brain.FileHash(prior):
		return "the file changed since this session wrote it; an overwrite asks", &empty
	}
	return "", &entry
}

// spendAutoBudget charges the session's auto-apply budget (DESIGN §10.4); a
// detached reflector stores nothing of its own, so only the live session
// spends.
func (h *Host) spendAutoBudget(sid string) {
	ag := h.agents
	ag.mu.Lock()
	if sid == ag.sessionID {
		ag.memAuto++
	}
	ag.mu.Unlock()
}

// acceptFileProposal writes the file-mode proposal the user accepted: the
// write replaces whatever is there now, the entry keeping its prior bytes
// for rewind (DESIGN §7).
func (h *Host) acceptFileProposal(p *memoryProposal) error {
	private := h.partition(p.Scope)
	name := filepath.Base(p.Path)
	if err := brain.WriteFile(private, cmp.Or(h.AgentSessionID(), "user"), name, []byte(p.Content), nil, h.now()); err != nil {
		return err
	}
	h.markBrainWrite(p.Path, []byte(p.Content))
	return nil
}

// queueFileProposal queues a file-mode write for review; accept writes it.
func (h *Host) queueFileProposal(sid string, p brain.Partition, name, content, reason string) (string, error) {
	pr := memoryProposal{Path: filepath.Join(p.Dir, name), Content: content, Scope: p.Kind.Scope()}
	pr.ProvenanceSessionID, pr.Origin = sid, store.OriginLearned
	if err := h.addProposal(pr); err != nil {
		return "", err
	}
	return "queued for the user's review (" + reason + ")", nil
}

// brainLintNote is the file's own open lint findings, one line.
func (h *Host) brainLintNote(p brain.Partition, name string) string {
	var out []string
	for _, f := range brain.Lint(p) {
		if f.Subject == name {
			out = append(out, f.Text)
		}
	}
	return strings.Join(out, "; ")
}

// brainWriteOf is the session's write-set entry for abs (DESIGN §10.4): the hash the
// session recorded when it last applied a write, ours when it wrote the
// file at all.
func (h *Host) brainWriteOf(abs string) (hash string, ours bool) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	hash, ours = ag.brainWrites[perm.CanonPath(abs)]
	return hash, ours
}

// markBrainWrite records a brain file this session wrote, with the hash of
// what it wrote, so a later file-mode overwrite applies only while the file
// still matches (DESIGN §10.4). Approved file-tool writes join here through the
// AfterWrite hook.
func (h *Host) markBrainWrite(abs string, content []byte) {
	ag := h.agents
	ag.mu.Lock()
	if ag.brainWrites == nil {
		ag.brainWrites = map[string]string{}
	}
	ag.brainWrites[perm.CanonPath(abs)] = brain.FileHash(content)
	ag.mu.Unlock()
}

// brainAfterWrite scrubs the brain path an approved file-tool write just made
// (DESIGN §10.4), after the write: the ask showed the bytes the tool wrote,
// and the note names what the scrub changed on top.
func (h *Host) brainAfterWrite(path string) string {
	user, private := h.brainParts()
	for _, p := range []brain.Partition{user, private} {
		// the brain dir is flat by design: only its direct children are
		// brain files, so a nested memories.md is freeform everywhere else too
		if p.Zero() || !h.underBrainDir(p.Dir, path) {
			continue
		}
		return h.scrubBrainWrite(p, path)
	}
	return ""
}

// redactedLines numbers the lines a scrub's marker landed on, 1-based.
func redactedLines(clean string) []int {
	var lines []int
	for i, l := range strings.Split(clean, "\n") {
		if strings.Contains(l, "[redacted ") {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// scrubBrainWrite runs the recipe treatment on the file under the partition
// flock and lints it, the note appending what was redacted and the file's
// open findings. Redacts, never drops; a failure costs the note only, the
// write happened.
func (h *Host) scrubBrainWrite(p brain.Partition, path string) string {
	unlock, err := brain.Lock(p)
	if err != nil {
		return "\n\nbrain: not scrubbed: " + err.Error()
	}
	defer unlock()
	prior, err := os.ReadFile(path)
	if err != nil {
		return "\n\nbrain: not scrubbed: " + err.Error()
	}
	clean := learn.ScrubShapes(string(prior))
	note := ""
	if clean != string(prior) {
		if perm.CanonPath(path) == perm.CanonPath(filepath.Join(p.Dir, brain.MemFile)) {
			var applied bool
			note, applied = h.scrubMemories(p, path, prior, []byte(clean))
			if !applied {
				clean = string(prior)
			}
		} else if err := config.WriteFileAtomic(path, []byte(clean)); err != nil {
			clean = string(prior)
			note = "could not be cleaned: " + err.Error()
		} else {
			note = fmt.Sprintf("secret-shaped text was redacted on lines %v; name the env var or use a placeholder like $TOKEN", redactedLines(clean))
		}
	}
	h.markBrainWrite(path, []byte(clean))
	if f := h.brainLintNote(p, filepath.Base(path)); f != "" {
		if note != "" {
			note += "; "
		}
		note += f
	}
	if note == "" {
		return ""
	}
	return "\n\nbrain: " + note
}

// scrubMemories replaces the reserved file's bytes and journals an edit entry
// per changed line, prior text and the new hash (DESIGN §7, §10.4), the ledger first
// under the caller's partition lock. ScrubShapes never adds, drops or
// reorders a line, so the lines diff by position; a changed line that does
// not parse journals nothing, so the note falls back to the line numbers.
func (h *Host) scrubMemories(p brain.Partition, path string, prior, clean []byte) (string, bool) {
	before, _ := brain.ParseMemories(prior)
	after, _ := brain.ParseMemories(clean)
	lg, err := p.OpenLedger()
	if err != nil {
		return "could not open the memory ledger: " + err.Error() + "; not rewritten", false
	}
	defer func() { _ = lg.Close() }()
	var ids []string
	for i, line := range after {
		if i >= len(before) || line.ID != before[i].ID || line.Text == before[i].Text {
			continue
		}
		op := &brain.Op{
			Op: brain.OpEdit, ID: line.ID, Text: line.Text, PriorText: before[i].Text,
			SessionID: cmp.Or(h.AgentSessionID(), "user"), Hash: brain.TextHash(line.Text),
			Scope: p.Kind.Scope(), Origin: line.Origin, TS: h.now().UnixMilli(),
		}
		if err := lg.Insert(op); err != nil {
			return "the redaction is not journaled: " + err.Error() + "; not rewritten", false
		}
		ids = append(ids, line.ID)
	}
	if err := config.WriteFileAtomic(path, clean); err != nil {
		return "could not be cleaned: " + err.Error(), false
	}
	if len(ids) > 0 {
		return fmt.Sprintf("secret-shaped text was redacted from memory lines %v; their prior text is in the memory ledger", ids), true
	}
	return fmt.Sprintf("secret-shaped text was redacted on lines %v; name the env var or use a placeholder like $TOKEN", redactedLines(string(clean))), true
}

// forgetMemory drops a memory for the model: one the user saved asks
// (Unsandboxed so no mode settles it), a learned one goes at once under
// auto autonomy and is proposed for review otherwise. The ask goes through
// p, so it names the agent that asked.
func (h *Host) forgetMemory(p *tools.Pipeline, sid, id string) (string, error) {
	db, m, err := h.locateMemory(id)
	if err != nil {
		return "", err
	}
	switch {
	case m.Origin == store.OriginUser:
		req := tools.Request{Tool: tools.ToolMemory, Subject: "memory.write", Arg: m.ID, Reason: "the user saved it", Preview: m.Text, Unsandboxed: true}
		if v := p.CheckDenied(req); v.Decision != perm.Allow {
			return "", fmt.Errorf("permission denied (%s): forget memory %s", v.DecidedBy, m.ID)
		}
	case !h.memoryAuto(sid):
		if _, err := h.queueForget(sid, m.ID); err != nil {
			return "", err
		}
		return "forget of " + m.ID + " queued for the user's review", nil
	}
	if err := brain.Forget(db, sid, m.ID, h.now()); err != nil {
		return "", err
	}
	return m.ID + " dropped", nil
}

// gateMemory is the autonomy gate (DESIGN §10.4): a near-duplicate is
// refused, a quote the user never wrote drops the memory, and what
// memoryReview finds no reason to hold is stored, the rest queued. A cut
// that would take a line the user saved queues instead.
func (h *Host) gateMemory(res *learn.Result) (memoryOutcome, error) {
	m := &res.Memory
	if dup, queued := h.duplicateMemory(*m, res.Supersedes); dup != nil {
		return memoryOutcome{Duplicate: dup, DupQueued: queued}, nil
	}
	if m.Origin == store.OriginLearned && m.Quote != "" && !h.quoteInSession(m.ProvenanceSessionID, m.Quote) {
		h.Log.Info("dropped: quote not in the session", "origin", "memory", "text", m.Text, "quote", m.Quote)
		return memoryOutcome{Dropped: true}, nil
	}
	reason, review := h.memoryReview(res)
	if !review {
		cut, err := h.applyProposal(res)
		if err != nil {
			if uc, ok := errors.AsType[brain.UserCutError](err); ok {
				// the write queues instead, so the proposal is not
				// dropped while the user makes room
				return memoryOutcome{Queued: userCutFull(m.Scope, uc).Error()}, h.addProposal(memoryProposal{Memory: *m, Supersedes: res.Supersedes})
			}
			return memoryOutcome{}, err
		}
		return memoryOutcome{ID: m.ID, Cut: cut}, nil
	}
	return memoryOutcome{Queued: reason}, h.addProposal(memoryProposal{Memory: *m, Supersedes: res.Supersedes})
}

// memoryReview says why res waits for the user, or that it may be stored
// at once. The budget is not a reason: the apply cuts the oldest learned
// lines to the ledger (DESIGN §10.4), and a cut that would take a user line
// queues from the apply's refusal.
func (h *Host) memoryReview(res *learn.Result) (reason string, review bool) {
	m := &res.Memory
	switch {
	case m.Origin == store.OriginLearned && m.Scope == store.ScopeUser && m.Quote == "":
		return "a memory about the user needs a quote", true
	case !h.memoryAuto(m.ProvenanceSessionID):
		return "", true
	case res.InstructionShaped:
		return "instruction-shaped text", true
	case h.replacementNeedsReview(res):
		return "replaces a memory the user saved or one of the other scope", true
	case !h.autoApplyBudgetLeft(m.ProvenanceSessionID):
		return "this session's auto budget is spent", true
	}
	return "", false
}

// userCutFull refuses what would cut a line the user saved (DESIGN §10.4):
// the cut never takes one, so the user makes room, the id naming the line.
func userCutFull(scope store.Scope, uc brain.UserCutError) error {
	return fmt.Errorf("%w (its oldest line %s is yours)", errScopeFull(scope), uc.ID)
}

func errScopeFull(scope store.Scope) error {
	return fmt.Errorf("%s memory is full; forget one of its oldest in /memory first", scope)
}

// scopeOver reports whether storing res would push its scope past
// brain.ScopeBudget; the count is the partition's memories.md lines (DESIGN §7).
func (h *Host) scopeOver(res *learn.Result) bool {
	lines, err := brain.Lines(h.partition(res.Memory.Scope))
	if err != nil {
		return false
	}
	used := brain.LineSize(res.Memory.Text)
	for _, l := range lines {
		if l.ID != res.Supersedes && l.ID != res.Memory.ID {
			used += brain.LineSize(l.Text)
		}
	}
	return used > brain.ScopeBudget
}

// quoteInSession reports whether the user wrote quote in session sid:
// case-folded and whitespace-collapsed, inside a user row that is not a
// compaction summary.
func (h *Host) quoteInSession(sid, quote string) bool {
	if h.DB == nil {
		return false
	}
	msgs, err := h.DB.Messages(sid)
	if err != nil {
		return false
	}
	want := foldSpace(quote)
	// only what was typed: an attached file or a command's output is not the user's word
	return slices.ContainsFunc(msgs, func(m *store.Message) bool {
		typed, ok := userText(m)
		return ok && strings.Contains(foldSpace(typed), want)
	})
}

func foldSpace(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// memoryAuto reports whether auto autonomy may skip review for what session
// sid wrote. It may not in an untrusted project, or when the session is not
// local only.
func (h *Host) memoryAuto(sid string) bool {
	return h.Settings.Str("memory.autonomy") == "auto" && h.ProjectTrusted() && h.sessionLocalOnly(sid)
}

// markNotLocal records that plugin code fed session sid text, a sent
// message or a rewritten tool result, as a telemetry row sessionLocalOnly
// reads like a tool zemi does not ship: the plugin may relay any text. One
// row per plugin, read from the store, so a rewind that drops it re-marks.
func (h *Host) markNotLocal(sid, who string) {
	if h.DB == nil || sid == "" {
		return
	}
	stats, _ := h.DB.ToolCallStats(sid, "")
	if !slices.ContainsFunc(stats, func(s store.ToolStat) bool { return s.Tool == who }) {
		h.recordToolCall(sid, StreamTool{Name: who}, true)
	}
}

// sessionLocalOnly reports that neither session sid nor one of its subagents
// read text from outside the machine (fetch, web_search, a Lua or MCP tool,
// any tool zemi does not ship, a plugin's text), where injected text would
// come from.
// NOTE: a shell command the user approved can fetch too and is not seen here.
func (h *Host) sessionLocalOnly(sid string) bool {
	if h.DB == nil {
		return false
	}
	sids := []string{sid}
	children, err := h.DB.ChildSessions(sid)
	if err != nil {
		return false
	}
	for _, c := range children {
		sids = append(sids, c.ID)
	}
	local := h.builtinToolNames()
	for _, id := range sids {
		stats, err := h.DB.ToolCallStats(id, "")
		if err != nil {
			return false
		}
		for _, s := range stats {
			if s.Tool == tools.ToolFetch || s.Tool == tools.ToolWebSearch || s.Tool == tools.ToolLua || !slices.Contains(local, s.Tool) {
				return false
			}
		}
	}
	return true
}

// replacementNeedsReview reports whether res supersedes a user-attested
// memory, or one in the other scope: a user-scope memory superseded from a
// project is hidden in every project (DESIGN §10.4).
func (h *Host) replacementNeedsReview(res *learn.Result) bool {
	if res.Supersedes == "" {
		return false
	}
	_, old, err := h.locateMemory(res.Supersedes)
	return err == nil && (old.Origin == store.OriginUser || old.Scope != res.Memory.Scope)
}

// autoApplyBudgetLeft caps what the live session stores without review;
// a closed session's writes (a detached reflector) are not counted.
func (h *Host) autoApplyBudgetLeft(sid string) bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return sid != ag.sessionID || ag.memAuto < memoryAutoApplyCap
}
