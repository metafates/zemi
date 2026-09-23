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

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// memoryAutoApplyCap bounds model-proposed memories stored without review
// per session, mirroring the auto-applied-rules cap (DESIGN §10.4).
const memoryAutoApplyCap = 20

// MemoryRow is one review-overlay row: a queued proposal or a stored
// memory from either scope.
type MemoryRow struct {
	ID      string
	Scope   string // "user" or "project"
	Origin  string // "user" or "learned"
	Text    string
	Quote   string // the user's words behind a memory about them
	Queued  bool   // proposal awaiting review, not yet stored
	Forget  bool   // proposal to forget a stored memory, which stays in its scope
	Updated int64  // last write of a stored memory, unix ms
	Action  string // set by the review screen: accept, reject, delete or ""
	Edit    bool   // set by the review screen: open this row in the editor, then reopen
}

// memoryDB returns the store owning a scope: user scope in the global DB,
// project scope in the project DB (DESIGN §7).
func (h *Host) memoryDB(scope string) *store.DB {
	if scope == "user" {
		return h.GlobalDB
	}
	return h.DB
}

// storeMemory runs the §10.4 intake policy over what session sid wrote;
// user config before the first send is its own provenance.
func (h *Host) storeMemory(sid, scope, origin, text, quote string) (*learn.Result, error) {
	if sid == "" {
		if origin != learn.OriginUser {
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
	if _, _, dup := h.duplicateMemory(res.Memory, res.Supersedes); dup {
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
		err = db.AddProposal(&store.Proposal{ID: p.ID, Kind: "memory", Payload: string(payload)})
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
	h.Notify("info", "Remembered", res.Memory.Text+redactNote(res.Scrubbed))
	return nil
}

// memoryOutcome is what became of a proposed memory: stored under ID,
// queued for review (Queued says why, "" for the autonomy setting alone),
// refused as a near-duplicate of a live or pending one, or dropped because
// its quote is not in the session.
type memoryOutcome struct {
	ID        string
	Queued    string
	Duplicate string
	DupScope  string
	Dropped   bool
}

// String is the memory tool's answer: the id and what to do next.
func (o memoryOutcome) String() string {
	switch {
	case o.ID != "":
		return "saved as " + learn.Truncate(o.ID, 8) + "; shows from the next session"
	case o.Duplicate != "":
		id := learn.Truncate(o.Duplicate, 8)
		return fmt.Sprintf("near-duplicate of %s (%s); pass replaces=%s to rewrite it", id, o.DupScope, id)
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
// proposal, that says what m says (learn.Similar); m itself and the row
// it replaces are no candidates.
// NOTE: scans both scopes; fine for the dozens of rows a budget holds.
func (h *Host) duplicateMemory(m store.Memory, replaces string) (id, scope string, found bool) {
	live, _ := h.memoryList(false)
	for _, o := range live {
		if o.ID != m.ID && o.ID != replaces && learn.Similar(o.Text, m.Text) {
			return o.ID, o.Scope, true
		}
	}
	for _, o := range h.memoryProposals() {
		if !o.Forget && o.ID != m.ID && learn.Similar(o.Text, m.Text) {
			return o.ID, o.Scope, true
		}
	}
	return "", "", false
}

// storeProposal writes a policy-clean memory to its scope's DB.
func (h *Host) storeProposal(res *learn.Result) error {
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	m := &res.Memory
	if id, _, dup := h.duplicateMemory(*m, res.Supersedes); dup {
		return fmt.Errorf("%w as %s", errDuplicateMemory, learn.Truncate(id, 8))
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
// full scope is refused with the ids to forget, since the user is there.
func (h *Host) Remember(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("usage: /memory <note>")
	}
	res, err := h.storeMemory(h.AgentSessionID(), "project", learn.OriginUser, text, "")
	if err != nil {
		return err
	}
	if oldest, over := h.scopeOver(res); over {
		return errScopeFull(res.Memory.Scope, oldest)
	}
	if err := h.storeProposal(res); err != nil {
		return err
	}
	h.Notify("info", "Remembered for this project"+redactNote(res.Scrubbed), "s in /memory makes it apply everywhere")
	return nil
}

// memoryCommand dispatches /memory; no args opens the review overlay on
// its own goroutine so the UI goroutine never blocks on the hook.
func (h *Host) memoryCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		if h.memReview.TryLock() {
			h.GoSafe("memory review", func() {
				defer h.memReview.Unlock()
				h.openMemoryReview()
			})
			return nil
		}
		return errors.New("memory review already open")
	case fields[0] == "forget" && len(fields) == 2:
		db, m, err := h.locateMemory(fields[1])
		if err != nil {
			return err
		}
		return db.DeleteMemory(m.ID)
	case fields[0] == "forget":
		return errors.New("usage: /memory forget <id>")
	default:
		return h.Remember(args)
	}
}

// openMemoryReview shows pending proposals plus stored memories from both
// scopes in the Go-owned review overlay and applies the decisions by row
// id: accepted proposals are stored, rejected ones dropped, deleted
// memories removed from their scope's DB. A row marked for editing opens
// in the user's editor after the other decisions apply, then the screen
// opens again.
func (h *Host) openMemoryReview() {
	ui := h.hooks()
	if ui == nil {
		h.Notify("warn", "Memory review unavailable", "")
		return
	}
	for h.reviewMemoriesOnce(ui) {
	}
}

// reviewMemoriesOnce runs the screen once and reports whether a row was
// edited, so the caller shows it again.
func (h *Host) reviewMemoriesOnce(ui UIHooks) bool {
	rows := h.memoryRows()
	was := map[string]MemoryRow{}
	for _, r := range rows {
		was[r.ID] = r
	}
	var accepted, rejected, deleted, moved int
	edit := ""
	for _, r := range ui.ReviewMemories(rows) {
		old, ok := was[r.ID]
		if !ok {
			continue
		}
		if r.Edit {
			edit = r.ID
		}
		if r.Scope != "user" {
			r.Scope = "project"
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
				h.Notify("warn", "Not accepted", err.Error())
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
			h.Notify("warn", "Not moved", "the "+r.Scope+" scope already has that text")
		case old.Queued:
		case r.Action == "delete":
			if db := h.memoryDB(old.Scope); db != nil && db.DeleteMemory(r.ID) == nil {
				deleted++
			}
		case r.Scope != old.Scope:
			if err := h.moveMemory(r.ID, old.Scope, r.Scope); err != nil {
				h.Notify("warn", "Not moved", err.Error())
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
		h.Notify("info", "Memory updated", strings.Join(parts, ", "))
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
		h.Notify("warn", "Memory not changed", err.Error())
	case scrubbed:
		h.Notify("info", "Memory updated", "secrets redacted")
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
// text changes nothing, and so does a refusal.
func (h *Host) rewriteMemory(id, text string) (scrubbed bool, err error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	for _, p := range h.memoryProposals() {
		if p.ID != id || p.Forget {
			continue
		}
		res, err := h.storeMemory(h.AgentSessionID(), p.Scope, learn.OriginUser, text, p.Quote)
		if err != nil || res.Memory.Text == p.Text {
			return false, err
		}
		res.Memory.ID, res.Supersedes = p.ID, p.Supersedes
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
	res, err := h.storeMemory(h.AgentSessionID(), m.Scope, learn.OriginUser, text, m.Quote)
	if err != nil || res.Memory.Text == m.Text {
		return false, err
	}
	res.Memory.ID = m.ID
	if id, _, dup := h.duplicateMemory(res.Memory, ""); dup {
		return false, fmt.Errorf("%w as %s", errDuplicateMemory, learn.Truncate(id, 8))
	}
	return res.Scrubbed, db.UpdateMemoryText(m.ID, res.Memory.Text, learn.OriginUser)
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
func (h *Host) moveMemory(id, from, to string) error {
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
		if oldest, over := h.scopeOver(&learn.Result{Memory: *m}); over {
			return errScopeFull(to, oldest)
		}
		err = dst.AddMemory(m)
	case m.Origin == learn.OriginUser && live[i].Origin != learn.OriginUser:
		// the copy left behind keeps the user's attestation (DESIGN §10.4)
		err = dst.UpdateMemoryText(live[i].ID, live[i].Text, learn.OriginUser)
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
func (h *Host) acceptProposal(id, scope string) (*memoryProposal, error) {
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
		if oldest, over := h.scopeOver(res); over {
			err = errScopeFull(p.Memory.Scope, oldest)
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
		rows, err := db.Proposals("memory")
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
			what := ""
			if _, old, err := h.locateMemory(m.Supersedes); err == nil && old.Scope != m.Scope {
				what = old.Scope + " memory "
			}
			text += " (replaces " + what + learn.Truncate(m.Supersedes, 8) + ")"
		}
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Origin: m.Origin,
			Text: text, Quote: m.Quote, Queued: true, Forget: m.Forget,
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
	for _, scope := range []string{"project", "user"} {
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
		Name: "memory", NArgs: "*", Desc: "review what zemi remembers; /memory <note> keeps one",
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
		scope := cmp.Or(rawString(opts, "scope"), "project")
		if scope != "project" && scope != "user" {
			L.RaiseError("zemi.memory.add: scope must be project or user")
		}
		// user config writes the user's own memory; the model's Lua tool
		// runs as user origin but its notes are the model's
		memOrigin := learn.OriginLearned
		if tool, _ := h.modelLuaRun(L); !tool && o == lua.OriginUser {
			memOrigin = learn.OriginUser
		}
		res, err := h.proposeMemory(h.AgentSessionID(), memOrigin, scope, text, rawString(opts, "quote"), rawString(opts, "replaces"))
		if err != nil {
			return luaResult(L, err)
		}
		out := L.NewTable()
		switch {
		case res.ID != "":
			out.RawSetString("stored", glua.LTrue)
			out.RawSetString("id", glua.LString(res.ID))
		case res.Duplicate != "":
			out.RawSetString("duplicate", glua.LString(res.Duplicate))
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

	tb.RawSetString("forget", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		h.refuseBelowUser(L, "memory.forget")
		db, m, err := h.locateMemory(id)
		if err == nil {
			err = db.DeleteMemory(m.ID)
		}
		return luaResult(L, err)
	}))

	return tb
}

// proposeMemory runs a memory written in session sid (the memory tool,
// zemi.memory.add) through intake and the autonomy gate; replaces names
// the memory it supersedes. A subagent passes its parent, which a
// background run may outlive.
func (h *Host) proposeMemory(sid, origin, scope, text, quote, replaces string) (memoryOutcome, error) {
	if scope != "project" && scope != "user" {
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

// memoryTool is the memory tool from session sid: text supersedes what
// replaces names, empty text with replaces forgets it.
func (h *Host) memoryTool(sid, scope, text, quote, replaces string) (string, error) {
	if strings.TrimSpace(text) == "" {
		if replaces == "" {
			return "", errors.New("text is empty; pass replaces=<id> to drop a memory")
		}
		return h.forgetMemory(sid, replaces)
	}
	out, err := h.proposeMemory(sid, learn.OriginLearned, scope, text, quote, replaces)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// forgetMemory drops a memory for the model: one the user saved asks
// (Unsandboxed so no mode settles it), a learned one goes at once under
// auto autonomy and is proposed for review otherwise.
func (h *Host) forgetMemory(sid, id string) (string, error) {
	db, m, err := h.locateMemory(id)
	if err != nil {
		return "", err
	}
	short := m.ID[:8]
	switch {
	case m.Origin == learn.OriginUser:
		req := tools.Request{Tool: tools.ToolMemory, Subject: "memory.write", Arg: short, Reason: "forget a memory the user saved", Preview: "- " + m.Text, Unsandboxed: true}
		if v := h.PipelineFor().CheckDenied(req); v.Decision != perm.Allow {
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
	if id, scope, dup := h.duplicateMemory(*m, res.Supersedes); dup {
		return memoryOutcome{Duplicate: id, DupScope: scope}, nil
	}
	if m.Origin == learn.OriginLearned && m.Quote != "" && !h.quoteInSession(m.ProvenanceSessionID, m.Quote) {
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
	case m.Origin == learn.OriginLearned && m.Scope == "user" && m.Quote == "":
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

func errScopeFull(scope string, oldest []string) error {
	return fmt.Errorf("%s memory is full; /memory forget one of %s first", scope, strings.Join(oldest, ", "))
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
		if m.Role != "user" {
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

// sessionLocalOnly reports that neither session sid nor one of its subagents
// read text from outside the machine (fetch, web_search, a Lua or MCP tool,
// any tool zemi does not ship), where injected text would come from.
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
	return err == nil && (old.Origin == learn.OriginUser || old.Scope != res.Memory.Scope)
}

// autoApplyBudgetLeft caps what the live session stores without review;
// a closed session's writes (a detached reflector) are not counted.
func (h *Host) autoApplyBudgetLeft(sid string) bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return sid != ag.sessionID || ag.memAuto < memoryAutoApplyCap
}
