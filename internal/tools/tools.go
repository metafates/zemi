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
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// Request is one permission check the pipeline needs answered.
type Request struct {
	Subject string // tool.bash, fs.read, fs.write, net.<host>
	Arg     string // path, url or normalized command segment
	Reason  string // human-facing description for the overlay
	Preview string // file tools: the proposed change as "- "/"+ " lines
}

// Verdict is the pipeline outcome for one permission request.
type Verdict struct {
	Decision  perm.Decision
	Reason    string
	DecidedBy string // rule, hook, user, overlay, default
}

// PreHook runs the tool.pre Lua events; decision is "" (pass),
// "deny", "rewrite" or "halt". Rewritten carries the replacement input
// (already shallow-merged over the original). Loosened reports a rewrite
// decided by a non-user principal: on re-entry an allow decision flips
// to ask, so a rewrite can never ride a pre-existing allow glob (DESIGN
// §6.2, §6.4).
type PreHook func(tool string, input map[string]any) (decision, reason string, rewritten map[string]any, loosened bool)

// Asker resolves an ask decision with the user; FIFO ordering is the
// caller's concern (one overlay at a time, host-side).
type Asker func(req Request) (allowed bool, decidedBy string)

// Pipeline wires tools to the permission store and the host hooks.
type Pipeline struct {
	Store     *perm.Store
	Pre       PreHook // nil = no tool.pre events
	Ask       Asker   // nil = asks fail closed
	Post      func(tool string, input map[string]any, ok bool, output string)
	Principal string

	// TodosRead/TodosWrite back the session todo list; nil = todos error.
	TodosRead  func() ([]store.Todo, error)
	TodosWrite func([]store.Todo) error

	// Question renders a structured ask overlay; nil = questions fail.
	Question func(q Question) (answer string, ok bool)

	// Jobs owns model-started background processes; nil = jobs error.
	Jobs *Jobs

	// Skills resolves enabled skills for the skill tool; nil = skills error.
	Skills func() []SkillInfo

	// Remember proposes a memory through the host's intake and autonomy
	// gate (stored reports auto-apply vs queued for review); Recall
	// searches memories. nil = memory tools error.
	Remember func(scope, kind, text string, tags []string) (stored bool, err error)
	Recall   func(query string, limit int) ([]*store.Memory, error)

	// RewriteLoosened marks a re-entry after a non-user rewrite: allow
	// decisions ask instead (never skips the gate, §6.4).
	RewriteLoosened bool
}

// check decides one request; tool.bash is special: the store normalizes
// the command (segments, redirect gates, assignment gate) in one call.
// Check exposes one permission check (host-side callers outside the
// pipeline, e.g. Lua-namespace gates).
func (p *Pipeline) Check(req Request) Verdict { return p.check(req) }

func (p *Pipeline) check(req Request) Verdict {
	loosen := func(v Verdict) Verdict {
		if v.Decision == perm.Allow && p.RewriteLoosened {
			return p.resolveAsk(req, "rewritten input re-checked (allow flipped to ask)")
		}
		return v
	}
	if req.Subject == "tool.bash" {
		o := p.Store.CheckCommand(req.Arg, p.Principal)
		switch o.Decision {
		case perm.Allow:
			return loosen(Verdict{perm.Allow, o.Reason, decidedBy(o)})
		case perm.Deny:
			return Verdict{perm.Deny, o.Reason, decidedBy(o)}
		}
		return p.resolveAsk(req, o.Reason)
	}
	o := p.Store.Evaluate(req.Subject, req.Arg, p.Principal)
	switch o.Decision {
	case perm.Allow:
		return loosen(Verdict{perm.Allow, o.Reason, decidedBy(o)})
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

func (t *tool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if err := validate(t.info, input); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	pipe := t.pipe
	if t.pipe.Pre != nil {
		decision, reason, rewritten, loosened := t.pipe.Pre(t.info.Name, input)
		switch decision {
		case "deny":
			return fantasy.NewTextErrorResponse("denied by hook: " + reason), nil
		case "halt":
			return fantasy.ToolResponse{}, fmt.Errorf("halted by hook: %s", reason)
		case "rewrite":
			if rewritten != nil {
				// The rewritten input re-enters at the permission gate
				// below (never skips it, DESIGN §6.4); a per-run pipeline
				// copy keeps concurrent tools race-free.
				if err := validate(t.info, rewritten); err != nil {
					return fantasy.NewTextErrorResponse("rewrite invalid: " + err.Error()), nil
				}
				input = rewritten
				rp := *t.pipe
				rp.RewriteLoosened = loosened
				pipe = &rp
			}
		}
	}
	for _, req := range t.subjects(input) {
		v := pipe.check(req)
		if v.Decision == perm.Deny {
			return t.post(pipe, input, fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): " + req.Subject + " " + req.Arg + ": " + v.Reason)), nil
		}
		if v.Decision == perm.Ask {
			return t.post(pipe, input, fantasy.NewTextErrorResponse("permission unresolved (ask failed): " + req.Subject + " " + req.Arg + ": " + v.Reason)), nil
		}
	}
	return t.runPost(ctx, pipe, input)
}

// runPost wraps run with the tool.post notification.
func (t *tool) runPost(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error) {
	res, err := t.run(ctx, p, input)
	return t.post(p, input, res), err
}

// post delivers the tool.post notification with a text summary of the
// response; Post is optional.
func (t *tool) post(p *Pipeline, input map[string]any, res fantasy.ToolResponse) fantasy.ToolResponse {
	if p.Post == nil {
		return res
	}
	p.Post(t.info.Name, input, !res.IsError, res.Content)
	return res
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

// All returns the M1+M2 builtin set bound to a pipeline.
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
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "write file", Preview: changePreview(in)}}
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
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "edit file", Preview: changePreview(in)}}
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
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "multiedit file", Preview: changePreview(in)}}
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

		mk("fetch", "Fetch a URL over http/https and return the response body as text (HTML is stripped). Every redirect hop is re-gated and private addresses are refused.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string"},
				},
			},
			[]string{"url"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "net.*", Arg: str(in["url"]), Reason: "fetch url"}}
			},
			runFetch),

		mk("todos", "Read or replace this session's todo list. Each todo: content, status (pending, in_progress or done), optional active_form.",
			false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{"type": "array"},
				},
			},
			nil,
			func(in map[string]any) []Request { return nil },
			runTodos),

		mk("question", "Ask the user a question with a fixed set of answer options; returns the chosen label.",
			false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question":     map[string]any{"type": "string"},
					"description":  map[string]any{"type": "string"},
					"options":      map[string]any{"type": "array"},
				},
			},
			[]string{"question", "options"},
			func(in map[string]any) []Request { return nil },
			runQuestion),

		mk("sourcegraph", "Search public open-source repositories on sourcegraph.com.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"count": map[string]any{"type": "number"},
				},
			},
			[]string{"query"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "net.sourcegraph.com", Arg: "sourcegraph.com", Reason: "sourcegraph search"}}
			},
			runSourcegraph),

		mk("job_start", "Start a command in the background and return a job id. The child env is filtered; stdio are pipes.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"args":    map[string]any{"type": "array"},
					"env":     map[string]any{"type": "object"},
				},
			},
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(strings.Join(append([]string{str(in["command"])}, strList(in["args"])...), " "))
			},
			runJobStart),

		mk("job_read", "Read a background job's buffered stdout/stderr and exit state.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobRead),

		mk("job_list", "List background jobs with their state.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
			nil,
			func(in map[string]any) []Request { return nil },
			runJobList),

		mk("job_kill", "Kill one background job started with job_start.",
			false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
				},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobKill),

		mk("memory_add", "Save a durable lesson for future sessions in this project: something the user taught you or you verified (kind: fact, gotcha, preference, convention or decision). One short, specific sentence; never secrets. It may be queued for the user's review.",
			false,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":  map[string]any{"type": "string", "enum": []string{"fact", "gotcha", "preference", "convention", "decision"}},
					"text":  map[string]any{"type": "string"},
					"scope": map[string]any{"type": "string", "enum": []string{"project", "user"}},
					"tags":  map[string]any{"type": "array"},
				},
			},
			[]string{"kind", "text"},
			func(in map[string]any) []Request { return nil },
			runMemoryAdd),

		mk("memory_search", "Search memories saved in earlier sessions (facts, gotchas, preferences, conventions, decisions).",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "number"},
				},
			},
			[]string{"query"},
			func(in map[string]any) []Request { return nil },
			runMemorySearch),

		mk("skill", "Load an installed skill's SKILL.md body on demand.",
			true,
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			},
			[]string{"name"},
			func(in map[string]any) []Request { return nil },
			runSkill),
	}
}

func runMemoryAdd(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
	if p.Remember == nil {
		return fantasy.NewTextErrorResponse("memory unavailable"), nil
	}
	var tags []string
	if list, ok := in["tags"].([]any); ok {
		for _, t := range list {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	scope := str(in["scope"])
	if scope == "" {
		scope = "project"
	}
	stored, err := p.Remember(scope, str(in["kind"]), str(in["text"]), tags)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if stored {
		return fantasy.NewTextResponse("memory saved"), nil
	}
	return fantasy.NewTextResponse("memory queued for the user's review"), nil
}

func runMemorySearch(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
	if p.Recall == nil {
		return fantasy.NewTextErrorResponse("memory unavailable"), nil
	}
	limit := 10
	if n, ok := in["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	rows, err := p.Recall(str(in["query"]), limit)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(rows) == 0 {
		return fantasy.NewTextResponse("no matching memories"), nil
	}
	var b strings.Builder
	for _, m := range rows {
		fmt.Fprintf(&b, "[%s · %s · %s] %s\n", m.Kind, m.Origin, m.Scope, m.Text)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// changePreview renders a file tool's change for the permission overlay
// from the input alone (the file is not read before the gate): "- " lines
// for replaced text, "+ " lines for new text.
func changePreview(in map[string]any) string {
	var lines []string
	add := func(prefix, text string) {
		if text == "" {
			return
		}
		for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			lines = append(lines, prefix+l)
		}
	}
	add("+ ", str(in["content"]))
	add("- ", str(in["old_string"]))
	add("+ ", str(in["new_string"]))
	if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				add("- ", str(m["old_string"]))
				add("+ ", str(m["new_string"]))
			}
		}
	}
	return strings.Join(lines, "\n")
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

func strList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, str(s))
	}
	return out
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
