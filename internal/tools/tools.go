// Package tools ships the M1 built-in tools (DESIGN §6.4): bash, read,
// write, edit, multiedit, glob, grep. Every tool passes the same
// pipeline: schema validation, tool.pre events, the permission gate, the
// Go implementation, then tool.post events. A failed gate or hook is a
// tool error result, never a Go panic path.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

// Request is one permission check the pipeline needs answered.
type Request struct {
	Subject string // tool.bash, fs.read, fs.write, net.<host>
	Arg     string // path, url or normalized command segment
	Reason  string // human-facing description for the overlay
}

// Verdict is the pipeline outcome for one permission request.
type Verdict struct {
	Decision  perm.Decision
	Reason    string
	DecidedBy string // rule, hook, user, overlay, default
}

// PreHook runs the tool.pre Lua events; decision is "" (pass),
// "deny", "rewrite" or "halt". Rewritten carries the replacement input.
type PreHook func(tool string, input map[string]any) (decision, reason string, rewritten map[string]any)

// Asker resolves an ask decision with the user; FIFO ordering is the
// caller's concern (one overlay at a time, host-side).
type Asker func(req Request) (allowed bool, decidedBy string)

// Pipeline wires tools to the permission store and the host hooks.
type Pipeline struct {
	Store  *perm.Store
	Pre    PreHook // nil = no tool.pre events
	Ask    Asker   // nil = asks fail closed
	Principal string
}

// check decides one request; tool.bash is special: the store normalizes
// the command (segments, redirect gates, assignment gate) in one call.
func (p *Pipeline) check(req Request) Verdict {
	if req.Subject == "tool.bash" {
		o := p.Store.CheckCommand(req.Arg, p.Principal)
		switch o.Decision {
		case perm.Allow:
			return Verdict{perm.Allow, o.Reason, decidedBy(o)}
		case perm.Deny:
			return Verdict{perm.Deny, o.Reason, decidedBy(o)}
		}
		return p.resolveAsk(req, o.Reason)
	}
	o := p.Store.Evaluate(req.Subject, req.Arg, p.Principal)
	switch o.Decision {
	case perm.Allow:
		return Verdict{perm.Allow, o.Reason, decidedBy(o)}
	case perm.Deny:
		return Verdict{perm.Deny, o.Reason, decidedBy(o)}
	}
	return p.resolveAsk(req, o.Reason)
}

func decidedBy(o perm.Outcome) string {
	if o.Rule != nil {
		return "rule"
	}
	return "default"
}

func (p *Pipeline) resolveAsk(req Request, reason string) Verdict {
	if p.Ask == nil {
		return Verdict{perm.Ask, reason + "; no overlay available, failing closed", "default"}
	}
	allowed, by := p.Ask(req)
	if allowed {
		return Verdict{perm.Allow, "", by}
	}
	return Verdict{perm.Deny, reason, by}
}

// tool implements fantasy.AgentTool over a plain run function.
type tool struct {
	info     fantasy.ToolInfo
	subjects func(input map[string]any) []Request
	run      func(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error)
	pipe     *Pipeline
}

func (t *tool) Info() fantasy.ToolInfo { return t.info }
func (t *tool) ProviderOptions() fantasy.ProviderOptions { return nil }
func (t *tool) SetProviderOptions(fantasy.ProviderOptions) {}

// denyOnly wraps a PreHook so rewrite and halt cannot re-fire; only deny
// decisions survive the re-entry (DESIGN §6.4).
func denyOnly(h PreHook) PreHook {
	return func(name string, input map[string]any) (string, string, map[string]any) {
		decision, reason, rewritten := h(name, input)
		if decision == "deny" {
			return "deny", reason, nil
		}
		return "", "", rewritten
	}
}

func (t *tool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if err := validate(t.info, input); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if t.pipe.Pre != nil {
		decision, reason, rewritten := t.pipe.Pre(t.info.Name, input)
		switch decision {
		case "deny":
			return fantasy.NewTextErrorResponse("denied by hook: " + reason), nil
		case "halt":
			return fantasy.ToolResponse{}, fmt.Errorf("halted by hook: %s", reason)
		case "rewrite":
			if rewritten != nil {
				// re-enter validation; tool.pre runs deny-only now and the
				// permission gate below still runs. A per-run pipeline copy
				// keeps concurrent tools race-free.
				if err := validate(t.info, rewritten); err != nil {
					return fantasy.NewTextErrorResponse("rewrite invalid: " + err.Error()), nil
				}
				input = rewritten
				rp := *t.pipe
				rp.Pre = denyOnly(t.pipe.Pre)
				return t.run(ctx, &rp, input)
			}
		}
	}
	for _, req := range t.subjects(input) {
		v := t.pipe.check(req)
		if v.Decision == perm.Deny {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): " + req.Subject + " " + req.Arg + ": " + v.Reason), nil
		}
		if v.Decision == perm.Ask {
			return fantasy.NewTextErrorResponse("permission unresolved (ask failed): " + req.Subject + " " + req.Arg + ": " + v.Reason), nil
		}
	}
	return t.run(ctx, t.pipe, input)
}

// validate checks required fields and their types against the schema.
func validate(info fantasy.ToolInfo, input map[string]any) error {
	for _, r := range info.Required {
		v, ok := input[r]
		if !ok || v == nil {
			return fmt.Errorf("missing required field %q", r)
		}
		if spec, ok := info.Parameters["properties"].(map[string]any); ok {
			if ps, ok := spec[r].(map[string]any); ok {
				if t, ok := ps["type"].(string); ok && !typeOK(t, v) {
					return fmt.Errorf("field %q must be %s", r, t)
				}
			}
		}
	}
	return nil
}

func typeOK(want string, v any) bool {
	switch want {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		switch v.(type) {
		case float64, int, int64:
			return true
		}
		return false
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return true
}

// All returns the M1 builtin set bound to a pipeline.
func All(p *Pipeline) []fantasy.AgentTool {
	mk := func(name, desc string, parallel bool, schema map[string]any, required []string,
		subjects func(map[string]any) []Request,
		run func(context.Context, *Pipeline, map[string]any) (fantasy.ToolResponse, error)) fantasy.AgentTool {
		return &tool{
			info: fantasy.ToolInfo{
				Name: name, Description: desc,
				Parameters: schema, Required: required, Parallel: parallel,
			},
			subjects: subjects,
			run:      run,
			pipe:     p,
		}
	}

	return []fantasy.AgentTool{
		mk("bash", "Run a shell command in the session directory. Output is truncated to 10000 characters.",
			false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    map[string]any{"type": "string"},
					"timeout_ms": map[string]any{"type": "number"},
				},
			},
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(str(in["command"]))
			},
			runBash),

		mk("read", "Read a file as text.", true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string"},
					"offset": map[string]any{"type": "number"},
					"limit":  map[string]any{"type": "number"},
				},
			},
			[]string{"path"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "read file"}}
			},
			runRead),

		mk("write", "Write a file, creating or overwriting it.", false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
			},
			[]string{"path", "content"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "write file"}}
			},
			runWrite),

		mk("edit", "Replace an exact substring in a file.", false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":         map[string]any{"type": "string"},
					"old_string":   map[string]any{"type": "string"},
					"new_string":   map[string]any{"type": "string"},
					"replace_all":  map[string]any{"type": "boolean"},
				},
			},
			[]string{"path", "old_string", "new_string"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "edit file"}}
			},
			runEdit),

		mk("multiedit", "Apply several exact replacements to one file in order.", false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"edits": map[string]any{"type": "array"},
				},
			},
			[]string{"path", "edits"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "multiedit file"}}
			},
			runMultiedit),

		mk("glob", "List files matching a glob pattern.", true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
					"path":    map[string]any{"type": "string"},
				},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: argOr(in["path"], "."), Reason: "glob files"}}
			},
			runGlob),

		mk("grep", "Search file contents with a regular expression.", true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
					"path":    map[string]any{"type": "string"},
					"include": map[string]any{"type": "string"},
				},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: argOr(in["path"], "."), Reason: "search files"}}
			},
			runGrep),
	}
}

// bashRequests routes the whole command through the tool.bash subject;
// the store's CheckCommand splits it into segments and gates.
func bashRequests(cmd string) []Request {
	return []Request{{Subject: "tool.bash", Arg: cmd, Reason: "run command"}}
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func argOr(v any, def string) string {
	if s := str(v); s != "" {
		return s
	}
	return def
}

func truncOut(s string) string {
	const max = 10000
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
