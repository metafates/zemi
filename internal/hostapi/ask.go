// ask.go is the permission gate for model tool calls: the ask hook and
// the "always" rules it offers.
package hostapi

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// askHook is the tools.Pipeline asker: veto event, then the permission
// mode (yolo allows, auto settles what autoLocal covers, and neither its
// rules nor auto settle an ask a rule asked for), then the Go-owned
// overlay and the "always" rule storage (DESIGN §6.2, §8).
func (h *Host) askHook() tools.Asker {
	return func(req tools.Request) (bool, string, string) { return h.ask(req, false) }
}

// ask answers one request. Asks reach the user one at a time under askMu,
// and each is judged again once it holds the lock (held): an answer, a mode
// switch or new trust while it waited may settle it. No Lua runs under
// askMu (the held pass skips the veto and decide), so nothing on the
// interpreter can wait on it, and the UI goroutine never takes it.
// NOTE: sync.Mutex hands off in arrival order only once a waiter has
// waited over 1ms (starvation mode), so prompt order is FIFO in practice,
// not by guarantee.
func (h *Host) ask(req tools.Request, held bool) (bool, string, string) {
	entry := req
	askedFor := req.Reason
	resolved := func(allowed bool, by string) (bool, string, string) {
		// only yolo or the user allows a write that creates a plugin: it is
		// protected, so no mode rule, decide or store rule settles it
		if allowed && req.Subject == perm.SubjectWrite {
			if dir := h.newPlugin(req.Arg); dir != "" {
				h.madePlugins.Store(dir, true)
			}
		}
		h.EmitAsync("permission.resolved", map[string]any{
			"subject": req.Subject, "arg": req.Arg,
			"decision": map[bool]perm.Decision{true: perm.Allow, false: perm.Deny}[allowed],
			"by":       by,
		})
		if req.Reason == askedFor {
			return allowed, by, ""
		}
		return allowed, by, req.Reason
	}
	var res VetoResult
	var def PermModeDef
	h.outsideReload(func() {
		// a rule answer changes no handler's verdict, and audit handlers
		// would see the ask twice
		if !held {
			res = h.EmitVetoable(context.Background(), "permission.request",
				map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason},
				lua.VetoDeadline)
		}
		def = h.permModeDef()
	})
	if res.Denied {
		return resolved(false, "event")
	}
	// what a mode's rules and decide may never allow, only yolo or the user:
	// a command leaving the sandbox or reaching the network, an ask a rule
	// asked for, and a protected write, which Pipeline.Check re-asks
	// whatever rule allowed it
	settleable := !req.Unsandboxed && !req.Network && !req.Explicit &&
		!(req.Subject == perm.SubjectWrite && !req.InPad() && h.protectedWrite(req.Arg))
	if def.Rules != nil {
		o := def.Rules.Evaluate(req.Subject, req.Arg, "")
		if req.Subject == perm.SubjectBash {
			o = def.Rules.CheckCommandFor(perm.SubjectBash, req.Command(), "")
		}
		switch {
		case o.Decision == perm.Allow && settleable:
			return resolved(true, tools.ByMode(def.Name))
		case o.Decision == perm.Deny:
			return resolved(false, tools.ByMode(def.Name))
		}
	}
	// a mode's own decide function (it may take seconds: a model call).
	// Anything but allow or deny leaves the ask to the user whatever the
	// base would settle, so a decide that never answers is how a user
	// asks to see every request.
	base := def.Base
	// a reason joins what the asker said (a subagent's name)
	note := func(s string) { req.Reason = strings.TrimPrefix(req.Reason+"; "+s, "; ") }
	// what decide sent to the user stays with the user
	if def.Decide != nil && !held {
		switch h.modeDecide(def, req) {
		case "allow":
			if settleable {
				return resolved(true, tools.ByMode(def.Name))
			}
		case "deny":
			return resolved(false, tools.ByMode(def.Name))
		}
	}
	if def.Decide != nil {
		base = ""
	}
	switch {
	case base == "yolo" && req.ReadOnly && (req.Subject == perm.SubjectBash && !h.sandboxLive() || strings.HasPrefix(req.Subject, "mcp.")) && !h.autoLocal(req):
		note("read-only agent: nothing confines this call")
	case base == "yolo":
		return resolved(true, tools.ByMode(def.Name))
	case req.Unsandboxed:
		// a mode settles a command only to run it sandboxed; the Lua tool
		// gives its own reason
		if req.Subject == perm.SubjectBash {
			req.Reason = "run outside the sandbox"
		}
	case req.Network:
		// the host is the model's, like a fetch URL, so no mode settles it
		req.Reason = "needs the network, staying sandboxed"
	case req.Explicit:
		// whoever wrote the rule wanted to see this; only yolo overrides it
		note("a rule asks for this")
	case base == "auto":
		if h.autoLocal(req) {
			return resolved(true, tools.ByMode(def.Name))
		}
		why := ""
		if req.Subject == perm.SubjectWrite && !req.InPad() {
			why = h.protectedWhy(req.Arg)
		}
		switch {
		case why != "":
			note(why)
		case tools.PathSubject(req.Subject) && h.secretPath(req.Arg):
			note("this file looks like it holds secrets")
		case strings.HasPrefix(req.Subject, "net."):
			note("a fetch always asks: the model chose the address")
		case req.Subject == perm.SubjectWrite && !req.InPad() && !h.inJail(req.Arg, h.writeJails()...):
			// the sandbox cannot confine the write tool, and a file outside the
			// project (~/.gitconfig, a shell rc) can name what runs next; the
			// write jail is the launch dir even in a trusted repository (the
			// sandbox's write root), so a sibling package says which dir
			if launch := h.launchDirInRepo(); launch != "" && h.inJail(req.Arg, h.Dirs.RepoRoot()) {
				note("this changes a file outside " + launch + ", the directory zemi was started in")
			} else {
				note("this changes a file outside the project")
			}
		case !h.ProjectTrusted():
			note("this project is not trusted (/trust)")
		case req.Subject == perm.SubjectBash && !h.sandboxLive():
			// a settled command would run unconfined
			note("no sandbox")
		case req.Subject == perm.SubjectBash:
			note(h.unsettled(req))
		default:
			note("auto mode settles reads and edits inside the project")
		}
	}
	// a recipe is followed by later sessions, so say when this one could
	// carry injected text
	if req.Subject == perm.SubjectWrite && h.inJail(req.Arg, h.recipesDir()) && !h.sessionLocalOnly(h.AgentSessionID()) {
		note("this session read text from outside the machine")
	}
	ui := h.hooks()
	if ui == nil {
		d := Denial{Tool: req.Tool, Subject: req.Subject, Arg: h.shownArg(req), Reason: req.Reason, Why: DenyApproval}
		// only a command's sandbox escape is a sandbox matter; a Lua script
		// or a memory edit is Unsandboxed for another reason
		switch {
		case req.Unsandboxed && req.Subject == perm.SubjectBash:
			d.Why = DenyOutsideSandbox
		case req.Network:
			d.Why = DenyNetwork
		case req.Tool == tools.ToolLua || req.Tool == tools.ToolMemory:
			d.Why = DenyNeedsTUI
		}
		h.modes.noteHeadless(d)
		return resolved(false, tools.ByNobody)
	}
	if !held {
		cut := h.askCut.Load()
		h.asksWaiting.Add(1)
		h.askMu.Lock()
		defer func() {
			h.asksWaiting.Add(-1)
			h.askMu.Unlock()
			ui.Refresh() // drops an answered prompt no other ask replaced
		}()
		if h.askCut.Load() != cut {
			return resolved(false, tools.ByUser) // told zemi what to do instead
		}
		v, fresh := req.Recheck()
		if v.Decision != perm.Ask {
			resolved(v.Decision == perm.Allow, v.DecidedBy)
			return false, tools.ByRecheck, ""
		}
		// the fresh judgement's flags: a rule added while it waited may ask
		entry.Explicit, entry.ReadOnly = fresh.Explicit, fresh.ReadOnly
		return h.ask(entry, true)
	}
	label, rules := h.alwaysChoice(req)
	wider, wideRules := h.repoChoice(req)
	project, projectRules := h.projectChoice(req, label, rules)
	allowed, by := ui.AskPermission(PermRequest{Tool: req.Tool, Always: label, Wider: wider, Project: project, Subject: req.Subject, Arg: h.shownArg(req), Reason: req.Reason, Preview: req.Preview, Description: req.Description, Outside: req.Unsandboxed && req.Subject == perm.SubjectBash})
	if by == "user:wider" {
		rules = wideRules
	}
	if allowed && (by == "user:always" || by == "user:wider") {
		for _, r := range rules {
			h.Perm.Add(r)
		}
	}
	if allowed && by == "user:project" {
		h.projectAllow(req.Arg, projectRules)
	}
	return resolved(allowed, by)
}

// AsksWaiting is how many asks are at the prompt stage, the one shown
// included; some may settle without a prompt.
func (h *Host) AsksWaiting() int { return int(h.asksWaiting.Load()) }

// DenyWaitingAsks denies every ask waiting for the prompt now, as the user.
func (h *Host) DenyWaitingAsks() { h.askCut.Add(1) }

// shownArg is the request's argument as the user reads it: a path relative
// to the project when inside it, or to the trusted repository's root, else
// with ~ for the home directory.
func (h *Host) shownArg(req tools.Request) string {
	if !tools.PathSubject(req.Subject) {
		return req.Arg
	}
	return TildePath(h.relPath(req.Arg))
}

// relPath is path relative to the project, else to the trusted
// repository's root ("<root>/pkg/a.go" is "pkg/a.go" from "<root>/other"),
// else as given.
func (h *Host) relPath(path string) string {
	roots := []string{h.Dirs.ProjectDir}
	if h.ProjectTrusted() {
		roots = append(roots, h.Dirs.RepoRoot())
	}
	for _, root := range roots {
		// as given, then resolved (/var is /private/var on macOS)
		for _, p := range []string{path, perm.RealPath(path)} {
			if rel, ok := perm.Under(perm.RealPath(root), p); ok {
				return rel
			}
		}
	}
	return path
}

// launchDirInRepo is the launch dir relative to the trusted repository's
// root, with a trailing separator ("pkg/"), or "" when it is the root or
// the project is untrusted.
func (h *Host) launchDirInRepo() string {
	if !h.ProjectTrusted() {
		return ""
	}
	rel, ok := perm.Under(perm.RealPath(h.Dirs.RepoRoot()), perm.RealPath(h.Dirs.ProjectDir))
	if !ok || rel == "." {
		return ""
	}
	return rel + string(filepath.Separator)
}

// alwaysChoice is the "for this session" option for a request: what it
// allows, as the prompt shows it ("allow go test"), and the session rules
// (model principal) accepting it adds. A command is allowed as a whole (its
// program, plus the subcommand when it has one) whatever the arguments,
// since arguments differ every run; file access its directory, fetches
// their host; the rest the exact request. Deleting programs and text with a
// glob wildcard stay exact or offer no choice, so the rule never widens to
// a command the user did not see.
func (h *Host) alwaysChoice(req tools.Request) (string, []perm.Rule) {
	rule := func(subject, pattern string) perm.Rule {
		return perm.Rule{Subject: subject, Pattern: pattern, Decision: perm.Allow, Principal: perm.PrincipalModel}
	}
	switch {
	case req.Subject == perm.SubjectBash:
		n := req.Command()
		// Opaque is an inner shell (sh -c) or a stripped assignment: what the
		// rule would match is not what the user read
		if n.Fail != "" || n.ForceAsk != "" || n.Assign != "" || n.Opaque {
			return "", nil
		}
		var names []string
		var rules []perm.Rule
		seen := map[string]bool{}
		unnamed := 0 // segments allowed exactly as written, named only by count
		for _, seg := range n.Segments {
			// a segment this manager already lets through, by a session rule or
			// a user rule, is not new: add neither a name nor a rule for it
			if h.Perm.CheckCommand(seg, perm.PrincipalModel).Decision == perm.Allow {
				continue
			}
			patterns, ok := bashAlwaysPatterns(seg)
			if !ok {
				return "", nil
			}
			for _, p := range patterns {
				r := rule(perm.SubjectBash, p)
				// the ask said "staying sandboxed", so the rule keeps it
				r.Network = req.Network
				rules = append(rules, r)
			}
			// one pattern is the exact form of a deleting command, named only
			// by count; two (p, "p *") is a command allowed as a whole
			if len(patterns) == 1 {
				unnamed++
				continue
			}
			p := patterns[0]
			if !seen[p] {
				seen[p] = true
				names = append(names, p)
			}
		}
		if len(rules) == 0 {
			return "", nil // every segment is allowed already
		}
		// a rule-allowed command runs unconfined, a network one keeps the
		// sandbox and adds sockets; the label says which, under a read-only
		// agent too, since the rule outlives its turn
		effect := ""
		switch {
		case req.Network:
			effect = " with the network"
		case h.sandboxLive():
			effect = " outside the sandbox"
		}
		// long lists name two programs and count the rest, so the label
		// still says how much it covers
		if len(names) == 0 {
			return "allow this exact command" + effect, rules
		}
		shown := names
		if len(shown) > 2 {
			shown = names[:2]
		}
		if hidden := len(names) - len(shown) + unnamed; hidden > 0 {
			return fmt.Sprintf("allow %s and %d more%s", strings.Join(shown, ", "), hidden, effect), rules
		}
		return "allow " + strings.Join(shown, " and ") + effect, rules
	case req.Subject == perm.SubjectWrite || req.Subject == perm.SubjectRead:
		// relative to the cwd, as the tools open it: Dir("a.go") is "." and
		// the rule would be "*", which matches every path on disk
		abs := perm.RealPath(req.Arg)
		if abs == "" || strings.ContainsAny(abs, globMeta) {
			return "", nil
		}
		// Pipeline.Check sends every such write back to the ask, so the rule
		// would do nothing
		if req.Subject == perm.SubjectWrite && !req.InPad() && h.protectedWrite(abs) {
			return "", nil
		}
		what := map[string]string{perm.SubjectWrite: "edits", perm.SubjectRead: "reads"}[req.Subject]
		// a directory glob would cover the next secret too, and Check re-asks
		// for it; the one file the user saw is safe to allow
		dir := filepath.Dir(abs)
		// home, / and the dirs between hold every dotfile, so a directory rule
		// would cover ~/.ssh and ~/Library/LaunchAgents too
		home, _ := os.UserHomeDir()
		_, aboveHome := perm.Under(dir, perm.RealPath(home))
		if h.secretPath(abs) || dir == "/" || home != "" && aboveHome {
			return fmt.Sprintf("allow %s of %s", what, TildePath(h.relPath(abs))), []perm.Rule{rule(req.Subject, abs)}
		}
		shown := TildePath(h.relPath(dir))
		// the rule's * spans separators, so the label says so
		where := "in " + shown + "/ and below"
		if shown == "." {
			where = "anywhere in the project"
		}
		return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{rule(req.Subject, filepath.Join(dir, "*"))}
	case req.Subject == "tool.workflow" && req.Arg == "script", req.Subject == "tool.lua":
		return "", nil // a pattern for "script" would allow every future inline script
	case strings.HasPrefix(req.Subject, "net."):
		if u, err := url.Parse(req.Arg); err == nil && u.Host != "" && !strings.ContainsAny(u.Host, globMeta) {
			base := u.Scheme + "://" + u.Host
			return "allow " + u.Host, []perm.Rule{rule(req.Subject, base), rule(req.Subject, base+"/*")}
		}
	}
	if strings.ContainsAny(req.Arg, globMeta) {
		return "", nil
	}
	return "allow this", []perm.Rule{rule(req.Subject, req.Arg)}
}

// bashAlwaysPatterns turns one command segment into the allow-lookup patterns
// the "always" answer (and a project permissions file) uses: it allows the
// command as a whole, its program plus the subcommand when it has one,
// whatever the arguments, since arguments differ every run. A deleting program
// stays exact, and a wrapper program (sh -c, node -e) or a glob offers
// nothing: a rule from those would widen to a command the user did not see.
// ok is false when there is no safe rule.
func bashAlwaysPatterns(seg string) (patterns []string, ok bool) {
	if strings.ContainsAny(seg, globMeta) {
		return nil, false
	}
	f := strings.Fields(seg)
	if len(f) == 0 {
		return nil, false
	}
	if wrapperProgram(f[0]) {
		return nil, false
	}
	// npm exec, uv run and the like run a package named after them, as npx
	// does; past an option whose value is unknown, any later word may be the
	// subcommand
	i := perm.Subcommand(f)
	known := i < 0 || perm.LeadKnown(f, i)
	if i > 0 {
		words := f[i : i+1]
		if !known {
			words = f[i:]
		}
		if slices.ContainsFunc(words, func(w string) bool { return wrapperSubcommands[programName(f[0])+" "+w] }) {
			return nil, false
		}
	}
	// a program or subcommand that can delete or overwrite must not broaden
	// to arbitrary later targets: the rule runs unconfined, and such a
	// command has no write path to re-ask on
	if destructiveProgram(programName(f[0])) || perm.Normalize(seg).Destructive("") != "" {
		return []string{seg}, true
	}
	p := commandPrefix(seg)
	switch {
	case p != "" && known:
	case subcommandPrograms[programName(f[0])]:
		// git --version and a bare git have no subcommand, and npm -w web test
		// has none for sure; `git *` or `npm -w web *` would allow every one
		return []string{seg}, true
	default:
		p = f[0]
	}
	return []string{p, p + " *"}, true
}

// projectChoice is the ask's "for this project" answer: the session always
// label kept, the rules the permissions file loads from the command allowed
// un-scoped now, and the command persisted to the file so it applies again
// on every boot. Empty when the project is untrusted (the file is only read
// then), for a non-bash request, when the command has no safe rule
// (alwaysLabel is ""), for a multi-line command (each file line loads as its
// own command, so a line continuation there would widen what the user
// approved), for a network ask (a file line runs unconfined, and the ask
// said the command stays sandboxed), and when the file would load more than the label names: the
// session answer skips a segment a model rule already allows, but the file
// outlives that rule.
func (h *Host) projectChoice(req tools.Request, alwaysLabel string, alwaysRules []perm.Rule) (string, []perm.Rule) {
	// a file line runs unconfined, which a network ask promised not to
	if req.Subject != perm.SubjectBash || req.Network || !h.ProjectTrusted() || alwaysLabel == "" || strings.ContainsAny(req.Arg, "\n") {
		return "", nil
	}
	patterns, ok := bashRulePatterns(req.Command())
	var rules []perm.Rule
	for _, ps := range patterns {
		for _, p := range ps {
			rules = append(rules, perm.Rule{Subject: perm.SubjectBash, Pattern: p, Decision: perm.Allow})
		}
	}
	if !ok || len(rules) != len(alwaysRules) {
		return "", nil
	}
	return alwaysLabel, rules
}

// repoChoice is the wider "always" answer for a file read: its whole
// repository instead of the directory it sits in, which in a deep tree
// (internal/app/api/launchapi/) is the same ask again one package over.
// Secret-shaped files still go back to the ask (Pipeline.Check loosens a
// model rule for them), so one rule can cover the tree.
func (h *Host) repoChoice(req tools.Request) (string, []perm.Rule) {
	if req.Subject != perm.SubjectRead {
		return "", nil
	}
	abs := perm.RealPath(req.Arg)
	if abs == "" || strings.ContainsAny(abs, globMeta) || h.secretPath(abs) {
		return "", nil
	}
	root := config.Dirs{ProjectDir: filepath.Dir(abs)}.RepoRoot()
	// RepoRoot answers the directory itself when it finds no .git above it,
	// and then the directory choice already covers everything this would
	if root == filepath.Dir(abs) {
		return "", nil
	}
	shown := root + string(filepath.Separator)
	if rel, ok := perm.Under(perm.RealPath(h.Dirs.ProjectDir), root); ok {
		shown = "the project"
		if rel != "." {
			shown = rel + string(filepath.Separator)
		}
	}
	return "allow reads anywhere in " + shown, []perm.Rule{{
		Subject: perm.SubjectRead, Pattern: filepath.Join(root, "*"), Decision: perm.Allow, Principal: perm.PrincipalModel,
	}}
}

// globMeta are perm's glob wildcards; its patterns have no escape
const globMeta = "*?"

var subcommandWord = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// programs whose second word is a subcommand; for any other it may be a
// file ("cat notes"), and the prefix would allow any file after it
var subcommandPrograms = map[string]bool{
	"git": true, "go": true, "cargo": true, "npm": true, "pnpm": true, "yarn": true, "bun": true,
	"make": true, "docker": true, "kubectl": true, "gh": true, "uv": true,
}

// wrapperSubcommands are the subcommand spellings of npx and uvx: the pair
// runs whatever package follows, so an always answer offers nothing for it
var wrapperSubcommands = map[string]bool{
	"npm exec": true, "npm x": true, "pnpm exec": true, "pnpm dlx": true, "yarn dlx": true, "bun x": true,
	"uv run": true, "uv tool": true,
}

func wrapperProgram(prog string) bool {
	return perm.WrapperPrograms[programName(prog)]
}

// programName is the name a command word is judged by: the pattern keeps
// the word as written, so `/usr/bin/git push` still allows only git push.
func programName(word string) string {
	return strings.ToLower(filepath.Base(word))
}

// destructivePrograms are programs that, with one more argument, can delete
// or overwrite without a write path the pipeline would re-ask on (rm and
// friends delete by syscall, find -exec and rsync run real deleters). An
// always-allow keeps a command by one of them exact.
var destructivePrograms = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "mv": true, "cp": true, "install": true,
	"find": true, "tar": true, "rsync": true, "shred": true, "truncate": true, "dd": true,
	"ln": true, "tee": true, "chmod": true, "chown": true, "chgrp": true,
	"sed": true, // -i and the w command overwrite, like tee
}

func destructiveProgram(prog string) bool {
	return destructivePrograms[prog]
}

// commandPrefix is the prefix through the subcommand the always answer
// allows for a command segment ("go test ./..." is "go test", "git -C d
// status" keeps its options), or "" when there is none.
func commandPrefix(seg string) string {
	f := strings.Fields(seg)
	i := perm.Subcommand(f)
	if !subcommandPrograms[programName(f[0])] || i < 0 || !subcommandWord.MatchString(f[i]) {
		return ""
	}
	return strings.Join(f[:i+1], " ")
}
