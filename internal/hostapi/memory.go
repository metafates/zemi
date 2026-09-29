// memory.go is the memory host API (DESIGN §6.7, §7, §10.4): the
// zemi.memory namespace, the Go-owned /memory command, the
// proposal queue behind the autonomy gate, and the §10.7 digest. Proposals
// persist in their scope's store until the user accepts or rejects them.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// memoryAutoApplyCap bounds model-proposed memories stored without review
// per session, mirroring the auto-applied-rules cap (DESIGN §10.4).
const memoryAutoApplyCap = 20

// MemoryRow is one review-overlay row: a queued proposal or a stored
// memory from either scope.
type MemoryRow struct {
	ID         string
	Scope      store.Scope
	Origin     store.Origin
	Text       string
	Quote      string // the user's words behind a memory about them
	Queued     bool   // proposal awaiting review, not yet stored
	Forget     bool   // proposal to forget a stored memory, which stays in its scope
	Supersedes string // the stored memory a forget or a replacement retires
	Updated    int64  // last write of a stored memory, unix ms
	Action     string // set by the review screen: accept, reject, delete or ""
	Edit       bool   // set by the review screen: open this row in the editor, then reopen
}

// memoryDB returns the store owning a scope: user scope in the global DB,
// project scope in the project DB (DESIGN §7).
func (h *Host) memoryDB(scope store.Scope) *store.DB {
	if scope == store.ScopeUser {
		return h.GlobalDB
	}
	return h.DB
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
		h.Log.Debug("memory", "duplicate proposal dropped", nil)
		return false
	}
	if err := h.addProposal(memoryProposal{Memory: res.Memory, Supersedes: res.Supersedes}); err != nil {
		h.Log.Warn("memory", "proposal dropped", map[string]any{"error": err.Error()})
		return false
	}
	return true
}

// addProposal persists a proposal in its scope's store as it is.
func (h *Host) addProposal(p memoryProposal) error {
	if p.ID == "" {
		p.ID = store.NewID()
	}
	db := h.memoryDB(p.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	payload, err := json.Marshal(p)
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
// of the per-session auto-apply budget.
func (h *Host) applyProposal(res *learn.Result) error {
	if err := h.storeProposal(res); err != nil {
		return err
	}
	ag := h.agents
	ag.mu.Lock()
	// a detached reflector stores a closed session's memories; the live
	// session's budget is not theirs to spend
	if res.Memory.ProvenanceSessionID == ag.sessionID {
		ag.memAuto++
	}
	ag.mu.Unlock()
	h.Notify(logging.Info, "Memory saved", res.Memory.Text+redactNote(res.Scrubbed))
	return nil
}

// memoryOutcome is what became of a proposed memory: stored under ID,
// queued for review (Queued says why, "" for the autonomy setting alone),
// refused as a near-duplicate of a live or pending one, or dropped because
// its quote is not in the session.
type memoryOutcome struct {
	ID        string
	Queued    string
	Duplicate *store.Memory
	DupQueued bool // Duplicate is a pending proposal, which replaces cannot name
	Dropped   bool
}

// String is the memory tool's answer: the id and what to do next.
func (o memoryOutcome) String() string {
	switch {
	case o.ID != "":
		return "saved as " + learn.Truncate(o.ID, 8) + "; shows from the next session"
	case o.Duplicate != nil && o.DupQueued:
		return fmt.Sprintf("already queued for the user's review as %s: %q", learn.Truncate(o.Duplicate.ID, 8), o.Duplicate.Text)
	case o.Duplicate != nil:
		id := learn.Truncate(o.Duplicate.ID, 8)
		return fmt.Sprintf("near-duplicate of %s (%s): %q; to keep both facts, pass replaces=%s with one line that says both", id, o.Duplicate.Scope, o.Duplicate.Text, id)
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
	errNoMemory        = errors.New("no such memory")
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

// storeProposal writes a policy-clean memory to its scope's DB.
func (h *Host) storeProposal(res *learn.Result) error {
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	m := &res.Memory
	if dup, _ := h.duplicateMemory(*m, res.Supersedes); dup != nil {
		return fmt.Errorf("%w: %q", errDuplicateMemory, dup.Text)
	}
	if err := db.AddMemory(m); err != nil {
		return err
	}
	if res.Supersedes != "" {
		h.linkSupersede(res.Supersedes, *m)
	}
	h.EmitAsync("memory.written", map[string]any{
		"id": m.ID, "scope": m.Scope, "origin": m.Origin, "text": m.Text,
	})
	return nil
}

// Remember stores a user-typed /memory <note> text (origin user, project
// scope): the keystroke bypasses the autonomy gate (DESIGN §10.4), and a
// full scope is refused, since the user is there to make room.
func (h *Host) Remember(text string) error {
	if strings.TrimSpace(text) == "" {
		return Usagef("usage: /memory <note>")
	}
	res, err := h.storeMemory(h.AgentSessionID(), store.ScopeProject, store.OriginUser, text, "")
	if err != nil {
		return err
	}
	if _, over := h.scopeOver(res); over {
		return errScopeFull(res.Memory.Scope)
	}
	if err := h.storeProposal(res); err != nil {
		return err
	}
	h.Notify(logging.Info, "Memory saved for this project", "press s on it in /memory to keep it for every project"+redactNote(res.Scrubbed))
	return nil
}

// memoryCommand dispatches /memory; a forget typed without --yes asks
// first (AskFirst) on the review screen.
func (h *Host) memoryCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		return h.openMemory("")
	case fields[0] == "forget" && (len(fields) == 2 || len(fields) == 3 && fields[2] == "--yes"):
		db, m, err := h.locateMemory(fields[1])
		if err != nil {
			return err
		}
		if len(fields) == 2 {
			return AskFirst("memory forget "+m.ID+" --yes", "", func(string) error { return h.openMemory(m.ID) })
		}
		if err := db.DeleteMemory(m.ID); err != nil {
			return err
		}
		h.Notify(logging.Info, "Memory forgotten", m.Text)
		return nil
	case fields[0] == "forget":
		return Usagef("usage: /memory forget <id> [--yes]")
	default:
		return h.Remember(args)
	}
}

// openMemory opens the review screen on its own goroutine, since the hook
// blocks, with memory focus open on its Forget when set.
func (h *Host) openMemory(focus string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if !h.memReview.TryLock() {
		return Usagef("memory review already open")
	}
	h.GoSafe("memory review", func() {
		defer h.memReview.Unlock()
		h.openMemoryReview(ui, focus)
	})
	return nil
}

// openMemoryReview shows pending proposals plus stored memories from both
// scopes in the Go-owned review overlay and applies the decisions by row
// id: accepted proposals are stored, rejected ones dropped, deleted
// memories removed from their scope's DB. A row marked for editing opens
// in the user's editor after the other decisions apply, then the screen
// opens again.
func (h *Host) openMemoryReview(ui UIHooks, focus string) {
	for h.reviewMemoriesOnce(ui, focus) {
		focus = ""
	}
}

// reviewMemoriesOnce runs the screen once and reports whether a row was
// edited, so the caller shows it again.
func (h *Host) reviewMemoriesOnce(ui UIHooks, focus string) bool {
	rows := h.memoryRows()
	was := map[string]MemoryRow{}
	for _, r := range rows {
		was[r.ID] = r
	}
	var accepted, rejected, deleted, moved int
	edit := ""
	for _, r := range ui.ReviewMemories(rows, focus) {
		old, ok := was[r.ID]
		if !ok {
			continue
		}
		if r.Edit {
			edit = r.ID
		}
		if r.Scope != store.ScopeUser {
			r.Scope = store.ScopeProject
		}
		if old.Forget {
			r.Scope = old.Scope // it names the memory where it is
		}
		switch {
		case old.Queued && r.Action == "reject":
			if h.takeProposal(r.ID) != nil {
				rejected++
			}
		case old.Queued && r.Action == "accept":
			if _, err := h.acceptProposal(r.ID, r.Scope); err != nil {
				h.Notify(logging.Warn, "Could not accept a memory", err.Error())
				continue
			}
			accepted++
		case old.Queued && r.Scope != old.Scope:
			p := h.takeProposal(r.ID)
			if p == nil {
				continue
			}
			p.Memory.Scope = r.Scope
			if h.queueProposal(&learn.Result{Memory: p.Memory, Supersedes: p.Supersedes}) {
				moved++
				continue
			}
			// the other scope already holds the text: it is out of its
			// queue by now, so put it back rather than lose it
			p.Memory.Scope = old.Scope
			if err := h.addProposal(*p); err != nil {
				h.Log.Warn("memory", "move rollback failed", map[string]any{"id": r.ID, "error": err.Error()})
			}
			h.Notify(logging.Warn, "Could not move a memory", "the "+string(r.Scope)+" scope already has that text")
		case old.Queued:
		case r.Action == "delete":
			if db := h.memoryDB(old.Scope); db != nil && db.DeleteMemory(r.ID) == nil {
				deleted++
			}
		case r.Scope != old.Scope:
			if err := h.moveMemory(r.ID, old.Scope, r.Scope); err != nil {
				h.Notify(logging.Warn, "Could not move a memory", err.Error())
				continue
			}
			moved++
		}
	}
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{{accepted, "accepted"}, {rejected, "rejected"}, {deleted, "deleted"}, {moved, "moved"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	if len(parts) > 0 {
		h.Notify(logging.Info, "Memory updated", strings.Join(parts, ", "))
	}
	if edit != "" {
		h.editMemory(ui, edit)
	}
	return edit != ""
}

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
		h.Notify(logging.Error, "Could not edit the memory", err.Error())
	case scrubbed:
		h.Notify(logging.Info, "Memory updated", "secrets redacted")
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
// text changes nothing, and so does a refusal, a full scope included.
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
		if _, over := h.scopeOver(res); over {
			return false, errScopeFull(p.Scope)
		}
		if h.takeProposal(id) == nil {
			return false, fmt.Errorf("no such proposal %q", id)
		}
		if err := h.storeProposal(res); err != nil {
			if qerr := h.addProposal(p); qerr != nil {
				h.Log.Warn("memory", "proposal dropped", map[string]any{"error": qerr.Error()})
			}
			return false, err
		}
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
	if _, over := h.scopeOver(res); over && len(res.Memory.Text) > len(m.Text) {
		return false, errScopeFull(m.Scope)
	}
	if dup, _ := h.duplicateMemory(res.Memory, ""); dup != nil {
		return false, fmt.Errorf("%w: %q", errDuplicateMemory, dup.Text)
	}
	return res.Scrubbed, db.UpdateMemoryText(m.ID, res.Memory.Text, store.OriginUser)
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

// moveMemory stores a memory in the other scope, then deletes it from the
// old one; a failed add, or a full destination, leaves it where it was.
func (h *Host) moveMemory(id string, from, to store.Scope) error {
	src, dst := h.memoryDB(from), h.memoryDB(to)
	if src == nil || dst == nil {
		return errors.New("memory store unavailable")
	}
	m, err := src.GetMemory(id)
	if err != nil {
		return err
	}
	m.Scope = to
	// a stored copy already there makes the move a merge; a pending proposal
	// with the same text does not, it may yet be dropped
	live, err := dst.ListMemory(to, false)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(live, func(o *store.Memory) bool { return strings.EqualFold(o.Text, m.Text) })
	switch {
	case i < 0:
		if _, over := h.scopeOver(&learn.Result{Memory: *m}); over {
			return errScopeFull(to)
		}
		err = dst.AddMemory(m)
	case m.Origin == store.OriginUser && live[i].Origin != store.OriginUser:
		// the copy left behind keeps the user's attestation (DESIGN §10.4)
		err = dst.UpdateMemoryText(live[i].ID, live[i].Text, store.OriginUser)
	}
	if err != nil {
		return err
	}
	if i >= 0 {
		// pending forgets and replacements follow the text to the copy it merged into
		for _, p := range h.memoryProposals() {
			if p.Supersedes == id && h.takeProposal(p.ID) != nil {
				p.Supersedes = live[i].ID
				if err := h.addProposal(p); err != nil {
					return err
				}
			}
		}
	}
	return src.DeleteMemory(id)
}

// acceptProposal stores a pending proposal (in scope when one is given), or deletes the memory a forget
// proposal names (one already gone counts as done, one rewritten since drops
// the proposal). A full scope or a store failure other than a duplicate puts
// it back in the queue, so the decision can be made again.
func (h *Host) acceptProposal(id string, scope store.Scope) (*memoryProposal, error) {
	p := h.takeProposal(id)
	if p == nil {
		return nil, fmt.Errorf("no such proposal %q", id)
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
			err = db.DeleteMemory(p.Supersedes)
		}
	} else {
		p.Memory.Scope = cmp.Or(scope, p.Memory.Scope)
		res := &learn.Result{Memory: p.Memory, Supersedes: p.Supersedes}
		if _, over := h.scopeOver(res); over {
			err = errScopeFull(p.Memory.Scope)
		} else {
			err = h.storeProposal(res)
		}
	}
	if err != nil {
		if !errors.Is(err, errDuplicateMemory) {
			if qerr := h.addProposal(*p); qerr != nil {
				h.Log.Warn("memory", "proposal dropped", map[string]any{"error": qerr.Error()})
			}
		}
		return nil, err
	}
	return p, nil
}

// memoryProposal is a queued proposal's payload. Forget proposes deleting
// the memory Supersedes names; the embedded memory is a copy of it.
type memoryProposal struct {
	store.Memory
	Supersedes string `json:",omitempty"`
	Forget     bool   `json:",omitzero"`
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
			h.Log.Warn("memory", "proposal list failed", map[string]any{"error": err.Error()})
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

// memoryRows merges pending proposals (first) and stored memories from
// both scopes, newest first within a scope.
func (h *Host) memoryRows() []MemoryRow {
	var rows []MemoryRow
	for _, m := range h.memoryProposals() {
		text := m.Text
		switch {
		case m.Forget:
			text = "forget: " + text
		case m.Supersedes != "":
			// the screen shows no ids, so the row names the memory by its text
			what := "a memory since deleted"
			if _, old, err := h.locateMemory(m.Supersedes); err == nil {
				what = `"` + clipRunes(old.Text, 40) + `"`
				if old.Scope != m.Scope {
					what = string(old.Scope) + " memory " + what
				}
			}
			text += " (replaces " + what + ")"
		}
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Origin: m.Origin,
			Text: text, Quote: m.Quote, Queued: true, Forget: m.Forget, Supersedes: m.Supersedes,
		})
	}
	list, _ := h.memoryList(false)
	for _, m := range list {
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Origin: m.Origin,
			Text: m.Text, Quote: m.Quote, Updated: m.UpdatedAt,
		})
	}
	return rows
}

// locateMemory finds a memory id across both scopes and returns the DB
// owning it.
func (h *Host) locateMemory(id string) (*store.DB, *store.Memory, error) {
	if len(id) >= 6 && len(id) < 32 {
		// the digest shows 8-character id prefixes
		// NOTE: scans every memory; fine for the hundreds of rows memories reach.
		all, err := h.memoryList(true)
		if err != nil {
			return nil, nil, err
		}
		var hits []string
		for _, m := range all {
			if strings.HasPrefix(m.ID, id) {
				hits = append(hits, m.ID)
			}
		}
		switch len(hits) {
		case 0:
			return nil, nil, fmt.Errorf("%w %q", errNoMemory, id)
		case 1:
			id = hits[0]
		default:
			return nil, nil, fmt.Errorf("memory id %q matches %d memories; pass more of it", id, len(hits))
		}
	}
	for _, db := range []*store.DB{h.DB, h.GlobalDB} {
		if db == nil {
			continue
		}
		m, err := db.GetMemory(id)
		if err == nil {
			return db, m, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("%w %q", errNoMemory, id)
}

// memoryList merges both scopes, project first, each newest-first. A
// failing scope is logged and skipped; its error comes back with the rows.
func (h *Host) memoryList(includeSuperseded bool) ([]*store.Memory, error) {
	var out []*store.Memory
	var errs []error
	for _, scope := range []store.Scope{store.ScopeProject, store.ScopeUser} {
		db := h.memoryDB(scope)
		if db == nil {
			continue
		}
		rows, err := db.ListMemory(scope, includeSuperseded)
		if err != nil {
			h.Log.Warn("memory", "list failed", map[string]any{"scope": scope, "error": err.Error()})
			errs = append(errs, err)
			continue
		}
		out = append(out, rows...)
	}
	return out, errors.Join(errs...)
}

// digestFor renders the §10.7 memory section: every live memory of both
// scopes, newest first, under the per-scope budget.
func (h *Host) digestFor() string {
	rows, _ := h.memoryList(false)
	return learn.Digest(rows, time.Now(), learn.ScopeBudget)
}

// memoryCommands is /memory.
func (h *Host) memoryCommands() []*Command {
	return []*Command{{
		Name: "memory", NArgs: NArgsAny, Desc: "review what zemi remembers; /memory <note> keeps one",
		GoFn: func(h *Host, args string) error { return h.memoryCommand(args) },
	}}
}

// ---------------------------------------------------------------------------
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
		db, m, err := h.locateMemory(id)
		if err == nil {
			err = db.DeleteMemory(m.ID)
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
// supersedes what replaces names, empty text with replaces forgets it.
func (h *Host) memoryTool(p *tools.Pipeline, sid string, scope store.Scope, text, quote, replaces string) (string, error) {
	if strings.TrimSpace(text) == "" {
		if replaces == "" {
			return "", errors.New("text is empty; pass replaces=<id> to drop a memory")
		}
		return h.forgetMemory(p, sid, replaces)
	}
	out, err := h.proposeMemory(sid, store.OriginLearned, scope, text, quote, replaces)
	if err != nil {
		return "", err
	}
	return out.String(), nil
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
	short := m.ID[:8]
	switch {
	case m.Origin == store.OriginUser:
		req := tools.Request{Tool: tools.ToolMemory, Subject: "memory.write", Arg: short, Reason: "the user saved it", Preview: m.Text, Unsandboxed: true}
		if v := p.CheckDenied(req); v.Decision != perm.Allow {
			return "", fmt.Errorf("permission denied (%s): forget memory %s", v.DecidedBy, short)
		}
	case !h.memoryAuto(sid):
		if _, err := h.queueForget(sid, m.ID); err != nil {
			return "", err
		}
		return "forget of " + short + " queued for the user's review", nil
	}
	if err := db.DeleteMemory(m.ID); err != nil {
		return "", err
	}
	return short + " dropped", nil
}

// gateMemory is the autonomy gate (DESIGN §10.4): a near-duplicate is
// refused, a quote the user never wrote drops the memory, and what
// memoryReview finds no reason to hold is stored, the rest queued.
func (h *Host) gateMemory(res *learn.Result) (memoryOutcome, error) {
	m := &res.Memory
	if dup, queued := h.duplicateMemory(*m, res.Supersedes); dup != nil {
		return memoryOutcome{Duplicate: dup, DupQueued: queued}, nil
	}
	if m.Origin == store.OriginLearned && m.Quote != "" && !h.quoteInSession(m.ProvenanceSessionID, m.Quote) {
		h.Log.Info("memory", "dropped: quote not in the session", map[string]any{"text": m.Text, "quote": m.Quote})
		return memoryOutcome{Dropped: true}, nil
	}
	reason, review := h.memoryReview(res)
	if !review {
		return memoryOutcome{ID: m.ID}, h.applyProposal(res)
	}
	return memoryOutcome{Queued: reason}, h.addProposal(memoryProposal{Memory: *m, Supersedes: res.Supersedes})
}

// memoryReview says why res waits for the user, or that it may be stored
// at once.
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
	if oldest, over := h.scopeOver(res); over {
		return fmt.Sprintf("%s memory is full; replace one of %s to store at once", m.Scope, strings.Join(oldest, ", ")), true
	}
	return "", false
}

func errScopeFull(scope store.Scope) error {
	return fmt.Errorf("%s memory is full; forget one of its oldest in /memory first", scope)
}

// scopeOver reports whether storing res would push its scope past
// learn.ScopeBudget, with the three oldest ids to replace when so.
func (h *Host) scopeOver(res *learn.Result) (oldest []string, over bool) {
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return nil, false
	}
	live, _ := db.ListMemory(res.Memory.Scope, false)
	used := learn.LineSize(res.Memory.Text)
	for _, o := range live {
		if o.ID != res.Supersedes && o.ID != res.Memory.ID {
			used += learn.LineSize(o.Text)
		}
	}
	if used <= learn.ScopeBudget {
		return nil, false
	}
	for i := len(live) - 1; i >= 0 && len(oldest) < 3; i-- {
		oldest = append(oldest, live[i].ID[:8])
	}
	return oldest, true
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
	for _, m := range msgs {
		if m.Role != string(fantasy.MessageRoleUser) {
			continue
		}
		var fm fantasy.Message
		if json.Unmarshal([]byte(m.PartsJSON), &fm) != nil {
			continue
		}
		if _, summary := summaryOf(fm); summary {
			continue
		}
		// only what was typed: an attached file or a command's output is not the user's word
		typed, _, _ := strings.Cut(concatText(fm.Content), agent.AttachMarker)
		if strings.Contains(foldSpace(typed), want) {
			return true
		}
	}
	return false
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
// NOTE: a bash command the user approved can fetch too and is not seen here.
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
