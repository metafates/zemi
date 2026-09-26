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
	// anything else to leave it to the user. It dies with its generation
	// (PurgeGeneration clears the list), so no reload check is needed.
	Decide *glua.LFunction
	Origin lua.Origin
}

var builtinModes = []PermModeDef{
	{Name: "auto", Base: "auto", Description: "safe actions run on their own, risky ones ask"},
	{Name: "yolo", Base: "yolo", Description: "everything runs without asking"},
}

// modeReg is the mode list in cycle order: the builtins until user config
// changes it, reset on reload so init.lua replays onto a clean list.
type modeReg struct {
	mu             sync.Mutex
	list           []PermModeDef // nil = builtinModes
	prePlanMode    string        // the mode active before a read-only agent was picked
	prePlanProfile string        // the agent active before a read-only agent was picked

	headless Denials // asks denied because no UI could answer them
}

// Denials counts asks nobody could answer (-p), every pipeline's:
// subagents and workflows run no rows the main transcript shows.
type Denials struct {
	Asked       int // all of them
	Unsandboxed int // of those, a command asking to leave the sandbox
	TUIOnly     int // of those, a Lua script or a memory edit
}

func (r *modeReg) noteHeadless(req tools.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headless.Asked++
	switch {
	case req.Unsandboxed && req.Subject == perm.SubjectBash:
		r.headless.Unsandboxed++
	case req.Tool == tools.ToolLua || req.Tool == tools.ToolMemory:
		r.headless.TUIOnly++
	}
}

// HeadlessDenials is the count so far; -p reads the change over a turn.
func (h *Host) HeadlessDenials() Denials {
	h.modes.mu.Lock()
	defer h.modes.mu.Unlock()
	return h.modes.headless
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
			item["actions"] = []any{map[string]any{"text": "Delete", "hotkey": "x", "hl": "Danger", "run": forget}}
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
				item["detail"] = "from " + TildePath(h.projectPermissionsPath()) + "; edit that file"
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
	h.Log.Info("permissions", "session rule removed", map[string]any{"subject": subject, "pattern": pattern})
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
		if d.Base == "auto" {
			return d
		}
	}
	return list[0]
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

// approvePlan is the plan tool's hook: it shows a plan in the approval popup.
// The plan joins the transcript and is saved, then the user approves it,
// types what to change, or dismisses it. A read-only agent (planner)
// remembers what ran before it (prePlan), so approving a plan returns to
// that agent and mode, yolo included, instead of asking; an approval records
// an implementation handoff (planApproved) that the ended turn passes on to
// planFollowup.
func (h *Host) approvePlan(plan string) (tools.PlanVerdict, bool) {
	ui := h.hooks()
	if ui == nil {
		return tools.PlanVerdict{}, false
	}
	var v tools.PlanVerdict
	if sid := h.AgentSessionID(); sid != "" && h.Dirs.DataHome != "" {
		path := filepath.Join(h.Dirs.DataHome, "plans", sid+".md")
		if os.MkdirAll(filepath.Dir(path), 0o700) == nil && os.WriteFile(path, []byte(plan+"\n"), 0o600) == nil {
			v.Path = path
		}
	}
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
	if verdict.Approved {
		if restore != "" {
			if err := h.SetProfile(restore); err != nil {
				return v, false
			}
			if mode != "" {
				if err := h.SetPermMode(mode); err != nil {
					return v, false
				}
			}
			// the read-only turn cannot implement the plan itself; the turn-end
			// hook runs it as the restored agent
			h.setPlanApproved(implementNow)
		}
		v.Approved = true
		return v, true
	}
	v.Feedback = verdict.Feedback
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

// modeNamespace is zemi.permission.mode: add, del and list modes from
// user config (a mode answers permission asks, so no other origin may).
func (h *Host) modeNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	gate := func(L *glua.LState, fn string) {
		h.refuseBelowUser(L, "permission.mode."+fn)
	}
	// add(name, { base, description, prompt, rules = { { subject, pattern, decision }, ... } })
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name, spec := L.CheckString(1), L.CheckTable(2)
		gate(L, "add")
		def := PermModeDef{
			Name: name, Base: rawString(spec, "base"), Description: rawString(spec, "description"), Prompt: rawString(spec, "prompt"),
			Origin: h.RT.CurrentOrigin(L),
		}
		def.Decide, _ = spec.RawGetString("decide").(*glua.LFunction)
		if !modeName.MatchString(name) {
			L.RaiseError("zemi.permission.mode.add: name %q must match [a-z][a-z0-9-]{0,15}", name)
		}
		if modeIndex(builtinModes, def.Base) < 0 {
			L.RaiseError("zemi.permission.mode.add: base must be auto or yolo")
		}
		if rules, ok := spec.RawGetString("rules").(*glua.LTable); ok {
			def.Rules = perm.NewStore()
			var bad string
			rules.ForEach(func(_, v glua.LValue) {
				rt, ok := v.(*glua.LTable)
				if !ok {
					bad = "each rule must be a table"
					return
				}
				field := func(i int, key string) string {
					if s := rawString(rt, key); s != "" {
						return s
					}
					s, _ := rt.RawGetInt(i).(glua.LString)
					return string(s)
				}
				r := perm.Rule{Subject: field(1, "subject"), Pattern: field(2, "pattern"), Decision: perm.Decision(field(3, "decision"))}
				if err := ruleShape(r); err != nil {
					bad = err.Error()
					return
				}
				def.Rules.Add(r)
			})
			if bad != "" {
				L.RaiseError("zemi.permission.mode.add: %s", bad)
			}
		}
		if def.Description == "" {
			def.Description = "like " + def.Base
		}
		_ = h.setModes(func(list []PermModeDef) ([]PermModeDef, error) {
			if i := modeIndex(list, name); i >= 0 {
				list[i] = def
				return list, nil
			}
			return append(list, def), nil
		})
		h.refresh()
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		gate(L, "del")
		err := h.setModes(func(list []PermModeDef) ([]PermModeDef, error) {
			i := modeIndex(list, name)
			switch {
			case i < 0:
				return nil, fmt.Errorf("no mode %q", name)
			case len(list) == 1:
				return nil, errors.New("the last mode cannot be removed")
			}
			return slices.Delete(list, i, i+1), nil
		})
		if err != nil {
			L.RaiseError("zemi.permission.mode.del: %v", err)
		}
		h.refresh()
		return 0
	}))
	// set(name) switches the active mode; list() says which one that is.
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		gate(L, "set")
		if modeIndex(h.PermModeList(), name) >= 0 && h.cfgReplayed("permissions.mode", name) {
			L.Push(glua.LTrue)
			return 1
		}
		if err := h.SetPermMode(name); err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		L.Push(glua.LTrue)
		return 1
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		cur := h.PermMode()
		for _, d := range h.PermModeList() {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(d.Name))
			row.RawSetString("base", glua.LString(d.Base))
			row.RawSetString("description", glua.LString(d.Description))
			row.RawSetString("active", glua.LBool(d.Name == cur))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	return tb
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
	case perm.SubjectBash:
		return h.unsettled(req.Command(), req.ReadOnly) == ""
	case "tool.workflow":
		return true // the script is sandboxed and each agent it starts is gated
	}
	if server, ok := strings.CutPrefix(req.Subject, "mcp."); ok {
		return h.mcpReadOnly(server, req.Arg)
	}
	return false
}

func (h *Host) protectedWrite(p string) bool { return h.protectedWhy(p) != "" }

// protectedWhy says why a write under any .git, to the Lua in any .agents
// (init.lua, lua/), to a .gitattributes or .gitmodules, the git dir a
// project .git file names (git init --separate-git-dir) or zemi's own dirs,
// local plugins included but one whose creation was allowed in this
// process, needs the user, "" for any other path: git runs
// what those name, and the rest changes what zemi runs or allows. The rest
// of .agents (skills, agents, commands, workflows) is an ordinary project
// write.
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
	case h.inJail(p, d.ConfigHome, d.DataHome, d.StateHome):
		return "this changes zemi's own files"
	}
	return ""
}

// unsettled says why auto mode leaves a bash command to the user, or ""
// when it runs. In a trusted project with a sandbox every command runs,
// confined (Pipeline.SandboxFor), but the few that delete in bulk; a
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
func (h *Host) unsettled(n perm.Normalized, readOnly bool) string {
	if !h.ProjectTrusted() || !h.sandboxLive() {
		if n.ReadCommand(h.safeCommandPath) {
			return ""
		}
		return "only commands that just read run without a trusted project and a sandbox"
	}
	if !readOnly {
		if why := n.Destructive(); why != "" {
			return why
		}
		// a confined git write can only fail; asking first saves the failed
		// run and the sandbox: false retry
		if why := n.GitWrites(); why != "" {
			return why
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
// Plain reads may also use the scratchpad, zemi's config and the local
// plugins, which the read tool reads without asking either; code may not run
// from there: the model writes the pad without asking, even for a read-only
// agent.
func (h *Host) safeCommandPath(arg string, use perm.ArgUse) bool {
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
		roots = append(append(roots, h.ScratchDir(), h.Dirs.ConfigHome, h.localPlugins()), h.Dirs.UserTrees()...)
	}
	walk := use == perm.ArgWalk || use == perm.ArgSearch
	// .git/config may hold tokens
	return h.inJail(p, roots...) && !h.secretPath(p) && !inDirNamed(p, ".git") &&
		(!walk || h.noSecretsUnder(p, roots, use == perm.ArgSearch))
}

// noSecretsUnder reports whether a recursive search of p could print no
// secret-shaped file: p exists, and if it is a directory, nothing under it
// (.git aside) is secret-shaped or a symlink to a directory, a secret or
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
		case n > 10000 || perm.PathIsSecret(path):
			return unsafe
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
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

// modeDecide runs a mode's decide function for one ask, as a task, and
// returns what it said; "" (an error, a timeout, a reload, anything but
// allow or deny) leaves the request to the user.
func (h *Host) modeDecide(def PermModeDef, req tools.Request) string {
	ctx := context.Background()
	if t := h.turn.Load(); t != nil {
		ctx = t.ctx // an interrupt stops the decision too
	}
	ctx, cancel := context.WithTimeout(ctx, modeDecideCeiling)
	defer cancel()
	out, err := h.RT.RunTask(ctx, lua.Task{
		Class: lua.ClassAgent, Ceiling: modeDecideCeiling, Origin: def.Origin, Path: "permission.mode." + def.Name,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			return def.Decide, []glua.LValue{lua.FromGo(L, map[string]any{
				"subject": req.Subject, "arg": h.shownArg(req), "tool": req.Tool,
				"reason": req.Reason, "preview": req.Preview, "description": req.Description,
			})}, nil
		},
		Done: func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			s, _ := lua.ToGo(rets[0])
			return []any{s}, nil
		},
	})
	if err != nil {
		h.Log.Warn("permissions", "mode decide failed", map[string]any{"mode": def.Name, "error": err.Error()})
		return ""
	}
	if len(out) == 0 {
		return ""
	}
	s, _ := out[0].(string)
	return s
}
