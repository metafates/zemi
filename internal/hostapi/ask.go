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

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// askHook is the tools.Pipeline asker: veto event, then the permission
// mode (yolo allows, auto lets a model approve routine requests), then the
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
		switch {
		case def.Base == "yolo":
			return resolved(true, "mode:"+def.Name)
		case req.Subject == "tool.window.text" && def.Base != "manual":
			// a text window runs nothing and reads nothing: it draws on the
			// screen, sanitized, so only manual mode asks about it
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
		case def.Base == "plan":
			if req.Subject == "tool.bash" && h.settledCommand(req.Arg) {
				return resolved(true, "mode:"+def.Name)
			}
			if req.Subject == "tool.bash" {
				if note := h.repoNote(req.Arg); note != "" {
					req.Reason = strings.TrimPrefix(req.Reason+"; not settled in plan mode: "+note, "; ")
				}
			}
		case def.Base == "auto":
			// everything the screener saw: an MCP or Lua tool's Arg is only its
			// name, the input it approved is in Preview, and the turn's request
			// is why it may have approved at all
			key := strings.Join([]string{req.Subject, req.Arg, req.Reason, req.Preview, h.turnRequest()}, "\x00")
			if h.autoLocal(req) {
				return resolved(true, "mode:"+def.Name)
			}
			if req.Subject == "fs.write" && h.protectedWrite(req.Arg) {
				// what zemi allows and what git runs: never the screener's call
				req.Reason = "not settled: this changes .git, .zemi or zemi's own files"
				break
			}
			if req.Subject == "fs.write" && !h.inJail(req.Arg, append(h.writeJails(), h.ScratchDir())...) {
				// the sandbox cannot confine the write tool, and a file outside the
				// project (~/.gitconfig, a shell rc) can name what runs next
				req.Reason = "not settled: this changes a file outside the project"
				break
			}
			if !h.ProjectTrusted() {
				// the screener reads text the repository can steer; the user decides
				req.Reason = "not settled: this project is not trusted (/trust)"
				break
			}
			if req.Subject == "tool.bash" && !h.sandboxLive() {
				// a screened command would run unconfined
				req.Reason = "not settled: no sandbox"
				break
			}
			if _, ok := h.autoApproved.Load(key); ok {
				return resolved(true, "mode:"+def.Name)
			}
			ok, why := h.autoApprove(req)
			if ok {
				h.autoApproved.Store(key, true)
				return resolved(true, "mode:"+def.Name)
			}
			h.Log.Info("permissions", "auto mode deferred to the user", map[string]any{"subject": req.Subject, "why": why})
			req.Reason = why
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
		allowed, by := ui.AskPermission(PermRequest{Tool: req.Tool, Always: label, Subject: req.Subject, Arg: h.shownArg(req), Reason: req.Reason, Preview: req.Preview, Description: req.Description})
		if allowed && by == "user:always" {
			for _, r := range rules {
				h.Perm.Add(r)
			}
		}
		return resolved(allowed, by)
	}
}

// shownArg is the request's argument as the user and the screener read it: a
// path relative to the project when inside it.
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
		if n.Fail != "" || n.ForceAsk != "" {
			return "", nil
		}
		var names []string
		var rules []perm.Rule
		seen := map[string]bool{}
		unnamed := 0 // segments allowed exactly as written, with no prefix to name
		for _, seg := range n.Segments {
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

// globMeta are perm's glob wildcards; its patterns have no escape
const globMeta = "*?"

var subcommandWord = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// programs whose second word is a subcommand; for any other it may be a
// file ("cat notes"), and the prefix would allow any file after it
var subcommandPrograms = map[string]bool{
	"git": true, "go": true, "cargo": true, "npm": true, "pnpm": true, "yarn": true, "bun": true,
	"make": true, "docker": true, "kubectl": true, "gh": true, "uv": true,
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
