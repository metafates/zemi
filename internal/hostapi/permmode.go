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

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// PermModeDef is one permission mode. Base picks the behavior: plan (file
// changes refused, risky commands ask), auto (deterministic local checks
// answer routine asks) or yolo (every
// ask allowed). Every mode only answers asks: deny rules and the
// tool.pre/permission.request vetoes still run first. Rules answer asks
// before the base does, and Prompt joins the system prompt while the mode
// is on.
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
	{Name: "plan", Base: "plan", Description: "read and plan only: no edits, risky commands ask"},
	{Name: "auto", Base: "auto", Description: "safe actions run on their own, risky ones ask"},
	{Name: "yolo", Base: "yolo", Description: "everything runs without asking"},
}

// modeReg is the mode list in tab order: the builtins until user config
// changes it, reset on reload so init.lua replays onto a clean list.
type modeReg struct {
	mu      sync.Mutex
	list    []PermModeDef // nil = builtinModes
	prePlan string        // the mode active before the last switch into plan mode
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

// planPrompt joins the system prompt while plan mode is on; the pipeline
// enforces it (Pipeline.Plan), the prompt only saves wasted attempts.
const planPrompt = `

# Plan mode
The user switched on plan mode: plan the work with them and do not do it until they approve a plan. File writes are refused and commands that could change anything ask, so read, search and run read-only commands. If the user asks for a change, plan that change.
1. Read the code the task touches first. Never ask what reading it would tell you.`

// planAsking is the rest of plan mode when a user can answer; a headless
// run has neither the question nor the plan tool.
const planAsking = `
2. Ask what only the user can answer (goal, scope, behavior, tradeoffs) with the question tool, related questions in one call. A vague request needs several rounds; a focused fix may need none.
3. When nothing is left to decide, call the plan tool with the plan.
End every turn with the question tool or the plan tool; never ask in plain text whether the plan is good. Approving the plan takes zemi out of plan mode: then implement it.`

const planHeadless = `
2. Write the plan as your reply and stop there: nobody can approve it in this run.`

// OpenModePicker lists the modes with what they mean; enter switches.
func (h *Host) OpenModePicker() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
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
	ui.OpenOverlay(Overlay{ID: "mode", Title: "Permission mode", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: "bottom"})
	return nil
}

// OpenPermissions lists the active mode and every rule in force. An
// "always" answer is the only rule a user can write at runtime and the only
// one with no file to edit, so a mis-keyed one must be visible and
// removable; config rules come back on the next reload, so they show
// read-only.
func (h *Host) OpenPermissions() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
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
		"text": "mode: " + mode.Name, "desc": mode.Description, "key": "mode",
		"section": "Mode", "actions": []any{map[string]any{"text": "Change", "run": "mode"}},
	}}
	rules := h.Perm.Rules()
	slices.SortStableFunc(rules, func(a, b perm.Rule) int {
		return cmp.Or(cmp.Compare(permRank[a.Decision], permRank[b.Decision]), cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Pattern, b.Pattern))
	})
	for _, r := range rules {
		desc := ""
		if r.NotMatch != "" {
			desc = "except " + r.NotMatch
		}
		item := map[string]any{
			"text": r.Subject + " " + r.Pattern, "desc": desc,
			"section": string(r.Decision) + " rules",
		}
		if r.Principal == "model" {
			forget := "permissions forget " + r.Subject + " " + r.Pattern
			item["key"], item["run"] = "session", forget
			item["actions"] = []any{map[string]any{"text": "Remove", "run": forget}}
			item["detail"] = "an always answer this session; removing it makes the next call ask again"
		} else {
			item["detail"] = "from config; edit permissions in your init.lua"
		}
		items = append(items, item)
	}
	return items
}

// ForgetRule drops the session rules an "always" answer wrote for one
// subject and pattern. Config rules stay: a reload would bring them back.
func (h *Host) ForgetRule(arg string) error {
	subject, pattern, _ := strings.Cut(strings.TrimSpace(arg), " ")
	gone := false
	for _, r := range h.Perm.Rules() {
		if r.Principal == "model" && r.Subject == subject && r.Pattern == pattern {
			h.Perm.Remove(r)
			gone = true
		}
	}
	if !gone {
		return fmt.Errorf("no session rule allows %s %s", subject, pattern)
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

// SetPermMode switches the mode at runtime (/mode, tab).
func (h *Host) SetPermMode(mode string) error {
	list := h.PermModeList()
	i := modeIndex(list, mode)
	if i < 0 {
		names := make([]string, len(list))
		for j, d := range list {
			names[j] = d.Name
		}
		return fmt.Errorf("unknown permission mode %q (want %s)", mode, strings.Join(names, ", "))
	}
	prev := h.permModeDef()
	if err := h.Settings.SetHost("permissions.mode", mode); err != nil {
		return err
	}
	if list[i].Base == "plan" && prev.Base != "plan" {
		h.modes.mu.Lock()
		h.modes.prePlan = prev.Name
		h.modes.mu.Unlock()
	}
	h.refresh()
	return nil
}

// approvePlan is the plan tool's hook: the plan joins the transcript and is
// saved, then the user approves it or types what to change. In plan mode
// each approval names the mode to implement in, the one active before plan
// mode first; yolo modes are left out so an approval never allows everything.
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
	h.appendChatMessage(ChatMessage{Role: "assistant", Text: sanitize.String(plan)})
	q := tools.Question{Question: "Implement this plan?", Other: "No, change it: type what to change"}
	var modes []string
	if h.permModeDef().Base == "plan" {
		h.modes.mu.Lock()
		prev := h.modes.prePlan
		h.modes.mu.Unlock()
		for _, d := range h.PermModeList() {
			if d.Base == "plan" || d.Base == "yolo" {
				continue
			}
			opt := tools.QuestionOption{Label: "Yes, in " + d.Name + " mode", Description: d.Description}
			if d.Name == prev {
				modes, q.Options = append([]string{d.Name}, modes...), append([]tools.QuestionOption{opt}, q.Options...)
			} else {
				modes, q.Options = append(modes, d.Name), append(q.Options, opt)
			}
		}
	} else {
		q.Options = []tools.QuestionOption{{Label: "Yes, implement it"}}
	}
	answers, ok := ui.AskQuestion([]tools.Question{q})
	if !ok || len(answers) == 0 {
		return v, false
	}
	for i, o := range q.Options {
		if answers[0] != o.Label {
			continue
		}
		if i < len(modes) {
			if err := h.SetPermMode(modes[i]); err != nil {
				return v, false
			}
			v.Mode = modes[i]
		}
		v.Approved = true
		return v, true
	}
	v.Feedback = answers[0]
	return v, true
}

// cyclePermMode steps through the modes (tab forward, shift+tab back).
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
		def := PermModeDef{Name: name, Base: rawString(spec, "base"), Description: rawString(spec, "description"), Prompt: rawString(spec, "prompt"),
			Origin: h.RT.CurrentOrigin(L)}
		def.Decide, _ = spec.RawGetString("decide").(*glua.LFunction)
		if !modeName.MatchString(name) {
			L.RaiseError("zemi.permission.mode.add: name %q must match [a-z][a-z0-9-]{0,15}", name)
		}
		if modeIndex(builtinModes, def.Base) < 0 {
			L.RaiseError("zemi.permission.mode.add: base must be plan, auto or yolo")
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
				if r.Subject == "" || r.Pattern == "" || (r.Decision != perm.Allow && r.Decision != perm.Deny && r.Decision != perm.Ask) {
					bad = "each rule needs subject, pattern and decision (allow, ask or deny)"
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
// inside the jail that are not secret-shaped, read-only or build and test
// commands, running workflows (their agents' tools are checked one by one)
// and MCP tools their server marks read-only. Anything else goes to the
// user with a reason (askHook); a network request always does, since the
// URL is the model's. An untrusted project settles only reads and commands
// that just read, and so does a trusted one without a sandbox backend for
// commands.
func (h *Host) autoLocal(req tools.Request) bool {
	switch req.Subject {
	case "fs.read":
		return h.inJail(req.Arg, h.readJails()...) && !h.secretPath(req.Arg)
	case "fs.write":
		// zemi's own dirs can sit inside the project (zemi run in $HOME), and a
		// write there (init.lua, .zemi/) changes what zemi allows
		return h.ProjectTrusted() && h.inJail(req.Arg, h.writeJails()...) && !h.secretPath(req.Arg) && !h.protectedWrite(req.Arg)
	case "tool.bash":
		return h.settledCommand(req.Arg)
	case "tool.workflow":
		return true // the script is sandboxed and each agent it starts is gated
	case "tool.window":
		// input to a window running unconfined is Unsandboxed and never
		// reaches here, so this drives a program the sandbox already holds
		return true
	}
	if server, ok := strings.CutPrefix(req.Subject, "mcp."); ok {
		return h.mcpReadOnly(server, req.Arg)
	}
	return false
}

// protectedWrite reports a write under any .git or .zemi, the git dir a
// project .git file names (git init --separate-git-dir) or zemi's own dirs:
// git runs what .git names, and the rest changes what zemi allows.
func (h *Host) protectedWrite(p string) bool {
	d := h.Dirs
	return inDirNamed(p, ".git", ".zemi") || h.inJail(p, d.ConfigHome, d.DataHome, d.StateHome, d.ProjectConfigDir(),
		gitdirTarget(filepath.Join(d.ProjectDir, ".git")), gitdirTarget(filepath.Join(d.RepoRoot(), ".git")))
}

// settledCommand reports a bash command auto and plan mode run without
// asking. Build and test commands and git run the repository's code and
// config, which only a trusted project may have them do, and only in the
// sandbox (Pipeline.SandboxFor); without a backend only readers settle.
func (h *Host) settledCommand(cmd string) bool {
	if h.ProjectTrusted() && h.sandboxLive() {
		return perm.SafeCommand(cmd, h.safeCommandPath)
	}
	return perm.ReadCommand(cmd, h.safeCommandPath)
}

// inDirNamed reports a path under a directory with one of names (or that
// entry itself): git runs programs its config and hooks name, even for
// read-only commands, and zemi loads the nearest .zemi above where it starts.
func inDirNamed(p string, names ...string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return true
	}
	// EqualFold also matches .GIT on a case-sensitive volume, which only asks more
	named := func(s string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(s, n) })
	}
	for _, q := range []string{abs, perm.RealPath(p)} {
		if slices.ContainsFunc(strings.Split(filepath.ToSlash(q), "/"), named) {
			return true
		}
	}
	return false
}

// safeCommandPath passes a settled command's argument when, read as a path
// from the project root, it stays in the project and is not secret-shaped.
// Plain reads may also use the scratchpad and zemi's config, which the read
// tool reads without asking either; code may not run from there: the model
// writes the pad without asking, even in plan mode.
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
		roots = append(append(roots, h.ScratchDir(), h.Dirs.ConfigHome), h.Dirs.UserTrees()...)
	}
	walk := use == perm.ArgWalk || use == perm.ArgSearch
	// a write flag the allowlist misses could still reach .git/config
	return h.inJail(p, roots...) && !h.secretPath(p) && !inDirNamed(p, ".git") &&
		(!walk || h.noSecretsUnder(p, roots, use == perm.ArgSearch)) && (use != perm.ArgRepo || h.trackedSecret(p) == "")
}

// trackedSecret says why git commands that print file content in the
// repository around dir may not settle: it tracks a secret-shaped file, or
// the listing failed (no repository too). Empty means they may.
// NOTE: a secret committed and later deleted is not seen, and git log -p
// still prints it; a history scan per command would be too slow.
func (h *Host) trackedSecret(dir string) string {
	// the pathspecs keep the listing short in a big repository
	args := []string{"ls-files", "-z", "--recurse-submodules", "--"}
	for _, s := range perm.SecretPathShape {
		args = append(args, ":(top,icase,glob)**/"+s)
	}
	out, err := h.git(dir, args...)
	if err != nil {
		return "cannot list the files git tracks"
	}
	for _, f := range strings.Split(out, "\x00") {
		if perm.PathIsSecret(f) {
			return "the repository tracks " + f
		}
	}
	return ""
}

// repoNote says why cmd did not settle when a tracked secret is the only
// reason: every other check passes and a git command prints file content
// from a repository that tracks one. Otherwise it is "".
func (h *Host) repoNote(cmd string) string {
	if !h.ProjectTrusted() || !h.sandboxLive() {
		return ""
	}
	note := ""
	ok := perm.SafeCommand(cmd, func(arg string, use perm.ArgUse) bool {
		if use != perm.ArgRepo {
			return h.safeCommandPath(arg, use)
		}
		// ArgRead runs every check ArgRepo does but the tracked-secret one
		if !h.safeCommandPath(arg, perm.ArgRead) {
			return false
		}
		if note == "" {
			p := arg
			if !filepath.IsAbs(p) {
				p = h.Dirs.ProjectDir + string(filepath.Separator) + p // as safeCommandPath joins it
			}
			note = h.trackedSecret(p)
		}
		return true
	})
	if !ok {
		return ""
	}
	return note
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
		for _, e := range strings.Split(out, "\x00") {
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

// modeDecideCeiling is how long a mode's decide function may run: long
// enough for a model call over zemi.http, short enough that a wedged one
// still trips the watchdog. It holds the interpreter for that time, which
// is the mode's own cost.
const modeDecideCeiling = 60 * time.Second

// modeDecide runs a mode's decide function for one ask and returns what it
// said; "" (an error, a timeout, anything but allow or deny) leaves the
// request to the user.
func (h *Host) modeDecide(def PermModeDef, req tools.Request) string {
	ctx := context.Background()
	if t := h.turn.Load(); t != nil {
		ctx = t.ctx // an interrupt stops the decision too
	}
	out, err := h.RT.Do(ctx, lua.ClassAgent, modeDecideCeiling, 2*time.Second, func(L *glua.LState) ([]any, error) {
		defer h.RT.PushFrame(L, def.Origin, "permission.mode."+def.Name)()
		base := L.GetTop()
		defer L.SetTop(base)
		L.Push(def.Decide)
		L.Push(lua.FromGo(L, map[string]any{
			"subject": req.Subject, "arg": h.shownArg(req), "tool": req.Tool,
			"reason": req.Reason, "preview": req.Preview, "description": req.Description,
		}))
		if err := L.PCall(1, 1, nil); err != nil {
			return nil, err
		}
		s, _ := lua.ToGo(L.Get(-1))
		return []any{s}, nil
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
