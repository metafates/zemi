// ask.go is the permission gate for model tool calls: the ask hook and
// the "always" rules it offers.

package hostapi

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
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
				map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason, "agent": req.Agent})
		}
		def = h.permModeDef()
	})
	if res.Denied {
		return resolved(false, "event")
	}
	// what a mode's rules may never settle, yolo or the user alone: a command
	// leaving the sandbox or reaching the network, an ask a rule asked for, and
	// a protected write, which Pipeline.Check re-asks whatever rule allowed it.
	// A decide may settle one, since a model call is what writing the mode
	// asked for, and the row it adds says which (fencedWhy).
	fenced := h.fencedWhy(req)
	if def.Rules != nil {
		o := def.Rules.Evaluate(req.Subject, req.Arg, "")
		if req.Subject == perm.SubjectShell {
			o = def.Rules.CheckCommandFor(perm.SubjectShell, req.Command(), "")
		}
		switch {
		case o.Decision == perm.Allow && fenced == "":
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
	// a reason joins what the asker said
	note := func(s string) { req.Reason = strings.TrimPrefix(req.Reason+"; "+s, "; ") }
	// what the request may be answered with beyond "once": the decide's payload
	// says which scopes are on offer, and the prompt offers them; an ask the
	// mode's rules or base settles needs neither
	var ch askChoices
	if def.Decide != nil && !held {
		ch = h.choices(req)
		ans := h.modeDecide(def, h.decidePayload(req, ch.scopes()))
		switch ans.Decision {
		case "allow":
			// nothing else in the transcript says a mode allowed what only yolo
			// or the user could before, and this one left the sandbox or asked
			// for the network
			if fenced != "" {
				text := def.Name + " allowed " + fenced + ": " + req.Subject + " " + h.shownArg(req)
				if ans.Reason != "" {
					text += " (" + sanitize.Line(ans.Reason) + ")"
				}
				h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: sanitize.String(text)})
			}
			// a mode below user origin may settle the ask and write session
			// rules, but the durable project file is the user's own grant, as an
			// allow rule is (rule.add): its "project" answer narrows to session
			if def.Origin != lua.OriginUser && ans.Scope == "project" {
				ans.Scope = "session"
			}
			h.grant(req, ch, ans.Scope)
			return resolved(true, tools.ByMode(def.Name))
		case "deny":
			if ans.Reason != "" {
				req.Reason = sanitize.Line(ans.Reason)
			}
			return resolved(false, tools.ByMode(def.Name))
		}
	}
	if def.Decide != nil {
		base = ""
	}
	// a protected write names its reason in every mode that reaches the
	// user, not only under auto (the .git class shares it)
	why := ""
	if req.Subject == perm.SubjectWrite && !req.InPad() {
		why = h.protectedWhy(req.Arg)
	}
	if why != "" && base != config.PermModeAuto {
		note(why)
	}
	switch {
	case base == config.PermModeYolo && req.ReadOnly && (req.Subject == perm.SubjectShell && !h.sandboxLive() || strings.HasPrefix(req.Subject, "mcp.")) && !h.autoLocal(req):
		note("read-only agent: nothing confines this call")
	case base == config.PermModeYolo:
		return resolved(true, tools.ByMode(def.Name))
	case req.Unsandboxed:
		// a mode settles a command only to run it sandboxed; the Lua tool
		// gives its own reason
		if req.Subject == perm.SubjectShell {
			req.Reason = "the model asked to leave the sandbox"
		}
	case req.Network:
		// the host is the model's, like a fetch URL, so no mode's rules settle it
		req.Reason = "the model asked for the network"
	case req.Explicit:
		// whoever wrote the rule wanted to see this; only yolo overrides it
		note("a rule asks for this")
	case base == config.PermModeAuto:
		if h.autoLocal(req) {
			return resolved(true, tools.ByMode(def.Name))
		}
		switch {
		case why != "":
			note(why)
		case req.FreeZone() && strings.Contains(req.Reason, tools.FreeSecret):
			// Check's reason says why already
		case tools.PathSubject(req.Subject) && h.secretPath(req.Arg):
			note("this file looks like it holds secrets")
		case strings.HasPrefix(req.Subject, "net.") && req.Tool == tools.ToolWebSearch:
			note("a search always asks: the model chose the query")
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
			note("this project is not trusted (" + h.trustHint() + ")")
		case req.Subject == perm.SubjectShell && !h.sandboxLive():
			// a settled command would run unconfined
			note("no sandbox")
		case req.Subject == perm.SubjectShell:
			note(h.unsettled(req))
		case tools.PathSubject(req.Subject):
			note("this file is outside the project")
		case strings.HasPrefix(req.Subject, "mcp."):
			note(strings.TrimPrefix(req.Subject, "mcp.") + " does not mark " + req.Arg + " read-only")
		default:
			note("zemi cannot tell what this tool does")
		}
	}
	// a recipe is followed by later sessions, so say when this one could
	// carry injected text; a brain file is read by later sessions too (DESIGN §10.4)
	user, private := h.brainParts()
	if req.Subject == perm.SubjectWrite && h.inJail(req.Arg, h.recipesDir(), user.Dir, private.Dir) && !h.sessionLocalOnly(h.AgentSessionID()) {
		note("this session read text from outside the machine")
	}
	// the reserved file's open findings ride the ask, so the user approves
	// the write seeing what the store already flags (DESIGN §10.4)
	if req.Subject == perm.SubjectWrite && !req.InPad() {
		for _, s := range h.memLintNotes(req.Arg) {
			note(s)
		}
	}
	ui := h.hooks()
	if ui == nil {
		why := req.Reason
		if req.Agent != "" {
			why = strings.TrimSuffix(req.Agent+": "+why, ": ")
		}
		d := Denial{Tool: req.Tool, Subject: req.Subject, Arg: sanitize.String(h.shownArg(req)), Reason: sanitize.String(why), Why: DenyApproval}
		// only a command's sandbox escape is a sandbox matter; a Lua script
		// or a memory edit is Unsandboxed for another reason
		switch {
		case req.Unsandboxed && req.Subject == perm.SubjectShell:
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
	ch = h.choices(req)
	pr := PermRequest{
		Tool: req.Tool, Always: ch.always, Wider: ch.wider, Project: ch.project != "", Subject: req.Subject, Arg: h.shownArg(req),
		Reason: req.Reason, Preview: req.Preview, Description: req.Description, Agent: req.Agent, Owner: req.Owner, Where: h.runsIn(req),
	}
	// a call a lua tool's script drove: the user read the script, so name it
	if req.Script != "" {
		pr.Script = "lua script"
	}
	if req.Subject == perm.SubjectShell {
		pr.Runs = req.Command().Programs(req.Pad)
	}
	allowed, by := ui.AskPermission(pr)
	if allowed {
		switch by {
		case "user:always":
			h.grant(req, ch, "session")
		case "user:wider":
			h.grant(req, ch, "wider")
		case "user:project":
			h.grant(req, ch, "project")
		}
	}
	return resolved(allowed, by)
}

// askChoices is what a permission request may be answered with beyond "once":
// the session rules an "always" answer writes, the wider read rule, and the
// project permissions file's lines. The prompt offers them, and a mode's decide
// may take one (askHook).
type askChoices struct {
	always, wider, project string
	rules, wide, file      []perm.Rule
}

func (h *Host) choices(req tools.Request) askChoices {
	c := askChoices{}
	c.always, c.rules = h.alwaysChoice(req)
	c.wider, c.wide = h.repoChoice(req)
	c.project, c.file = h.projectChoice(req, c.always, c.rules)
	return c
}

// scopes lists what an answer to this request may take, "once" first; a scope
// left out is not on offer here.
func (c askChoices) scopes() []string {
	out := []string{"once"}
	if len(c.rules) > 0 {
		out = append(out, "session")
	}
	if c.wider != "" {
		out = append(out, "wider")
	}
	if c.project != "" {
		out = append(out, "project")
	}
	return out
}

// grant adds what an answer of that scope writes: the project permissions file,
// the wider read rule or the session rules an "always" answer writes. A scope
// this request does not offer narrows to the closest one below it, never wider.
func (h *Host) grant(req tools.Request, c askChoices, scope string) {
	switch {
	case scope == "project" && c.project != "":
		h.projectAllow(req.Arg, c.file)
	case scope == "wider" && c.wider != "":
		for _, r := range c.wide {
			h.Perm.Add(r)
		}
	case scope == "session" || scope == "wider" || scope == "project":
		for _, r := range c.rules {
			h.Perm.Add(r)
		}
	}
}

// fencedWhy says why a request is one no mode rule may settle, "" when one may:
// a command leaving the sandbox or reaching the network, an ask a rule asked
// for, and a protected write, which Pipeline.Check re-asks whatever a rule
// allowed. Only yolo or the user answers one, and a decide that does says so.
func (h *Host) fencedWhy(req tools.Request) string {
	switch {
	case req.Unsandboxed:
		return "a call outside the sandbox"
	case req.Network:
		return "a call asking for the network"
	case req.Explicit:
		return "a call a rule asked about"
	case req.Subject == perm.SubjectWrite && !req.InPad() && h.protectedWrite(req.Arg):
		return "a protected write (" + h.protectedWhy(req.Arg) + ")"
	}
	return ""
}

// runsIn is where a yes runs the command, as Pipeline.SandboxFor decides:
// unconfined unless it asked for the network or a read-only agent runs it.
func (h *Host) runsIn(req tools.Request) RunsIn {
	switch {
	case req.Subject != perm.SubjectShell:
		return ""
	case !h.sandboxLive():
		return RunsNoSandbox
	case req.Unsandboxed:
		return RunsOutside
	case req.Network:
		return RunsNetwork
	case req.ReadOnly:
		return RunsConfined
	}
	return RunsOutside
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
	return perm.TildePath(h.relPath(req.Arg))
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
	case req.Subject == perm.SubjectShell:
		n := req.Command()
		// Opaque is an inner shell (sh -c) or a stripped assignment: what the
		// rule would match is not what the user read
		if n.Fail != "" || n.ForceAsk != "" || n.Assign != "" || n.Opaque {
			return "", nil
		}
		var names, syntax, deleters []string
		var rules []perm.Rule
		seen := map[string]bool{}
		unnamed := 0 // segments allowed exactly as written, named only by count
		for _, seg := range n.Rows() {
			// a segment this manager already lets through, by a session rule or
			// a user rule, is not new: add neither a name nor a rule for it
			if h.Perm.CheckCommandFor(perm.SubjectShell, seg, perm.PrincipalModel).Decision == perm.Allow {
				continue
			}
			patterns, ok := shellAlwaysPatterns(seg)
			if !ok {
				return "", nil
			}
			for _, p := range patterns {
				r := rule(perm.SubjectShell, p)
				// the ask said the run stays sandboxed, so the rule keeps it
				r.Network = req.Network
				rules = append(rules, r)
			}
			// a no-op builtin (true, set) is named only when nothing else is;
			// one pattern is an exact form, named first when it deletes, so the
			// answer never hides it in a count; two (p, "p *") is a command
			// allowed as a whole
			prog := seg.Args[0][0]
			switch {
			case perm.ShellWords[prog] && len(patterns) > 1:
				if !seen[prog] {
					seen[prog] = true
					syntax = append(syntax, prog)
				}
				continue
			case len(patterns) == 1 && deletes(seg):
				if d := perm.ProgramName(prog) + " (exact)"; !seen[d] {
					seen[d] = true
					deleters = append(deleters, d)
				}
				continue
			case len(patterns) == 1:
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
			names = syntax
		}
		if len(names) == 0 {
			return "allow this exact command" + effect, rules
		}
		names = append(deleters, names...)
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
			return fmt.Sprintf("allow %s of %s", what, perm.TildePath(h.relPath(abs))), []perm.Rule{rule(req.Subject, abs)}
		}
		shown := perm.TildePath(h.relPath(dir))
		// the rule's * spans separators, so the label says so
		where := "in " + shown + "/ and below"
		if shown == "." {
			where = "anywhere in the project"
		}
		return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{rule(req.Subject, filepath.Join(dir, "*"))}
	case req.Subject == perm.SubjectWorkflow && req.Arg == "script", req.Subject == perm.SubjectLua:
		return "", nil // a pattern for "script" would allow every future inline script
	case req.Subject == "memory.write":
		return "", nil // a memory is forgotten once, so its rule would never match again
	case strings.HasPrefix(req.Subject, "net."):
		if u, err := url.Parse(req.Arg); err == nil && u.Host != "" && !strings.ContainsAny(u.Host, globMeta) {
			base := u.Scheme + "://" + u.Host
			return "allow " + u.Host, []perm.Rule{rule(req.Subject, base), rule(req.Subject, base+"/*")}
		}
	}
	if strings.ContainsAny(req.Arg, globMeta) {
		return "", nil
	}
	label := "allow this"
	if server, ok := strings.CutPrefix(req.Subject, "mcp."); ok {
		label = "allow " + req.Arg + " from " + server
	} else if req.Subject == "tool."+req.Arg {
		label = "allow the " + req.Arg + " tool" // a Lua tool
	}
	return label, []perm.Rule{rule(req.Subject, req.Arg)}
}

// deletes reports a segment that can delete or overwrite, whose always rule
// stays exact.
func deletes(seg perm.Normalized) bool {
	return destructivePrograms[perm.ProgramName(seg.Args[0][0])] || seg.Destructive("") != ""
}

// shellAlwaysPatterns turns one command segment into the allow-lookup patterns
// the "always" answer (and a project permissions file) uses: it allows the
// command as a whole, its program plus the subcommand when it has one,
// whatever the arguments, since arguments differ every run. A deleting program
// stays exact, and a wrapper program (sh -c, node -e) or a glob offers
// nothing: a rule from those would widen to a command the user did not see.
// ok is false when there is no safe rule.
func shellAlwaysPatterns(row perm.Normalized) (patterns []string, ok bool) {
	seg, f := row.Segments[0], row.Args[0]
	if strings.ContainsAny(seg, globMeta) {
		return nil, false
	}
	// judged by its name, kept as written: a rule for `/usr/bin/git push`
	// allows only that git
	prog := perm.ProgramName(f[0])
	if perm.WrapperPrograms[prog] {
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
		if slices.ContainsFunc(words, func(w string) bool { return wrapperSubcommands[prog+" "+w] }) {
			return nil, false
		}
	}
	// a program or subcommand that can delete or overwrite must not broaden
	// to arbitrary later targets: the rule runs unconfined, and such a
	// command has no write path to re-ask on
	// a builtin that sets variables by name evaluates a quoted subscript,
	// where $(...) runs. An expansion is no exact text: rm $f deletes what a
	// loop hands it
	if perm.ShellState[f[0]] || deletes(row) {
		if strings.ContainsAny(seg, "$`") {
			return nil, false
		}
		return []string{seg}, true
	}
	p := commandPrefix(f)
	switch {
	case p != "" && known:
	case perm.SubcommandPrograms[prog]:
		// git --version and a bare git have no subcommand, and npm -w web test
		// has none for sure; `git *` or `npm -w web *` would allow every one
		return []string{seg}, true
	default:
		p = f[0]
	}
	return []string{p, p + " *"}, true
}

// projectChoice is the ask's "for this project" answer: the always label,
// and the rules the permissions file will load for the command. None in an
// untrusted project (the file is not read), for a line continuation (each
// file line loads as its own command), for a network ask (a file line runs
// unconfined), or when the file would load more than the label names: the
// session answer skips a segment a model rule allows, the file outlives it.
func (h *Host) projectChoice(req tools.Request, alwaysLabel string, alwaysRules []perm.Rule) (string, []perm.Rule) {
	if req.Subject != perm.SubjectShell || req.Network || !h.ProjectTrusted() || alwaysLabel == "" || strings.ContainsAny(req.Arg, "\n") {
		return "", nil
	}
	rules, ok := projectRules(req.Command())
	if !ok || len(rules) != len(alwaysRules) {
		return "", nil
	}
	return alwaysLabel, rules
}

// repoChoice is the wider "always" answer for a file read: its whole
// repository instead of the directory it sits in, which in a deep tree
// (internal/app/api/launchapi/) is the same ask again one package over.
// Secret-shaped files still go back to the ask (Pipeline.Check loosens a
// model rule for them while the run hides secrets), so one rule can cover
// the tree.
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

// wrapperSubcommands are the subcommand spellings of npx and uvx: the pair
// runs whatever package follows, so an always answer offers nothing for it
var wrapperSubcommands = map[string]bool{
	"npm exec": true, "npm x": true, "pnpm exec": true, "pnpm dlx": true, "yarn dlx": true, "bun x": true,
	"uv run": true, "uv tool": true,
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

// commandPrefix is the prefix through the subcommand the always answer
// allows for a segment's words ("go test ./..." is "go test", "git -C d
// status" keeps its options), or "" when there is none.
func commandPrefix(f []string) string {
	i := perm.SubcommandAt(f)
	if i < 0 {
		return ""
	}
	return strings.Join(f[:i+1], " ")
}
