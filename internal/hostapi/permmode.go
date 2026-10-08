package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// PermModeDef is one permission mode. Base picks the behavior: auto
// (deterministic local checks answer routine asks) or yolo (every ask
// allowed). A read-only profile denies writes and confines commands on its own,
// whatever the mode (Profile.ReadOnly). Every mode only answers asks:
// deny rules and the tool.pre/permission.request vetoes still run first.
// Rules answer asks before the base does, and Prompt joins the system
// prompt while the mode is on.
type PermModeDef struct {
	Name, Base, Description, Prompt string
	Rules                           *perm.Store
	// Decide answers an ask the rules left open: "allow", "deny" or
	// anything else to leave it to the user. An ask copies the def out of
	// the list before it awaits, so the function outlives a purge and Gen
	// is what refuses it (0 for a builtin, which has no decide).
	Decide *glua.LFunction
	Origin lua.Origin
	// Path is the file that registered the mode: it tells two plugins' modes
	// of one name apart (mayReplace), and a builtin has none.
	Path string
	Gen  Gen
}

var builtinModes = []PermModeDef{
	{Name: config.PermModeAuto, Base: config.PermModeAuto, Description: config.PermModeAutoDesc},
	{Name: config.PermModeYolo, Base: config.PermModeYolo, Description: config.PermModeYoloDesc},
}

// modeReg is the mode list in cycle order: the builtins until user config
// changes it, reset on reload so init.lua replays onto a clean list.
type modeReg struct {
	mu             sync.Mutex
	list           []PermModeDef // nil = builtinModes
	prePlanMode    string        // the mode active before a read-only agent was picked
	prePlanProfile string        // the agent active before a read-only agent was picked

	headless []Denial // asks denied because no UI could answer them
}

// Denial is an ask nobody could answer because no UI runs (zemi run), any
// pipeline's: subagents and workflows run no rows the main transcript shows.
// Arg and Reason are sanitized.
type Denial struct {
	Tool    string    `json:"tool"`
	Subject string    `json:"subject"`
	Arg     string    `json:"arg"`
	Reason  string    `json:"reason"`
	Why     DenialWhy `json:"why"`
}

// DenialWhy is what a denied ask needed.
type DenialWhy string

const (
	DenyApproval       DenialWhy = "approval"        // the mode left it to the user
	DenyOutsideSandbox DenialWhy = "outside_sandbox" // a command asked to leave the sandbox
	DenyNetwork        DenialWhy = "network"         // a command asked for sockets
	DenyNeedsTUI       DenialWhy = "needs_tui"       // a Lua script or a memory edit
)

func (r *modeReg) noteHeadless(d Denial) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headless = append(r.headless, d)
}

// HeadlessDenials is every ask denied so far; zemi run reads the ones past
// its start.
func (h *Host) HeadlessDenials() []Denial {
	h.modes.mu.Lock()
	defer h.modes.mu.Unlock()
	return slices.Clip(h.modes.headless)
}

func (h *Host) PermModeList() []PermModeDef {
	h.modes.mu.Lock()
	defer h.modes.mu.Unlock()
	if h.modes.list == nil {
		return builtinModes
	}
	return slices.Clone(h.modes.list)
}

func (h *Host) setModes(fn func([]PermModeDef) ([]PermModeDef, error)) error {
	h.modes.mu.Lock()
	defer h.modes.mu.Unlock()
	cur := h.modes.list
	if cur == nil {
		cur = slices.Clone(builtinModes)
	}
	next, err := fn(cur)
	if err != nil {
		return err
	}
	h.modes.list = next
	return nil
}

func modeIndex(list []PermModeDef, name string) int {
	return slices.IndexFunc(list, func(d PermModeDef) bool { return d.Name == name })
}

// OpenModePicker lists the modes with what they mean; enter switches.
func (h *Host) OpenModePicker() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	cur, sel := h.PermMode(), 0
	var items []any
	for i, m := range h.PermModeList() {
		item := map[string]any{"text": m.Name, "desc": m.Description, "run": "mode " + m.Name}
		if m.Name == cur {
			item["key"], sel = "current", i
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "mode", Title: "Permission mode", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: OverlayBottomLeft, Verb: "pick"})
	return nil
}

// OpenPermissions lists the active mode and every rule in force. An
// "always" answer is the only rule a user can write at runtime and the only
// one with no file to edit, so a mis-keyed one must be visible and
// removable; config rules come back on the next reload, so they show
// read-only.
func (h *Host) OpenPermissions() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(Overlay{
		ID: "permissions", Title: "Permissions", Origin: lua.OriginBuiltin,
		Items:  h.permItems(),
		Reload: func() ([]any, [][]Segment) { return h.permItems(), nil },
	})
	return nil
}

// permRank orders the list the way the engine reads it: deny, ask, allow.
var permRank = map[perm.Decision]int{perm.Deny: 0, perm.Ask: 1, perm.Allow: 2}

func (h *Host) permItems() []any {
	mode := h.permModeDef()
	items := []any{map[string]any{
		"text": mode.Name, "desc": mode.Description,
		"section": "Mode", "actions": []any{map[string]any{"text": "Change", "hotkey": "c", "run": "mode"}},
	}}
	rules := h.Perm.Rules()
	slices.SortStableFunc(rules, func(a, b perm.Rule) int {
		return cmp.Or(cmp.Compare(permRank[a.Decision], permRank[b.Decision]), cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Pattern, b.Pattern))
	})
	answers := map[string]map[string]any{} // one row per always answer
	for _, r := range rules {
		desc := ""
		if r.NotMatch != "" {
			desc = "except " + r.NotMatch
		}
		if r.Network {
			desc = "sandboxed, with the network"
		}
		item := map[string]any{
			"text": r.Subject + " " + r.Pattern, "desc": desc,
			"section": sourceSection(r.Decision) + " rules",
		}
		if r.Principal == perm.PrincipalModel {
			// an answer writes p and "p *" (a host and its paths): one row,
			// named by the pattern that covers the other, removes both
			id := r.Subject + " " + answerKey(r.Pattern)
			// the copied lines are wider than the answer: a config allow skips the
			// secret re-ask a model rule keeps, and a network one would run unconfined
			line := allowLine(r.Subject, r.Pattern, strings.ReplaceAll(r.Pattern, "*", "..."), "")
			if row := answers[id]; row != nil {
				row["text"] = r.Subject + " " + r.Pattern
				if prev, ok := row["copy"].(string); ok {
					row["copy"] = prev + "\n" + line
				}
				continue
			}
			answers[id] = item
			forget := "permissions forget " + id
			item["key"] = "session"
			item["actions"] = []any{map[string]any{"text": "Delete", "hotkey": "x", "run": forget}}
			item["detail"] = "an always answer this session; deleting it makes the next call ask again"
			if !r.Network {
				item["copy"] = line
				item["detail"] = item["detail"].(string) + ". y copies it as init.lua lines, which also allow the secret-shaped files this answer still asks about"
			}
		} else {
			if _, ok := perm.CutPluginPrincipal(r.Principal); ok {
				item["key"] = r.Principal // it covers that plugin's calls alone
			}
			switch r.Source {
			case "":
				item["detail"] = "from config; edit permissions in your init.lua"
			case "permissions":
				item["detail"] = "from " + perm.TildePath(h.projectPermissionsPath()) + "; edit that file"
			case "project":
				item["detail"] = "from the project's .agents/init.lua"
			default:
				item["detail"] = "from " + r.Source + " config"
			}
		}
		items = append(items, item)
	}
	return items
}

// answerKey is a session rule's pattern without the " *" or "/*" an always
// answer adds to the pattern it also writes bare.
func answerKey(pattern string) string {
	return strings.TrimSuffix(strings.TrimSuffix(pattern, " *"), "/*")
}

// ForgetRule drops the session rules an "always" answer wrote for one
// subject and pattern, the bare and the wildcard form alike. Config rules
// stay: a reload would bring them back.
func (h *Host) ForgetRule(arg string) error {
	subject, pattern, _ := strings.Cut(strings.TrimSpace(arg), " ")
	gone := false
	for _, r := range h.Perm.Rules() {
		if r.Principal == perm.PrincipalModel && r.Subject == subject && answerKey(r.Pattern) == answerKey(pattern) {
			h.Perm.Remove(r)
			gone = true
		}
	}
	if !gone {
		return Usagef("no session rule allows %s %s", subject, pattern)
	}
	h.Log.Info("session rule removed", "origin", "permissions", "subject", subject, "pattern", pattern)
	h.refresh()
	return nil
}

// PermMode reports the active permission mode. An unknown value reads as
// the default, auto.
func (h *Host) PermMode() string { return h.permModeDef().Name }

func (h *Host) permModeDef() PermModeDef {
	list := h.PermModeList()
	if i := modeIndex(list, h.Settings.Str("permissions.mode")); i >= 0 {
		return list[i]
	}
	for _, d := range list {
		if d.Base == config.PermModeAuto {
			return d
		}
	}
	// never list[0]: user config builds the list, and one holding no
	// auto-based mode must not fall back on what allows everything
	return builtinModes[0]
}

// SetPermMode switches the mode at runtime (/mode, ctrl+y).
func (h *Host) SetPermMode(mode string) error {
	list := h.PermModeList()
	i := modeIndex(list, mode)
	if i < 0 {
		names := make([]string, len(list))
		for j, d := range list {
			names[j] = d.Name
		}
		return Usagef("unknown permission mode %q (want %s)", mode, strings.Join(names, ", "))
	}
	return h.setWatched("permissions.mode", mode)
}

// dropModePick forgets a kept permissions.mode pick that named a mode whose
// base just turned yolo. keepPick refuses to store a new one, and loadPicks
// drops a literal yolo row, both before any Lua runs; a pick made while the
// mode was auto-based would otherwise start every later boot allowing what
// the rules do not deny. Registration is where the base becomes known, so a
// fresh boot re-checks too, not only a replay.
func (h *Host) dropModePick(def PermModeDef) {
	if def.Base != config.PermModeYolo {
		return
	}
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	pick, _ := h.picked["permissions.mode"].(string)
	if pick != def.Name {
		return
	}
	delete(h.picked, "permissions.mode")
	if h.GlobalDB != nil {
		if _, err := h.GlobalDB.DelKV("config:permissions.mode"); err != nil {
			h.Log.Warn("could not drop a kept pick", "origin", "config", "path", "permissions.mode", "error", err)
		}
	}
	// the setting goes back to its default only when the pick is what set it:
	// init.lua writing the mode is a choice of its own
	if live, _ := h.Settings.Get("permissions.mode"); live == any(pick) {
		h.Settings.SetQuiet("permissions.mode", h.Settings.Default("permissions.mode"))
	}
}

// approvePlan is the plan tool's hook: it shows a plan in the approval popup.
// The plan joins the transcript, then the user approves it, keeps it without
// starting, types what to change, or dismisses it; the file is written once
// the verdict is in, so a dismissed plan cannot overwrite the session's own.
// A read-only agent (planner) remembers what ran before it (prePlan), so any
// approval returns to that agent and mode, yolo included; an approval that
// starts the work records an implementation handoff (planApproved) that the
// ended turn passes on to planFollowup, and a hold skips that handoff.
func (h *Host) approvePlan(plan string) (tools.PlanVerdict, bool) {
	ui := h.hooks()
	if ui == nil {
		return tools.PlanVerdict{}, false
	}
	var v tools.PlanVerdict
	h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: sanitize.String(plan)})
	restore, mode := "", ""
	if h.readOnly() {
		h.modes.mu.Lock()
		restore, mode = h.modes.prePlanProfile, h.modes.prePlanMode
		h.modes.mu.Unlock()
		if restore == "" { // a read-only agent picked from config: the shipped profile, the mode as it is
			restore = "default"
		}
		v.Profile = restore
	}
	verdict, ok := ui.AskPlan(sanitize.String(plan), restore)
	if !ok {
		return v, false
	}
	if !verdict.Approved {
		v.Feedback = verdict.Feedback
		return v, true
	}
	if path := h.planPath(h.AgentSessionID()); path != "" {
		if os.MkdirAll(filepath.Dir(path), 0o700) == nil && os.WriteFile(path, []byte(plan+"\n"), 0o600) == nil {
			v.Path = path
		}
	}
	if restore != "" {
		// a profile or mode the config dropped since is no reason to read the
		// approval as a dismissal: the fallbacks (default, auto) stand
		if err := h.SetProfile(restore); err != nil {
			h.Log.Info("plan restore skipped a profile", "origin", "permissions", "profile", restore, "error", err)
		}
		if mode != "" {
			if err := h.SetPermMode(mode); err != nil {
				h.Log.Info("plan restore skipped a mode", "origin", "permissions", "mode", mode, "error", err)
			}
		}
		if !verdict.Hold {
			// the read-only turn cannot implement the plan itself; the turn-end
			// hook runs it as the restored agent
			h.setPlanApproved(implementNow)
		}
	}
	v.Approved, v.Hold = true, verdict.Hold
	return v, true
}

// cyclePermMode steps through the modes (the mode next and prev actions).
func (h *Host) cyclePermMode(step int) error {
	list := h.PermModeList()
	n := len(list)
	i := modeIndex(list, h.PermMode())
	return h.SetPermMode(list[((i+step)%n+n)%n].Name)
}

var modeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// modeNamespace is zemi.permission.mode: add and del register modes from
// user, builtin or plugin code (a mode answers permission asks, so the
// project may not); only user config switches the active one.
func (h *Host) modeNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	gate := func(L *glua.LState, fn string) {
		if o := h.RT.CurrentOrigin(L); o.Rank() > lua.OriginProject.Rank() {
			return
		}
		L.RaiseError("zemi.permission.mode.%s: project code may not register a permission mode", fn)
	}
	// add(name, { base, description, prompt, rules = { { subject, pattern, decision }, ... } })
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, spec := L.CheckString(1), L.CheckTable(2)
		gate(L, "add")
		def := PermModeDef{
			Name: name, Base: rawString(L, spec, "base"), Description: rawString(L, spec, "description"), Prompt: rawString(L, spec, "prompt"),
			Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L), Gen: h.currentGen(),
		}
		def.Decide, _ = opt[*glua.LFunction](L, spec, "decide")
		if !modeName.MatchString(name) {
			L.RaiseError("zemi.permission.mode.add: name %q must match [a-z][a-z0-9-]{0,15}", name)
		}
		if modeIndex(builtinModes, def.Base) < 0 {
			L.RaiseError("zemi.permission.mode.add: base must be auto or yolo")
		}
		// the builtins carry no origin, so their names are reserved by name:
		// auto and yolo are what settles every ask nobody answered otherwise
		if modeIndex(builtinModes, name) >= 0 {
			L.RaiseError("zemi.permission.mode.add: %q is a builtin mode", name)
		}
		if rules, ok := opt[*glua.LTable](L, spec, "rules"); ok {
			def.Rules = new(perm.Store)
			var bad string
			rules.ForEach(func(_, v glua.LValue) {
				rt, ok := v.(*glua.LTable)
				if !ok {
					bad = "each rule must be a table"
					return
				}
				field := func(i int, key string) string {
					if s := rawString(L, rt, key); s != "" {
						return s
					}
					s, _ := rt.RawGetInt(i).(glua.LString)
					return string(s)
				}
				r := perm.Rule{Subject: field(1, "subject"), Pattern: field(2, "pattern"), Decision: perm.Decision(field(3, "decision"))}
				switch err := h.ruleShape(r); {
				case err != nil:
					bad = err.Error()
				case r.Decision == perm.Allow && def.Origin != lua.OriginUser:
					// a mode's rules match every principal (ask.go), so its allow
					// widens as far as rule.add's: user config alone writes one
					bad = "allow rules may only come from user config"
				default:
					def.Rules.Add(r)
				}
			})
			if bad != "" {
				L.RaiseError("zemi.permission.mode.add: %s", bad)
			}
		}
		if def.Description == "" {
			def.Description = "like " + def.Base
		}
		if err := h.setModes(func(list []PermModeDef) ([]PermModeDef, error) {
			if i := modeIndex(list, name); i >= 0 {
				if err := mayReplace(h.PluginOf, "permission mode "+name, list[i].Origin, list[i].Path, def.Origin, def.Path); err != nil {
					return nil, err
				}
				list[i] = def
				return list, nil
			}
			return append(list, def), nil
		}); err != nil {
			L.RaiseError("zemi.permission.mode.add: %v", err)
		}
		h.dropModePick(def)
		h.refresh()
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		gate(L, "del")
		by, byPath := h.RT.CurrentOrigin(L), h.RT.CurrentPath(L)
		if modeIndex(builtinModes, name) >= 0 {
			L.RaiseError("zemi.permission.mode.del: %q is a builtin mode", name)
		}
		err := h.setModes(func(list []PermModeDef) ([]PermModeDef, error) {
			i := modeIndex(list, name)
			switch {
			case i < 0:
				return nil, fmt.Errorf("no mode %q", name)
			case len(list) == 1:
				return nil, errors.New("the last mode cannot be removed")
			}
			if err := mayReplace(h.PluginOf, "permission mode "+name, list[i].Origin, list[i].Path, by, byPath); err != nil {
				return nil, err
			}
			return slices.Delete(list, i, i+1), nil
		})
		if err != nil {
			L.RaiseError("zemi.permission.mode.del: %v", err)
		}
		h.refresh()
		return 0
	}))
	// set(name) switches the active mode; get() reads it back.
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		h.refuseBelowUser(L, "permission.mode.set")
		if modeIndex(h.PermModeList(), name) >= 0 && h.cfgReplayed("permissions.mode", name) {
			L.Push(glua.LTrue)
			return 1
		}
		return luaResult(L, h.SetPermMode(name))
	}))
	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		L.Push(modeRow(L, h.permModeDef()))
		return 1
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, d := range h.PermModeList() {
			out.Append(modeRow(L, d))
		}
		L.Push(out)
		return 1
	}))
	return tb
}

func modeRow(L *glua.LState, d PermModeDef) *glua.LTable {
	row := L.NewTable()
	row.RawSetString("name", glua.LString(d.Name))
	row.RawSetString("base", glua.LString(d.Base))
	row.RawSetString("description", glua.LString(d.Description))
	return row
}

// autoLocal is everything auto mode settles: reading and editing files
// inside the jail that are not secret-shaped (editing a local plugin whose
// creation was allowed too), commands (unsettled), running
// workflows (their agents' tools are checked one by one) and MCP tools their
// server marks read-only. Anything else goes to the user with a reason
// (askHook); a network request always does, since the URL is the model's.
func (h *Host) autoLocal(req tools.Request) bool {
	switch req.Subject {
	case perm.SubjectRead:
		return h.inJail(req.Arg, append(h.readJails(), h.localPlugins())...) && !h.secretPath(req.Arg)
	case perm.SubjectWrite:
		// zemi's own dirs can sit inside the project (zemi run in $HOME), and a
		// write there (init.lua) changes what zemi allows.
		// NOTE: a plugin whose creation was allowed settles with no trust check
		// (it is in no project, and $HOME is never trusted), so text the model
		// reads later can steer code every later boot runs, in every project.
		// Plugin origin bounds it: it adds no allow rule, and gated calls need a
		// rule, though a user one with no principal covers it too.
		return (h.ProjectTrusted() && h.inJail(req.Arg, h.writeJails()...) || h.madePlugin(req.Arg)) && !h.secretPath(req.Arg) && !h.protectedWrite(req.Arg)
	case perm.SubjectShell:
		return h.unsettled(req) == ""
	case perm.SubjectWorkflow:
		return true // the script is sandboxed and each agent it starts is gated
	}
	if server, ok := strings.CutPrefix(req.Subject, "mcp."); ok {
		return h.mcpReadOnly(server, req.Arg)
	}
	return false
}

func (h *Host) protectedWrite(p string) bool { return h.protectedWhy(p) != "" }

// memFiles are the brain partitions' reserved one-liner stores: the memory
// tool owns them, so a file-tool write asks with that reason (DESIGN §10.4).
func (h *Host) memFiles() []string {
	user, private := h.brainParts()
	var out []string
	for _, p := range []brain.Partition{user, private} {
		if !p.Zero() {
			out = append(out, filepath.Join(p.Dir, brain.MemFile))
		}
	}
	return out
}

// memLintMax caps the reserved file's findings an ask carries (DESIGN §10.4).
const memLintMax = 3

// memLintNotes lints the brain partition a reserved-file write lands in and
// returns up to three of the file's own findings' texts for the ask (DESIGN §10.4).
func (h *Host) memLintNotes(arg string) []string {
	user, private := h.brainParts()
	for _, p := range []brain.Partition{user, private} {
		if !h.inJail(arg, filepath.Join(p.Dir, brain.MemFile)) {
			continue
		}
		findings := brain.Lint(p)
		n := 0
		for _, f := range findings {
			if f.Subject == brain.MemFile {
				findings[n] = f
				n++
			}
		}
		findings = findings[:min(n, memLintMax)]
		notes := make([]string, len(findings))
		for i, f := range findings {
			notes[i] = f.Text
		}
		return notes
	}
	return nil
}

// protectedWhy says why a write needs the user, "" when it does not: git
// runs what .git, a .gitattributes or .gitmodules names, and the Lua in
// .agents, zemi's own dirs and a local plugin (but one whose creation was
// allowed in this process) change what zemi runs or allows.
func (h *Host) protectedWhy(p string) string {
	d, base := h.Dirs, filepath.Base(p)
	const git = "which decides what git runs"
	switch {
	case inDirNamed(p, ".agents/init.lua", ".agents/lua"),
		h.inJail(p, filepath.Join(d.ProjectConfigDir(), "init.lua"), filepath.Join(d.ProjectConfigDir(), "lua")):
		return "this changes the Lua in .agents, which zemi runs"
	case inDirNamed(p, ".git"):
		return "this changes .git, " + git
	case strings.EqualFold(base, ".gitattributes"), strings.EqualFold(base, ".gitmodules"):
		return "this changes " + base + ", " + git
	case h.inJail(p, gitdirTarget(filepath.Join(d.ProjectDir, ".git")), gitdirTarget(filepath.Join(d.RepoRoot(), ".git"))):
		return "this changes .git, " + git
	case h.inJail(p, d.WorktreesDir()), h.madePlugin(p):
		return ""
	case h.newPlugin(p) != "":
		return "this creates the local plugin " + h.PluginOf(p) + ", which zemi runs in every project; once allowed, zemi edits it without asking until it exits"
	case h.inJail(p, h.localPlugins()):
		// an approval in one project must not plant code every project loads
		return "this changes a local plugin, which zemi runs in every project"
	case h.inJail(p, h.memFiles()...):
		return memOwnedReason
	case h.inJail(p, d.ConfigHome, d.DataHome, d.StateHome):
		return "this changes zemi's own files"
	}
	return ""
}

// unsettled says why auto mode leaves a shell command to the user, or ""
// when it runs. In a trusted project with a sandbox every command runs,
// confined (Pipeline.SandboxFor), but the few that delete in bulk outside
// the request's scratchpad; a
// read-only agent's run is confined with the project unwritable whoever
// allowed it, so those fail on their own and an ask would only add a prompt.
// Anywhere else it would run unconfined, so only readers settle.
// NOTE: nothing parses what a sandboxed command does, so the sandbox policy
// is the only limit: it may rewrite any project file (rewind restores only
// repository files git does not ignore, at most snapCheckpoints per call),
// run the project's code, delete through a variable or an interpreter, and
// read what the policy does not hide, outside the project too (without
// HidesSecretFiles, a .env it names by a glob). Screening the text again
// would bring the asks back; tighten sandboxPolicy instead.
func (h *Host) unsettled(req tools.Request) string {
	n := req.Command()
	// a plugin's tests, the command the plugin steps repeat, read only the
	// plugin and start nothing, so confined they run in any project
	if exe, _ := os.Executable(); h.sandboxLive() && n.PluginTest(exe) {
		return ""
	}
	if !h.ProjectTrusted() || !h.sandboxLive() {
		if n.ReadCommand(h.safeCommandPath(req.Pad)) {
			return ""
		}
		return "only commands that just read run without a trusted project and a sandbox"
	}
	if !req.ReadOnly {
		if why := n.Destructive(req.Pad); why != "" {
			return why
		}
		// a git write that can only fail confined asks first, which saves the
		// failed run and the sandbox: false retry
		if why := n.GitWrites(false); why != "" {
			if why = n.GitWrites(h.policyWalk(false, req.Pad, false).Git != ""); why != "" {
				return why
			}
		}
	}
	if h.sandbox.Load().backend.HidesSecretFiles {
		return ""
	}
	for _, f := range n.Args {
		for _, a := range f[1:] {
			if !filepath.IsAbs(a) {
				a = filepath.Join(h.Dirs.ProjectDir, a)
			}
			if _, err := os.Lstat(a); err == nil && perm.PathIsSecret(a) {
				return filepath.Base(a) + " looks like it holds secrets, and this sandbox cannot hide it"
			}
		}
	}
	return ""
}

// inDirNamed reports a path under an entry with one of names (or that entry
// itself), a name being one or more path components: git runs programs its
// config and hooks name, even for read-only commands, and zemi runs the Lua
// in a project's .agents.
func inDirNamed(p string, names ...string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return true
	}
	wants := make([][]string, len(names))
	for i, n := range names {
		wants[i] = strings.Split(n, "/")
	}
	// EqualFold also matches .GIT on a case-sensitive volume, which only asks more
	named := func(parts []string) bool {
		return slices.ContainsFunc(wants, func(want []string) bool {
			return len(parts) >= len(want) && slices.EqualFunc(parts[:len(want)], want, strings.EqualFold)
		})
	}
	for _, q := range []string{abs, perm.RealPath(p)} {
		parts := strings.Split(filepath.ToSlash(q), "/")
		for i := range parts {
			if named(parts[i:]) {
				return true
			}
		}
	}
	return false
}

// safeCommandPath passes a settled command's argument when, read as a path
// from the project root, it stays in the project and is not secret-shaped.
// Plain reads may also use pad, zemi's config, the local plugins and the
// shipped dirs, which the read tool reads without asking either; cd may not enter them, since
// an unconfined command from there could run what the model wrote to the
// pad without asking.
func (h *Host) safeCommandPath(pad string) func(arg string, use perm.ArgUse) bool {
	return func(arg string, use perm.ArgUse) bool {
		if strings.HasPrefix(arg, "~") {
			return false
		}
		p := arg
		if !filepath.IsAbs(p) {
			// not Join: it cleans .. before RealPath sees a symlink before it
			p = h.Dirs.ProjectDir + string(filepath.Separator) + p
		}
		roots := []string{h.Dirs.ProjectDir}
		if use != perm.ArgRun {
			roots = slices.Concat(roots, []string{pad, h.Dirs.ConfigHome, h.localPlugins()}, h.shippedDirs(), h.Dirs.UserTrees())
		}
		walk := use == perm.ArgWalk || use == perm.ArgSearch
		// .git/config may hold tokens
		return h.inJail(p, roots...) && !h.secretPath(p) && !inDirNamed(p, ".git") &&
			(!walk || h.noSecretsUnder(p, roots, use == perm.ArgSearch))
	}
}

// noSecretsUnder reports whether a recursive search of p could print no
// secret-shaped file: p exists, and if it is a directory, nothing under it
// is secret-shaped, a .git directory (grep -r and rg --hidden read its
// config, which may hold tokens) or a symlink to a directory, a secret or
// out of roots. With skipIgnored, hidden entries and those git ignores are
// left out, as rg leaves them out.
// NOTE: gives up past 10000 entries, so a search over a big tree asks;
// an index of secret-shaped names kept per project would lift that.
func (h *Host) noSecretsUnder(p string, roots []string, skipIgnored bool) bool {
	p = perm.RealPath(p)
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	// a ! line in an ignore file makes rg search what it names, hidden or not
	if skipIgnored && st.IsDir() && parentWhitelist(p) {
		skipIgnored = false
	}
	ignored := map[string]bool{}
	if skipIgnored && st.IsDir() {
		// a failed listing (no repo, no git) leaves nothing ignored: rg
		// outside a repository skips no gitignored path either; rg matches
		// ignore rules case-sensitively, whatever core.ignorecase says
		out, err := h.git(p, "-c", "core.ignorecase=false", "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
		for e := range strings.SplitSeq(out, "\x00") {
			if err == nil && e != "" {
				ignored[strings.TrimSuffix(e, "/")] = true
			}
		}
	}
	n, unsafe := 0, errors.New("unsafe")
	return filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		// rg reads the ignore files of every directory it enters, hidden or
		// ignored as they are; one that whitelists asks
		if skipIgnored && slices.Contains(ignoreFiles, d.Name()) && whitelists(path) {
			return unsafe
		}
		if rel, _ := filepath.Rel(p, path); skipIgnored && rel != "." && (strings.HasPrefix(d.Name(), ".") || ignored[filepath.ToSlash(rel)]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		n++
		switch {
		case err != nil:
			return err
		case n > 10000 || perm.PathIsSecret(path) || d.IsDir() && strings.EqualFold(d.Name(), ".git"):
			return unsafe
		case d.Type()&fs.ModeSymlink != 0:
			if st, err := os.Stat(path); err != nil || st.IsDir() || !h.inJail(path, roots...) || h.secretPath(path) {
				return unsafe
			}
		}
		return nil
	}) == nil
}

var ignoreFiles = []string{".ignore", ".rgignore", ".gitignore"}

// parentWhitelist reports an ignore file in a parent of dir that rg reads
// for a search of dir and that has a whitelist line.
func parentWhitelist(dir string) bool {
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		for _, name := range ignoreFiles {
			if whitelists(filepath.Join(d, name)) {
				return true
			}
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// whitelists reports a line starting with ! in the ignore file at path.
func whitelists(path string) bool {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return false // a FIFO would block the read
	}
	data, _ := os.ReadFile(path)
	return slices.ContainsFunc(strings.Split(string(data), "\n"), func(l string) bool {
		return strings.HasPrefix(strings.TrimSpace(l), "!")
	})
}

// modeDecideCeiling bounds a mode's decide in wall time, long enough for a
// model call over zemi.http; a wait in it frees the interpreter.
const modeDecideCeiling = 60 * time.Second

// modeAnswer is what a mode's decide returned: the decision, the scope an
// allow takes (askChoices), and its own reason for the row and the denial.
type modeAnswer struct {
	Decision, Scope, Reason string
}

// decidePayload is what a decide sees: the request as the ask shows it, what
// no mode rule may settle (fencedWhy), the host's own facts, and the scopes an
// allow may take.
func (h *Host) decidePayload(req tools.Request, options []string) map[string]any {
	protected := ""
	if req.Subject == perm.SubjectWrite && !req.InPad() {
		protected = h.protectedWhy(req.Arg)
	}
	return map[string]any{
		"subject": req.Subject, "arg": h.shownArg(req), "tool": req.Tool,
		"reason": req.Reason, "preview": req.Preview, "description": req.Description, "agent": req.Agent,
		"options": options, "read_only": req.ReadOnly, "trusted": h.ProjectTrusted(), "sandbox": h.sandboxLive(),
		"unsandboxed": req.Unsandboxed, "network": req.Network, "explicit": req.Explicit,
		"protected_write": protected != "", "protected_why": protected,
	}
}

// modeDecide runs a mode's decide function for one ask, as a task, and
// returns what it answered; "" (an error, a timeout, a reload, anything but
// allow or deny) leaves the request to the user.
func (h *Host) modeDecide(def PermModeDef, payload map[string]any) modeAnswer {
	ctx := context.Background()
	if t := h.turn.Load(); t != nil {
		ctx = t.ctx // an interrupt stops the decision too
	}
	ctx, cancel := context.WithTimeout(ctx, modeDecideCeiling)
	defer cancel()
	out, err := h.RT.RunTask(ctx, lua.Task{
		Class: lua.ClassAgent, Ceiling: modeDecideCeiling, Origin: def.Origin, Path: "permission.mode." + def.Name,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			if def.Gen != 0 && h.currentGen() != def.Gen {
				return nil, nil, lua.ErrReloaded
			}
			return def.Decide, []glua.LValue{lua.FromGo(L, payload)}, nil
		},
		Done: func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			var ans modeAnswer
			if len(rets) > 0 {
				v, _ := lua.ToGo(rets[0])
				switch t := v.(type) {
				case string:
					ans.Decision = t
				case map[string]any:
					// a table answer names the decision and how far it goes
					ans.Decision, _ = t["decision"].(string)
					ans.Scope, _ = t["scope"].(string)
					ans.Reason, _ = t["reason"].(string)
				}
			}
			return []any{ans}, nil
		},
	})
	if err != nil {
		if !errors.Is(err, lua.ErrReloaded) {
			h.Log.Warn("mode decide failed", "origin", "permissions", "mode", def.Name, "error", err)
		}
		return modeAnswer{}
	}
	if len(out) == 0 {
		return modeAnswer{}
	}
	ans, _ := out[0].(modeAnswer)
	return ans
}

func (h *Host) permissionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	rule := L.NewTable()
	rule.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.permission.rule.add: rule must be a table")
		}
		r := perm.Rule{
			Subject:  rawString(L, spec, "subject"),
			Pattern:  rawString(L, spec, "pattern"),
			Decision: perm.Decision(rawString(L, spec, "decision")),
			NotMatch: rawString(L, spec, "not_match"),
		}
		switch rawString(L, spec, "scope") {
		case "", "session":
		case "permanent":
			L.RaiseError(`zemi.permission.rule.add: scope "permanent" was removed; a rule in init.lua applies on every start, so drop the scope field`)
		default:
			L.RaiseError(`zemi.permission.rule.add: scope must be "session"`)
		}
		// Origins (DESIGN §5, §6.1): allow rules widen, so they come from
		// user config only; every other origin may only add deny/ask rules,
		// which match every principal, so none can aim at another plugin.
		if origin := h.RT.CurrentOrigin(L); origin != lua.OriginUser {
			if r.Decision == perm.Allow {
				L.RaiseError("zemi.permission.rule.add: allow rules may only come from user config")
			}
			if rawString(L, spec, "principal") != "" {
				L.RaiseError("zemi.permission.rule.add: only user config scopes a rule to a principal")
			}
			r.Source = string(origin)
		}
		if p := rawString(L, spec, "principal"); p != "" {
			if name, ok := perm.CutPluginPrincipal(p); !ok || name == "" {
				L.RaiseError(`zemi.permission.rule.add: principal must be "plugin:<name>", got %q`, p)
			}
			r.Principal = p
		}
		if err := h.validateRule(&r, rawString(L, spec, "match"), rawString(L, spec, "not_match_example")); err != nil {
			L.RaiseError("zemi.permission.rule.add: %v", err)
		}
		h.addPermissionRule(r)
		return 0
	}))

	tb.RawSetString("mode", h.modeNamespace(L))

	rule.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, r := range h.Perm.Rules() {
			row := L.NewTable()
			row.RawSetString("subject", glua.LString(r.Subject))
			row.RawSetString("pattern", glua.LString(r.Pattern))
			row.RawSetString("decision", glua.LString(r.Decision))
			row.RawSetString("not_match", glua.LString(r.NotMatch))
			if r.Principal != "" {
				row.RawSetString("principal", glua.LString(r.Principal))
			}
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("rule", rule)
	tb.RawSetString("decision", enumTable(L, perm.Allow, perm.Ask, perm.Deny))
	return tb
}

// askingSubjects are the builtin tools' own subjects; every other builtin
// asks under fs., net. or memory., if at all.
var askingSubjects = []string{"tool.*", perm.SubjectShell, perm.SubjectLua, perm.SubjectWorkflow}

// toolSubjects are the tool.* subjects a request can carry: the builtin
// tools' and the Lua tools'.
func (h *Host) toolSubjects() []string {
	subjects := slices.Clone(askingSubjects)
	for _, t := range h.luaTools.list() {
		subjects = append(subjects, "tool."+t.Name)
	}
	return subjects
}

// ruleShape checks what every rule intake needs: a subject in a known
// family, a pattern and a decision. A tool.<builtin> subject no builtin asks
// as raises here; any other tool.<name> may be a Lua tool config adds later,
// so CheckToolNames judges it once boot is done.
func (h *Host) ruleShape(r perm.Rule) error {
	name, isTool := strings.CutPrefix(r.Subject, "tool.")
	switch {
	case r.Subject == "" || r.Pattern == "":
		return errors.New("subject and pattern are required")
	case !slices.ContainsFunc(perm.SubjectPrefixes, func(p string) bool { return strings.HasPrefix(r.Subject, p) }):
		return fmt.Errorf("subject %q starts with none of %s", r.Subject, strings.Join(perm.SubjectPrefixes, " "))
	case isTool && slices.Contains(h.builtinToolNames(), name) && !slices.Contains(askingSubjects, r.Subject):
		return fmt.Errorf("no tool asks as %q: tool subjects are %s and tool.<a Lua tool>", r.Subject, strings.Join(askingSubjects, " "))
	case r.Decision != perm.Allow && r.Decision != perm.Ask && r.Decision != perm.Deny:
		return errors.New("decision must be allow, ask or deny")
	}
	return nil
}

// CheckToolNames records each tool.* rule and tool override no tool answers
// to, once boot has added every Lua tool: the tool one names may come from a
// project or a plugin, so the call that adds it cannot tell.
func (h *Host) CheckToolNames() {
	subjects := h.toolSubjects()
	rules := h.Perm.Rules()
	for _, m := range h.PermModeList() {
		if m.Rules != nil {
			rules = append(rules, m.Rules.Rules()...)
		}
	}
	for _, r := range rules {
		if strings.HasPrefix(r.Subject, "tool.") && !slices.Contains(subjects, r.Subject) {
			h.RecordErrorOnce("permissions", fmt.Sprintf("no tool asks as %q, so its rule never matches: tool subjects are %s", r.Subject, strings.Join(subjects, " ")))
		}
	}
	names := h.builtinToolNames()
	for _, t := range h.luaTools.list() {
		names = append(names, t.Name)
	}
	for _, name := range h.luaTools.overridden() {
		if !slices.Contains(names, name) && !strings.HasPrefix(name, "mcp_") {
			h.RecordErrorOnce("tools", fmt.Sprintf("zemi.tool.override names %q, which no tool is, so it never runs: tools are %s and mcp_<server>_<tool>", name, strings.Join(names, " ")))
		}
	}
}

// validateRule checks a Lua rule. Examples are required (DESIGN §9): the match example must match
// the pattern without being excluded, the not_match example must be
// excluded; a guard needs both examples.
func (h *Host) validateRule(r *perm.Rule, matchEx, notMatchEx string) error {
	if err := h.ruleShape(*r); err != nil {
		return err
	}
	if r.NotMatch == "" {
		if matchEx == "" {
			return fmt.Errorf(`rule %s %q needs a match example: add match = "<an argument the pattern should match>"`, r.Subject, r.Pattern)
		}
		if !perm.Match(r.Pattern, matchEx) {
			return fmt.Errorf("match example %q does not match pattern %q", matchEx, r.Pattern)
		}
		return nil
	}
	if matchEx == "" || notMatchEx == "" {
		return fmt.Errorf("rule %s %q with not_match needs both examples: match and not_match_example", r.Subject, r.Pattern)
	}
	if !perm.Match(r.Pattern, matchEx) || perm.Match(r.NotMatch, matchEx) {
		return fmt.Errorf("match example %q must match %q and not be excluded by %q", matchEx, r.Pattern, r.NotMatch)
	}
	if perm.Match(r.Pattern, notMatchEx) && !perm.Match(r.NotMatch, notMatchEx) {
		return fmt.Errorf("not_match example %q must be excluded by %q", notMatchEx, r.NotMatch)
	}
	return nil
}
