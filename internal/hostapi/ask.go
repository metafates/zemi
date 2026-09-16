// ask.go is the permission gate for model tool calls: the ask hook and
// the "always" rules it offers.
package hostapi

import (
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
	return func(req tools.Request) (bool, string) {
		resolved := func(allowed bool, by string) (bool, string) {
			h.EmitAsync("permission.resolved", map[string]any{
				"subject": req.Subject, "arg": req.Arg,
				"decision": map[bool]string{true: "allow", false: "deny"}[allowed],
				"by":       by,
			})
			return allowed, by
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
			switch o.Decision {
			case perm.Allow:
				return resolved(true, "mode:"+def.Name)
			case perm.Deny:
				return resolved(false, "mode:"+def.Name)
			}
		}
		switch def.Base {
		case "yolo":
			return resolved(true, "mode:"+def.Name)
		case "plan":
			if req.Subject == "tool.bash" && perm.SafeCommand(req.Arg, h.safeCommandPath) {
				return resolved(true, "mode:"+def.Name)
			}
		case "auto":
			// everything the screener saw: an MCP or Lua tool's Arg is only its
			// name, the input it approved is in Preview
			key := strings.Join([]string{req.Subject, req.Arg, req.Reason, req.Preview}, "\x00")
			if h.autoLocal(req) {
				return resolved(true, "mode:"+def.Name)
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
			return resolved(false, "default")
		}
		label, rules := h.alwaysChoice(req)
		allowed, by := ui.AskPermission(PermRequest{Tool: req.Tool, Always: label, Subject: req.Subject, Arg: req.Arg, Reason: req.Reason, Preview: req.Preview, Description: req.Description})
		if allowed && by == "user:always" {
			for _, r := range rules {
				h.Perm.Add(r)
			}
		}
		return resolved(allowed, by)
	}
}

// alwaysChoice is the "don't ask again" option for a request: the label the
// prompt shows and the session rules (model principal) accepting it adds.
// Subcommand programs allow their two-word prefix ("go test"), file access its
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
		// The label is a button: long enough and the row stacks, which turns
		// the prompt into a wall. It still has to say how much it covers, so
		// what is not named is counted.
		if len(names) == 0 {
			return "don't ask again for this exact command", rules
		}
		shown := names
		if len(shown) > 2 {
			shown = names[:2]
		}
		if hidden := len(names) - len(shown) + unnamed; hidden > 0 {
			return fmt.Sprintf("don't ask again for %s and %d more", strings.Join(shown, ", "), hidden), rules
		}
		return "don't ask again for " + strings.Join(shown, ", "), rules
	case req.Subject == "fs.write" || req.Subject == "fs.read":
		// relative to the cwd, as the tools open it: Dir("a.go") is "." and
		// the rule would be "*", which matches every path on disk
		abs := perm.RealPath(req.Arg)
		if abs == "" || strings.ContainsAny(abs, globMeta) {
			return "", nil
		}
		dir := filepath.Dir(abs)
		shown := dir
		if rel, err := filepath.Rel(perm.RealPath(h.Dirs.ProjectDir), dir); err == nil && !strings.HasPrefix(rel, "..") {
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
			if strings.Contains(strings.ToLower(abs), string(filepath.Separator)+".git") && !inGitDir(abs) &&
				!strings.EqualFold(base, ".gitattributes") && !strings.EqualFold(base, ".gitmodules") {
				return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{r, rule(req.Subject, abs)}
			}
		}
		return fmt.Sprintf("allow %s %s", what, where), []perm.Rule{r}
	case req.Subject == "tool.workflow" && req.Arg == "script":
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
	return "don't ask again for this", []perm.Rule{rule(req.Subject, req.Arg)}
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

// commandPrefix is the two-word prefix "don't ask again" allows for a
// command segment ("go test ./..." is "go test"), or "" when there is none.
func commandPrefix(seg string) string {
	f := strings.Fields(seg)
	if len(f) < 2 || !subcommandPrograms[f[0]] || !subcommandWord.MatchString(f[1]) {
		return ""
	}
	return f[0] + " " + f[1]
}
