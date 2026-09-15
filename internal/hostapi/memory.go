// memory.go is the M2 memory host API (DESIGN §6.7, §7, §10.4): the
// zemi.memory namespace, the Go-owned :remember/:memory commands, the
// session-scoped proposal queue behind the autonomy gate, and the §10.7
// digest. The proposal queue lives in memory on purpose: proposals not
// reviewed before shutdown are simply not stored.
package hostapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
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
	Queued bool // proposal awaiting review, not yet stored
}

func (h *Host) memoryAutonomy() string {
	if v, ok := h.Settings.Get("memory.autonomy"); ok {
		if s, ok := v.(string); ok && s == "auto" {
			return "auto"
		}
	}
	return "ask"
}

func (h *Host) memoryDigestLines() int {
	if v, ok := h.Settings.Get("memory.digest_lines"); ok {
		if n, ok := v.(int); ok && n >= 0 {
			return n
		}
	}
	return 12
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

// memNotify is a nil-safe toast.
func (h *Host) memNotify(level, title, body string) {
	if ui := h.hooks(); ui != nil {
		ui.Notify(level, title, body)
	}
}

// queueProposal parks a policy-clean memory for review. The id is
// assigned at queue time so the review overlay can drop the proposal
// unambiguously.
func (h *Host) queueProposal(res *learn.Result) {
	if res.Memory.ID == "" {
		res.Memory.ID = store.NewID()
	}
	ag := h.agents
	ag.mu.Lock()
	ag.memProposals = append(ag.memProposals, res)
	ag.mu.Unlock()
	h.memNotify("info", "memory", "proposal queued for review: "+res.Memory.Text)
}

// applyProposal stores a proposal and spends one unit of the per-session
// auto-apply budget.
func (h *Host) applyProposal(res *learn.Result) error {
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	if err := db.AddMemory(&res.Memory); err != nil {
		return err
	}
	ag := h.agents
	ag.mu.Lock()
	ag.memAuto++
	ag.mu.Unlock()
	note := ""
	if res.Scrubbed {
		note = " (secrets redacted)"
	}
	h.memNotify("info", "memory", "stored: "+res.Memory.Text+note)
	return nil
}

// Remember stores a user-typed :remember text (origin user, project
// scope): the keystroke bypasses the autonomy gate (DESIGN §10.4).
func (h *Host) Remember(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("usage: :remember <text>")
	}
	res, err := h.storeMemory("project", learn.OriginUser, "fact", text, nil)
	if err != nil {
		return err
	}
	db := h.memoryDB(res.Memory.Scope)
	if db == nil {
		return errors.New("memory store unavailable")
	}
	if err := db.AddMemory(&res.Memory); err != nil {
		return err
	}
	note := ""
	if res.Scrubbed {
		note = " (secrets redacted)"
	}
	h.memNotify("info", "remember", "stored"+note)
	return nil
}

// memoryCommand dispatches :memory; no args opens the review overlay on
// its own goroutine so the UI goroutine never blocks on the hook.
func (h *Host) memoryCommand(args string) error {
	fields := strings.Fields(args)
	switch {
	case len(fields) == 0:
		if h.memReview.TryLock() {
			go func() {
				defer h.memReview.Unlock()
				h.openMemoryReview()
			}()
			return nil
		}
		return errors.New("memory review already open")
	case fields[0] == "forget" && len(fields) == 2:
		db, _, err := h.locateMemory(fields[1])
		if err != nil {
			return err
		}
		return db.DeleteMemory(fields[1])
	case fields[0] == "supersede" && len(fields) == 3:
		db, _, err := h.locateMemory(fields[1])
		if err != nil {
			return err
		}
		return db.SupersedeMemory(fields[1], fields[2])
	default:
		return errors.New("usage: :memory | :memory supersede <old> <new> | :memory forget <id>")
	}
}

// openMemoryReview shows queued proposals plus live memories from both
// scopes in the Go-owned review overlay and applies the returned
// deletions: queued proposals are dropped, stored memories are deleted
// from their scope's DB.
func (h *Host) openMemoryReview() {
	ui := h.hooks()
	if ui == nil {
		h.memNotify("warn", "memory", "review UI unavailable")
		return
	}
	rows := h.memoryRows()
	if len(rows) == 0 {
		h.memNotify("info", "memory", "no memories")
		return
	}
	del := ui.ReviewMemories(rows)
	removed := 0
	for _, i := range del {
		if i < 0 || i >= len(rows) {
			continue
		}
		r := rows[i]
		if r.Queued {
			h.dropProposal(r.ID)
			removed++
			continue
		}
		if db := h.memoryDB(r.Scope); db != nil {
			if err := db.DeleteMemory(r.ID); err != nil {
				h.Log.Warn("memory", "delete failed", map[string]any{"id": r.ID, "error": err.Error()})
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		h.memNotify("info", "memory", fmt.Sprintf("removed %d", removed))
	}
}

func (h *Host) dropProposal(id string) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	for i, p := range ag.memProposals {
		if p.Memory.ID == id {
			ag.memProposals = append(ag.memProposals[:i], ag.memProposals[i+1:]...)
			return
		}
	}
}

// memoryRows merges queued proposals (first) and live memories from both
// scopes, newest first within a scope.
func (h *Host) memoryRows() []MemoryRow {
	var rows []MemoryRow
	ag := h.agents
	ag.mu.Lock()
	for _, p := range ag.memProposals {
		m := p.Memory
		rows = append(rows, MemoryRow{
			ID: m.ID, Scope: m.Scope, Kind: m.Kind, Origin: m.Origin,
			Text: m.Text, Tags: m.Tags, Queued: true,
		})
	}
	ag.mu.Unlock()
	appendScope := func(db *store.DB, scope string) {
		list, err := db.ListMemory(scope, false)
		if err != nil {
			h.Log.Warn("memory", "list failed", map[string]any{"scope": scope, "error": err.Error()})
			return
		}
		for _, m := range list {
			rows = append(rows, MemoryRow{
				ID: m.ID, Scope: m.Scope, Kind: m.Kind, Origin: m.Origin,
				Text: m.Text, Tags: m.Tags,
			})
		}
	}
	if h.DB != nil {
		appendScope(h.DB, "project")
	}
	if h.GlobalDB != nil {
		appendScope(h.GlobalDB, "user")
	}
	return rows
}

// locateMemory finds a memory id across both scopes and returns the DB
// owning it.
func (h *Host) locateMemory(id string) (*store.DB, *store.Memory, error) {
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

// memorySearch queries one or both scopes best-first; every hit bumps
// hit_count and last_hit_at (DESIGN §7).
func (h *Host) memorySearch(scope, query string, limit int) ([]*store.Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	scopes := []string{scope}
	if scope == "all" || scope == "" {
		scopes = []string{"project", "user"}
	}
	var out []*store.Memory
	for _, s := range scopes {
		db := h.memoryDB(s)
		if db == nil {
			continue
		}
		rows, err := db.SearchMemory(s, query, limit, true)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// memoryList merges both scopes newest-first.
func (h *Host) memoryList(includeSuperseded bool) ([]*store.Memory, error) {
	var out []*store.Memory
	if h.DB != nil {
		rows, err := h.DB.ListMemory("project", includeSuperseded)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if h.GlobalDB != nil {
		rows, err := h.GlobalDB.ListMemory("user", includeSuperseded)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// digestFor assembles the §10.7 memory digest across both scopes.
func (h *Host) digestFor() string {
	var cands []*store.Memory
	for _, pair := range []struct {
		db    *store.DB
		scope string
	}{{h.DB, "project"}, {h.GlobalDB, "user"}} {
		if pair.db == nil {
			continue
		}
		rows, err := pair.db.ListMemory(pair.scope, false)
		if err != nil {
			h.Log.Warn("memory", "digest list failed", map[string]any{
				"scope": pair.scope, "error": err.Error(),
			})
			continue
		}
		cands = append(cands, rows...)
	}
	return learn.Digest(cands, time.Now(), h.memoryDigestLines())
}

// MemoryCommandsStep registers the Go-owned :remember and :memory
// commands with the builtin origin (user config may rebind them).
func (h *Host) MemoryCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "memory commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			for _, c := range []*Command{
				{Name: "remember", NArgs: "+", Desc: "remember a fact for this project",
					GoFn: func(h *Host, args string) error { return h.Remember(args) },
					Origin: lua.OriginBuiltin, Gen: Gen(g.ID)},
				{Name: "memory", Desc: "review memories (:memory supersede <old> <new> | forget <id>)",
					GoFn: func(h *Host, args string) error { return h.memoryCommand(args) },
					Origin: lua.OriginBuiltin, Gen: Gen(g.ID)},
			} {
				if _, err := h.Commands.Add(c); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// zemi.memory namespace

func memoryManageAllowed(o lua.Origin) bool {
	return o == lua.OriginUser || o == lua.OriginBuiltin
}

func (h *Host) memoryNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	memoryToLua := func(rows []*store.Memory) *glua.LTable {
		out := L.NewTable()
		for _, m := range rows {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(m.ID))
			row.RawSetString("scope", glua.LString(m.Scope))
			row.RawSetString("kind", glua.LString(m.Kind))
			row.RawSetString("origin", glua.LString(m.Origin))
			row.RawSetString("text", glua.LString(m.Text))
			row.RawSetString("score", glua.LNumber(m.Score))
			tags := L.NewTable()
			for _, t := range m.Tags {
				tags.Append(glua.LString(t))
			}
			row.RawSetString("tags", tags)
			out.Append(row)
		}
		return out
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
			return 0
		}
		rows, err := h.memorySearch(scope, query, limit)
		if err != nil {
			L.RaiseError("zemi.memory.search: %v", err)
			return 0
		}
		L.Push(memoryToLua(rows))
		return 1
	}))

	// add(spec): the model/Lua path (origin learned, §10.4 intake, §10.4
	// autonomy gate). Returns { queued = true } or { stored = true, id }.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.memory.add: spec must be a table")
			return 0
		}
		kind := rawString(spec, "kind")
		text := rawString(spec, "text")
		scope := rawString(spec, "scope")
		if scope == "" {
			scope = "project"
		}
		if scope != "project" && scope != "user" {
			L.RaiseError("zemi.memory.add: scope must be project or user")
			return 0
		}
		var tags []string
		if lt, ok := spec.RawGetString("tags").(*glua.LTable); ok {
			lt.ForEach(func(_, v glua.LValue) {
				if s, ok := v.(glua.LString); ok {
					tags = append(tags, string(s))
				}
			})
		}
		res, err := h.storeMemory(scope, learn.OriginLearned, kind, text, tags)
		if err != nil {
			L.RaiseError("zemi.memory.add: %v", err)
			return 0
		}
		out := L.NewTable()
		if h.memoryAutonomy() == "auto" && !res.InstructionShaped && h.autoApplyBudgetLeft() {
			if err := h.applyProposal(res); err != nil {
				L.RaiseError("zemi.memory.add: %v", err)
				return 0
			}
			out.RawSetString("stored", glua.LTrue)
			out.RawSetString("id", glua.LString(res.Memory.ID))
		} else {
			h.queueProposal(res)
			out.RawSetString("queued", glua.LTrue)
		}
		L.Push(out)
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows, err := h.memoryList(L.OptBool(1, false))
		if err != nil {
			L.RaiseError("zemi.memory.list: %v", err)
			return 0
		}
		L.Push(memoryToLua(rows))
		return 1
	}))

	tb.RawSetString("supersede", L.NewFunction(func(L *glua.LState) int {
		old, by := L.CheckString(1), L.CheckString(2)
		if !memoryManageAllowed(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.memory.supersede: only user or builtin origin may modify memories")
			return 0
		}
		db, _, err := h.locateMemory(old)
		if err != nil {
			L.RaiseError("zemi.memory.supersede: %v", err)
			return 0
		}
		if _, err := db.GetMemory(by); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				L.RaiseError("zemi.memory.supersede: no such memory %q", by)
			} else {
				L.RaiseError("zemi.memory.supersede: %v", err)
			}
			return 0
		}
		if err := db.SupersedeMemory(old, by); err != nil {
			L.RaiseError("zemi.memory.supersede: %v", err)
			return 0
		}
		return 0
	}))

	tb.RawSetString("forget", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		if !memoryManageAllowed(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.memory.forget: only user or builtin origin may modify memories")
			return 0
		}
		db, _, err := h.locateMemory(id)
		if err != nil {
			L.RaiseError("zemi.memory.forget: %v", err)
			return 0
		}
		if err := db.DeleteMemory(id); err != nil {
			L.RaiseError("zemi.memory.forget: %v", err)
			return 0
		}
		return 0
	}))

	return tb
}

func (h *Host) autoApplyBudgetLeft() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.memAuto < memoryAutoApplyCap
}
