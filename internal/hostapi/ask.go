// ask.go is the permission gate for model tool calls: the ask hook and
// the "always" rules it offers.
package hostapi

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// askHook is the tools.Pipeline asker: veto event, then the permission
// mode (yolo allows, auto settles what autoLocal covers), then the
// Go-owned overlay and the "always" rule storage (DESIGN §6.2, §8).
func (h *Host) askHook() tools.Asker {
	return func(req tools.Request) (bool, string, string) {
		askedFor := req.Reason
		if tools.PathSubject(req.Subject) {
			// judged and shown where it lands: a symlink a sandboxed command
			// planted must not dress a write to zemi's config as a project file
			req.Arg = cmp.Or(perm.RealPath(req.Arg), req.Arg)
		}
		resolved := func(allowed bool, by string) (bool, string, string) {
			h.EmitAsync("permission.resolved", map[string]any{
				"subject": req.Subject, "arg": req.Arg,
				"decision": map[bool]string{true: "allow", false: "deny"}[allowed],
				"by":       by,
			})
			if req.Reason == askedFor {
				return allowed, by, ""
			}
			return allowed, by, req.Reason
		}
		var res VetoResult
		h.outsideReload(func() {
			res = h.EmitVetoable(context.Background(), "permission.request",
				map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason},
				lua.VetoDeadline)
		})
		if res.Denied {
			return resolved(false, "event")
		}
		def := h.permModeDef()
		if def.Rules != nil {
			o := def.Rules.Evaluate(req.Subject, req.Arg, "")
			if req.Subject == "tool.bash" {
				o = def.Rules.CheckCommand(req.Arg, "")
			}
			switch {
			case o.Decision == perm.Allow && !req.Unsandboxed && !req.Network:
				return resolved(true, "mode:"+def.Name)
			case o.Decision == perm.Deny:
				return resolved(false, "mode:"+def.Name)
			}
		}
		// a mode's own decide function (it may take seconds: a model call).
		// Anything but allow or deny leaves the ask to the user whatever the
		// base would settle, so a decide that never answers is how a user
		// asks to see every request.
		base := def.Base
		// a reason joins what the asker said (a subagent's name)
		note := func(s string) { req.Reason = strings.TrimPrefix(req.Reason+"; "+s, "; ") }
		if def.Decide != nil {
			switch h.modeDecide(def, req) {
			case "allow":
				if !req.Unsandboxed && !req.Network {
					return resolved(true, "mode:"+def.Name)
				}
			case "deny":
				return resolved(false, "mode:"+def.Name)
			}
			base = ""
		}
		switch {
		case base == "yolo":
			return resolved(true, "mode:"+def.Name)
		case req.Subject == "tool.window.text":
			// a text window runs nothing and reads nothing: it draws on the
			// screen, sanitized, so every mode settles it
			return resolved(true, "mode:"+def.Name)
		case req.Unsandboxed:
			// a mode settles a command only to run it sandboxed; the Lua tool
			// gives its own reason
			switch req.Subject {
			case "tool.bash":
				req.Reason = "run outside the sandbox"
			case "tool.window":
				req.Reason += " (not sandboxed)"
			}
		case req.Network:
			// the host is the model's, like a fetch URL, so no mode settles it
			req.Reason = "needs the network, staying sandboxed"
		case base == "plan":
			if req.Subject == "tool.bash" {
				why := h.unsettled(req.Arg)
				if why == "" {
					return resolved(true, "mode:"+def.Name)
				}
				req.Reason = strings.TrimPrefix(req.Reason+"; not settled: "+why, "; ")
			}
		case base == "auto":
			if h.autoLocal(req) {
				return resolved(true, "mode:"+def.Name)
			}
			switch {
			case req.Subject == "fs.write" && h.protectedWrite(req.Arg):
				note("not settled: this changes .git, .zemi or zemi's own files")
			case req.Subject == "fs.write" && !h.inJail(req.Arg, append(h.writeJails(), h.ScratchDir())...):
				// the sandbox cannot confine the write tool, and a file outside the
				// project (~/.gitconfig, a shell rc) can name what runs next
				note("not settled: this changes a file outside the project")
			case !h.ProjectTrusted():
				note("not settled: this project is not trusted (/trust)")
			case req.Subject == "tool.bash" && !h.sandboxLive():
				// a settled command would run unconfined
				note("not settled: no sandbox")
			case req.Subject == "tool.bash":
				note("not settled: " + h.unsettled(req.Arg))
			default:
				note("not settled: auto mode settles reads and edits inside the project")
			}
		}
		// a recipe is followed by later sessions, so say when this one could
		// carry injected text
		if req.Subject == "fs.write" && h.inJail(req.Arg, h.recipesDir()) && !h.sessionLocalOnly(h.AgentSessionID()) {
			note("this session read text from outside the machine")
		}
		ui := h.hooks()
		if ui == nil {
			// only a command's sandbox escape is a sandbox matter; a Lua
			// script or a memory edit is Unsandboxed for another reason and
			// must not be reported as one
			if req.Unsandboxed && (req.Subject == "tool.bash" || req.Subject == "tool.window") {
				return resolved(false, "unsandboxed")
			}
			if req.Network {
				return resolved(false, "network")
			}
			return resolved(false, "default")
		}
		label, rules := h.alwaysChoice(req)
		wider, wideRules := h.repoChoice(req)
		allowed, by := ui.AskPermission(PermRequest{Tool: req.Tool, Always: label, Wider: wider, Subject: req.Subject, Arg: h.shownArg(req), Reason: req.Reason, Preview: req.Preview, Description: req.Description})
		if by == "user:wider" {
			rules = wideRules
		}
		if allowed && (by == "user:always" || by == "user:wider") {
			for _, r := range rules {
				h.Perm.Add(r)
			}
		}
		return resolved(allowed, by)
	}
}

// shownArg is the request's argument as the user reads it: a path relative
// to the project when inside it.
func (h *Host) shownArg(req tools.Request) string {
	if !tools.PathSubject(req.Subject) {
		return req.Arg
	}
	if rel, ok := perm.Under(perm.RealPath(h.Dirs.ProjectDir), req.Arg); ok {
		return rel
	}
	return req.Arg
}

// alwaysChoice is the "for this session" option for a request: what it
// allows, as the prompt shows it ("allow `go test`"), and the session rules
// (model principal) accepting it adds. Subcommand programs allow their two-word prefix ("go test"), file access its
// directory, fetches their host; the rest the exact request. Gate-forced
// asks (environment assignments, heredocs) and text with a glob wildcard,
// which the rule would widen, offer no choice.
func (h *Host) alwaysChoice(req tools.Request) (string, []perm.Rule) {
	rule := func(subject, pattern string) perm.Rule {
		return perm.Rule{Subject: subject, Pattern: pattern, Decision: perm.Allow, Principal: "model"}
	}
	switch {
	case req.Subject == "tool.bash":
		n := perm.Normalize(req.Arg)
		// Opaque is an inner shell (sh -c) or a stripped assignment: what the
		// rule would match is not what the user read
		if n.Fail != "" || n.ForceAsk != "" || n.Assign != "" || n.Opaque {
			return "", nil
		}
		var names []string
		var rules []perm.Rule
		seen := map[string]bool{}
		unnamed := 0 // segments allowed exactly as written, with no prefix to name
		for i, seg := range n.Segments {
			if wrapperProgram(n.Args[i][0]) {
				return "", nil
			}
			p := commandPrefix(seg)
			if p == "" {
				if strings.ContainsAny(seg, globMeta) {
					return "", nil
				}
				rules = append(rules, rule("tool.bash", seg))
				unnamed++
				continue
			}
			rules = append(rules, rule("tool.bash", p), rule("tool.bash", p+" *"))
			if !seen[p] {
				seen[p] = true
				names = append(names, "`"+truncRunes(p, 24)+"`")
			}
		}
		// long lists name two programs and count the rest, so the label
		// still says how much it covers
		if len(names) == 0 {
			return "allow this exact command", rules
		}
		shown := names
		if len(shown) > 2 {
			shown = names[:2]
		}
		if hidden := len(names) - len(shown) + unnamed; hidden > 0 {
			return fmt.Sprintf("allow %s and %d more", strings.Join(shown, ", "), hidden), rules
		}
		return "allow " + strings.Join(shown, " and "), rules
	case req.Subject == "fs.write" || req.Subject == "fs.read":
		// relative to the cwd, as the tools open it: Dir("a.go") is "." and
		// the rule would be "*", which matches every path on disk
		abs := perm.RealPath(req.Arg)
		if abs == "" || strings.ContainsAny(abs, globMeta) {
			return "", nil
		}
		// Pipeline.Check sends every such write back to the ask, so the rule
		// would do nothing
		if req.Subject == "fs.write" && !h.inScratch(abs) && h.protectedWrite(abs) {
			return "", nil
		}
		dir := filepath.Dir(abs)
		shown := dir
		if rel, ok := perm.Under(perm.RealPath(h.Dirs.ProjectDir), dir); ok {
			shown = rel
		}
		what := map[string]string{"fs.write": "edits", "fs.read": "reads"}[req.Subject]
		where := "in " + shown + "/"
		if shown == "." {
			where = "anywhere in the project"
		}
		r := rule(req.Subject, filepath.Join(dir, "*"))
		if req.Subject == "fs.write" {
			// git runs programs .git/config and hooks name, and .gitattributes
			// or .gitmodules point it at them; dir* stands for dir and below
			r.NotMatch = filepath.Join(dir+"*", ".git*")
			// NOTE: one glob cannot tell .gitignore or .github/ from .git, so of
			// those only the approved file itself is allowed
			base := filepath.Base(abs)
			if strings.Contains(strings.ToLower(abs), string(filepath.Separator)+".git") && !inDirNamed(abs, ".git") &&
				!strings.EqualFold(base, ".gitattributes") && !strings.EqualFold(base, ".gitmodules") {
				return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{r, rule(req.Subject, abs)}
			}
		}
		return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{r}
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

// repoChoice is the wider "always" answer for a file read: its whole
// repository instead of the directory it sits in, which in a deep tree
// (internal/app/api/launchapi/) is the same ask again one package over.
// Secret-shaped files still go back to the ask (Pipeline.Check loosens a
// model rule for them), so one rule can cover the tree.
func (h *Host) repoChoice(req tools.Request) (string, []perm.Rule) {
	if req.Subject != "fs.read" {
		return "", nil
	}
	abs := perm.RealPath(req.Arg)
	if abs == "" || strings.ContainsAny(abs, globMeta) {
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
		Subject: "fs.read", Pattern: filepath.Join(root, "*"), Decision: perm.Allow, Principal: "model",
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

// wrapperPrograms run whatever is handed to them, as code (node -e) or as
// another command (sudo, xargs), so no rule written from such a segment
// says what a later call would run. An always answer offers nothing for them.
var wrapperPrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true, "fish": true, "csh": true, "tcsh": true,
	"node": true, "nodejs": true, "deno": true, "python": true, "python2": true, "python3": true,
	"perl": true, "ruby": true, "php": true, "lua": true, "luajit": true, "julia": true, "rscript": true, "osascript": true,
	"env": true, "sudo": true, "doas": true, "su": true, "xargs": true, "eval": true, "exec": true, "source": true, ".": true,
	"nohup": true, "time": true, "timeout": true, "watch": true, "setsid": true, "stdbuf": true, "script": true,
	"nice": true, "parallel": true, "ssh": true, "command": true, "builtin": true,
}

func wrapperProgram(prog string) bool {
	return wrapperPrograms[strings.ToLower(filepath.Base(prog))]
}

// commandPrefix is the two-word prefix the always answer allows for a
// command segment ("go test ./..." is "go test"), or "" when there is none.
func commandPrefix(seg string) string {
	f := strings.Fields(seg)
	if len(f) < 2 || !subcommandPrograms[f[0]] || !subcommandWord.MatchString(f[1]) {
		return ""
	}
	return f[0] + " " + f[1]
}
