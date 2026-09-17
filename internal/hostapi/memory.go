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
	"strings"
	"time"

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
	ID     string
	Scope  string // "user" or "project"
	Kind   string
	Origin string // "user" or "learned"
	Text   string
	Tags   []string
	Queued bool   // proposal awaiting review, not yet stored
	Forget bool   // proposal to forget a stored memory, which stays in its scope
	Action string // set by the review screen: accept, reject, delete or ""
}

// memoryDB returns the store owning a scope: user scope in the global DB,
// project scope in the project DB (DESIGN §7).
func (h *Host) memoryDB(scope string) *store.DB {
	if scope == "user" {
		return h.GlobalDB
	}
	return h.DB
}

// storeMemory runs the §10.4 intake policy. Provenance is the live
// session; a user keystroke before the first send is its own provenance.
func (h *Host) storeMemory(scope, origin, kind, text string, tags []string) (*learn.Result, error) {
	prov := h.AgentSessionID()
	if prov == "" {
		if origin != learn.OriginUser {
			return nil, learn.ErrNoProvenance
		}
		prov = "user"
	}
	return learn.Intake(scope, origin, kind, text, tags, prov)
}

// queueProposal persists a policy-clean memory for review in its scope's
// store. The id is assigned here so review decisions travel by id.
func (h *Host) queueProposal(res *learn.Result) {
	if h.duplicateMemory(res.Memory) {
		h.Log.Debug("memory", "duplicate proposal dropped", nil)
		return
	}
	if err := h.addProposal(memoryProposal{Memory: res.Memory, Supersedes: res.Supersedes}); err != nil {
		h.Log.Warn("memory", "proposal dropped", map[string]any{"error": err.Error()})
	}
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
func (h *Host) queueForget(sid, id string) error {
	_, old, err := h.locateMemory(id)
	if err != nil {
		return err
	}
	for _, p := range h.memoryProposals() {
		if p.Forget && p.Supersedes == old.ID {
			return nil
		}
	}
	p := memoryProposal{Memory: *old, Supersedes: old.ID, Forget: true}
	p.ID, p.ProvenanceSessionID = "", sid
	return h.addProposal(p)
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

func redactNote(scrubbed bool) string {
	if scrubbed {
		return " (secrets redacted)"
	}
	return ""
}

var errDuplicateMemory = errors.New("already remembered")

// duplicateMemory reports whether a live memory or pending proposal in the
// same scope has the same text. Intake collapses whitespace, so a
// case-insensitive compare is the whole normalization.
// NOTE: scans the scope; fine for the hundreds of rows memories reach.
func (h *Host) duplicateMemory(m store.Memory) bool {
	db := h.memoryDB(m.Scope)
	if db == nil {
		return false
	}
	live, _ := db.ListMemory(m.Scope, false)
	for _, o := range live {
		if strings.EqualFold(o.Text, m.Text) {
			return true
		}
	}
	for _, o := range h.memoryProposals() {
		if o.Scope == m.Scope && strings.EqualFold(o.Text, m.Text) {
			return true
		}
	}
	return false
}

// storeProposal writes a policy-clean memory to its scope's DB.
func (h *Host) storeProposal(res *learn.Result) error {
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	m := &res.Memory
	if h.duplicateMemory(*m) {
		return errDuplicateMemory
	}
	if err := db.AddMemory(m); err != nil {
		return err
	}
	if res.Supersedes != "" {
		h.linkSupersede(res.Supersedes, *m)
	}
	h.EmitAsync("memory.written", map[string]any{
		"id": m.ID, "scope": m.Scope, "kind": m.Kind, "origin": m.Origin, "text": m.Text,
	})
	return nil
}

// Remember stores a user-typed /memory <note> text (origin user, project
// scope): the keystroke bypasses the autonomy gate (DESIGN §10.4).
func (h *Host) Remember(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("usage: /memory <note>")
	}
	res, err := h.storeMemory("project", learn.OriginUser, "fact", text, nil)
	if err != nil {
		return err
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
	case fields[0] == "supersede" && len(fields) == 3:
		return h.supersedeMemory(fields[1], fields[2])
	case fields[0] == "forget" || fields[0] == "supersede":
		return errors.New("usage: /memory forget <id> | /memory supersede <old> <new>")
	default:
		return h.Remember(args)
	}
}

// openMemoryReview shows pending proposals plus stored memories from both
// scopes in the Go-owned review overlay and applies the decisions by row
// id: accepted proposals are stored, rejected ones dropped, deleted
// memories removed from their scope's DB.
func (h *Host) openMemoryReview() {
	ui := h.hooks()
	if ui == nil {
		h.Notify("warn", "Memory review unavailable", "")
		return
	}
	rows := h.memoryRows()
	was := map[string]MemoryRow{}
	for _, r := range rows {
		was[r.ID] = r
	}
	var accepted, rejected, deleted, moved int
	for _, r := range ui.ReviewMemories(rows) {
		old, ok := was[r.ID]
		if !ok {
			continue
		}
		if r.Scope != "user" {
			r.Scope = "project"
		}
		switch {
		case old.Queued && r.Action == "reject":
			if h.takeProposal(r.ID) != nil {
				rejected++
			}
		case old.Queued && r.Action == "accept":
			if _, err := h.acceptProposal(r.ID, r.Scope); err != nil {
				h.Log.Warn("memory", "accept failed", map[string]any{"id": r.ID, "error": err.Error()})
				continue
			}
			accepted++
		case old.Queued && r.Scope != old.Scope && !old.Forget:
			if p := h.takeProposal(r.ID); p != nil {
				p.Memory.Scope = r.Scope
				h.queueProposal(&learn.Result{Memory: p.Memory, Supersedes: p.Supersedes})
				moved++
			}
		case old.Queued:
		case r.Action == "delete":
			if db := h.memoryDB(old.Scope); db != nil && db.DeleteMemory(r.ID) == nil {
				deleted++
			}
		case r.Scope != old.Scope:
			if err := h.moveMemory(r.ID, old.Scope, r.Scope); err != nil {
				h.Log.Warn("memory", "move failed", map[string]any{"id": r.ID, "error": err.Error()})
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
// old one; a failed add leaves it where it was.
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
	// a copy already there makes the move a merge
	if !h.duplicateMemory(*m) {
		if err := dst.AddMemory(m); err != nil {
			return err
		}
	}
	return src.DeleteMemory(id)
}

// acceptProposal stores a pending proposal (in scope when one is given), or deletes the memory a forget
// proposal names (one already gone counts as done, one rewritten since drops
// the proposal). A store failure other than a duplicate puts it back in the
// queue, so the decision can be made again.
func (h *Host) acceptProposal(id, scope string) (*memoryProposal, error) {
	p := h.takeProposal(id)
	if p == nil {
		return nil, fmt.Errorf("no such proposal %q", id)
	}
	var err error
	if p.Forget {
		db := h.memoryDB(p.Scope)
		var m *store.Memory
		if db == nil {
			err = errors.New("memory store unavailable")
		} else if m, err = db.GetMemory(p.Supersedes); err == nil && m.Text != p.Text {
			// the review showed the old text: forgetting the new one needs a new proposal
			return nil, errors.New("the memory changed since zemi proposed forgetting it")
		} else if err == nil {
			err = db.DeleteMemory(p.Supersedes)
		}
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
	} else {
		p.Memory.Scope = cmp.Or(scope, p.Memory.Scope)
		err = h.storeProposal(&learn.Result{Memory: p.Memory, Supersedes: p.Supersedes})
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
	Forget     bool   `json:",omitempty"`
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
			text += " (replaces " + learn.Truncate(m.Supersedes, 8) + ")"
		}
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Kind: m.Kind, Origin: m.Origin,
			Text: text, Tags: m.Tags, Queued: true, Forget: m.Forget,
		})
	}
	list, _ := h.memoryList(false)
	for _, m := range list {
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Kind: m.Kind, Origin: m.Origin,
			Text: m.Text, Tags: m.Tags,
		})
	}
	return rows
}

// locateMemory finds a memory id across both scopes and returns the DB
// owning it.
func (h *Host) locateMemory(id string) (*store.DB, *store.Memory, error) {
	if len(id) >= 6 && len(id) < 32 {
		// the digest and memory_search show 8-character id prefixes
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
			return nil, nil, fmt.Errorf("no such memory %q", id)
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
	return nil, nil, fmt.Errorf("no such memory %q", id)
}

// supersedeMemory resolves both ids (prefixes allowed) and links them; the
// replacement must live in the same scope.
func (h *Host) supersedeMemory(oldID, byID string) error {
	db, old, err := h.locateMemory(oldID)
	if err != nil {
		return err
	}
	byDB, by, err := h.locateMemory(byID)
	if err != nil {
		return err
	}
	if byDB != db || by.Scope != old.Scope {
		return fmt.Errorf("memory %s and %s are in different scopes", old.ID[:8], by.ID[:8])
	}
	return db.SupersedeMemory(old.ID, by.ID)
}

// memorySearch queries one or both scopes best-first; with bump every hit
// counts as a use in hit_count and last_hit_at (DESIGN §7).
func (h *Host) memorySearch(scope, query string, limit int, bump bool) ([]*store.Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	scopes := []string{scope}
	if scope == "all" || scope == "" {
		scopes = []string{"project", "user"}
	}
	var per [][]*store.Memory
	for _, s := range scopes {
		db := h.memoryDB(s)
		if db == nil {
			continue
		}
		rows, err := db.SearchMemory(s, query, limit, bump)
		if err != nil {
			return nil, err
		}
		per = append(per, rows)
	}
	// bm25 scores from two FTS indexes do not compare, so interleave by rank
	var out []*store.Memory
	for i := 0; len(out) < limit; i++ {
		n := len(out)
		for _, rows := range per {
			if i < len(rows) && len(out) < limit {
				out = append(out, rows[i])
			}
		}
		if len(out) == n {
			break
		}
	}
	return out, nil
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

// digestFor assembles the §10.7 memory digest across both scopes, ranked
// toward what query (the session's first message) is about. bm25 stays near
// zero for a word most memories hold, so matching only common words earns
// almost no boost. Pinning is not a use, so hits are not bumped.
func (h *Host) digestFor(query string) string {
	cands, _ := h.memoryList(false)
	lines := h.setInt("memory.digest_lines", 12)
	if lines < 0 {
		lines = 12
	}
	if strings.TrimSpace(query) != "" && len(cands) > lines {
		hits, err := h.memorySearch("all", query, 3*lines, false)
		if err != nil {
			h.Log.Warn("memory", "digest search failed", map[string]any{"error": err.Error()})
		}
		score := map[string]float64{}
		for _, m := range hits {
			score[m.ID] = 1 + min(m.Score, 2)*2
		}
		for _, m := range cands {
			m.Score = score[m.ID]
		}
	}
	return learn.Digest(cands, time.Now(), lines)
}

// MemoryCommandsStep registers the Go-owned /memory command with the
// builtin origin (user config may rebind it).
func (h *Host) MemoryCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "memory commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "memory", NArgs: "*", Desc: "review what zemi remembers",
				GoFn:   func(h *Host, args string) error { return h.memoryCommand(args) },
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

// ---------------------------------------------------------------------------
// zemi.memory namespace

func (h *Host) memoryNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	memoryToLua := func(L *glua.LState, rows []*store.Memory) glua.LValue {
		out := make([]any, 0, len(rows))
		for _, m := range rows {
			out = append(out, map[string]any{
				"id": m.ID, "scope": m.Scope, "kind": m.Kind, "origin": m.Origin,
				"text": m.Text, "score": m.Score, "tags": m.Tags,
			})
		}
		return lua.FromGo(L, out)
	}

	// search(query, opts): opts { scope = "all"|"user"|"project", limit }.
	// Every hit bumps hit_count (DESIGN §7).
	tb.RawSetString("search", L.NewFunction(func(L *glua.LState) int {
		query := L.CheckString(1)
		scope, limit := "all", 10
		if opts, ok := L.Get(2).(*glua.LTable); ok {
			if s := rawString(opts, "scope"); s != "" {
				scope = s
			}
			if n, ok := opts.RawGetString("limit").(glua.LNumber); ok && int(n) > 0 {
				limit = int(n)
			}
		}
		if scope != "all" && scope != "user" && scope != "project" {
			L.RaiseError("zemi.memory.search: scope must be all, user or project")
		}
		rows, err := h.memorySearch(scope, query, limit, true)
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(memoryToLua(L, rows))
		return 1
	}))

	// add(spec): the model/Lua path (origin learned, §10.4 intake, §10.4
	// autonomy gate). Returns { queued = true }, { stored = true, id } or nil, err.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.memory.add: spec must be a table")
		}
		if o := h.RT.CurrentOrigin(L); o.Rank() <= lua.OriginProject.Rank() {
			L.RaiseError("zemi.memory.add: %s code may not add memories", o)
		}
		kind := rawString(spec, "kind")
		text := rawString(spec, "text")
		scope := rawString(spec, "scope")
		if scope == "" {
			scope = "project"
		}
		stored, id, err := h.proposeMemory(scope, kind, text, rawStrings(spec, "tags"))
		switch {
		case errors.Is(err, learn.ErrBadKind) || scope != "project" && scope != "user":
			L.RaiseError("zemi.memory.add: %v", err)
		case err != nil:
			return luaResult(L, err)
		}
		out := L.NewTable()
		if stored {
			out.RawSetString("stored", glua.LTrue)
			out.RawSetString("id", glua.LString(id))
		} else {
			out.RawSetString("queued", glua.LTrue)
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

	tb.RawSetString("supersede", L.NewFunction(func(L *glua.LState) int {
		old, by := L.CheckString(1), L.CheckString(2)
		h.refuseBelowUser(L, "memory.supersede")
		return luaResult(L, h.supersedeMemory(old, by))
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

// proposeMemory runs a learned memory (zemi.memory.add, the memory_add
// tool) through intake and the autonomy gate.
func (h *Host) proposeMemory(scope, kind, text string, tags []string) (stored bool, id string, err error) {
	if scope != "project" && scope != "user" {
		return false, "", errors.New("scope must be project or user")
	}
	res, err := h.storeMemory(scope, learn.OriginLearned, kind, text, tags)
	if err != nil {
		return false, "", err
	}
	stored, err = h.gateMemory(res)
	return stored, res.Memory.ID, err
}

// modelEditMemory is memory_update (text set) and memory_forget (text "").
// The change applies at once only under auto autonomy, to a learned memory,
// with text that is not instruction-shaped; otherwise it asks, so an edit
// cannot slip past the review a new memory would get.
func (h *Host) modelEditMemory(id, text string) (string, error) {
	db, m, err := h.locateMemory(id)
	if err != nil {
		return "", err
	}
	verb, preview, shaped := "delete", "- "+m.Text, false
	if text != "" {
		prov := h.AgentSessionID()
		if prov == "" {
			prov = m.ProvenanceSessionID
		}
		// the new text is the model's, whoever wrote the old one
		res, err := learn.Intake(m.Scope, learn.OriginLearned, m.Kind, text, m.Tags, prov)
		if err != nil {
			return "", err
		}
		text, verb, shaped = res.Memory.Text, "update", res.InstructionShaped
		preview += "\n+ " + text
	}
	if !h.memoryAuto(h.AgentSessionID()) || m.Origin != learn.OriginLearned || shaped {
		req := tools.Request{Tool: "memory_" + verb, Subject: "memory.write", Arg: m.ID[:8], Reason: verb + " memory", Preview: preview}
		if v := h.PipelineFor().Check(req); v.Decision != perm.Allow {
			return "", fmt.Errorf("permission denied (%s): %s memory %s", v.DecidedBy, verb, m.ID[:8])
		}
	}
	if text == "" {
		err = db.DeleteMemory(m.ID)
	} else {
		err = db.UpdateMemoryText(m.ID, text, learn.OriginLearned)
	}
	if err != nil {
		return "", err
	}
	return "memory " + m.ID[:8] + " " + verb + "d", nil
}

// gateMemory is the autonomy gate (DESIGN §10.4): an intake result is
// stored under auto when policy-clean and within budget, queued otherwise.
func (h *Host) gateMemory(res *learn.Result) (stored bool, err error) {
	if h.memoryAuto(res.Memory.ProvenanceSessionID) && !res.InstructionShaped && !h.replacesUserMemory(res.Supersedes) && h.autoApplyBudgetLeft() {
		return true, h.applyProposal(res)
	}
	h.queueProposal(res)
	return false, nil
}

// memoryAuto reports whether auto autonomy may skip review for what session
// sid wrote. It may not in an untrusted project, or when the session or one
// of its subagents read text from outside the machine (fetch, web_search, an
// MCP tool), where an injected memory would come from.
// NOTE: a bash command the user approved can fetch too and is not seen here.
func (h *Host) memoryAuto(sid string) bool {
	if h.setStr("memory.autonomy") != "auto" || !h.ProjectTrusted() || h.DB == nil {
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
	for _, id := range sids {
		stats, err := h.DB.ToolCallStats(id)
		if err != nil {
			return false
		}
		for _, s := range stats {
			if s.Tool == "fetch" || s.Tool == "web_search" || strings.HasPrefix(s.Tool, "mcp_") {
				return false
			}
		}
	}
	return true
}

// replacesUserMemory reports whether id names a user-attested memory, which
// a learned one may replace only through review (DESIGN §10.4).
func (h *Host) replacesUserMemory(id string) bool {
	if id == "" {
		return false
	}
	_, old, err := h.locateMemory(id)
	return err == nil && old.Origin == learn.OriginUser
}

func (h *Host) autoApplyBudgetLeft() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.memAuto < memoryAutoApplyCap
}
