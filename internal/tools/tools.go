// Package tools ships the builtin model tools. Every call runs schema
// validation, tool.pre, the permission gate, the tool, then tool.post;
// failures are tool errors, never panics.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/aymanbagabas/go-udiff"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/store"
)

// Request is one permission check the pipeline needs answered.
type Request struct {
	Tool    string // the tool asking (bash, edit, mcp_github_search); set by the pipeline
	Subject string // tool.bash, fs.read, fs.write, net.<host>
	Arg     string // path, url or normalized command segment
	Reason  string // human-facing description for the overlay
	Preview string // file tools: the proposed change as "- "/"+ " lines
	// Description is the model's own summary of a command; the command is
	// what gets checked and shown, this only helps the user read it
	Description string
	// Unsandboxed is the model's sandbox: false, input to a window that runs
	// unconfined, or a Lua tool script that widens what zemi allows: no mode
	// settles the request, the user answers it
	Unsandboxed bool
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

	// Question asks the user and returns one answer per question; false =
	// dismissed. nil = nobody can answer (headless).
	Question func(qs []Question) (answers []string, ok bool)
	// Edit opens path in the user's editor and returns when it closes.
	// nil = nobody is at a terminal (headless, subagent).
	Edit func(ctx context.Context, path string, line int, title string) error
	// ApprovePlan shows a plan and asks the user to approve it; false =
	// dismissed. nil = nobody can answer (headless).
	ApprovePlan func(plan string) (PlanVerdict, bool)

	// Jobs owns model-started background processes; nil = jobs error.
	Jobs *Jobs

	// Skills resolves enabled skills for the skill tool; nil = skills error.
	Skills func() []SkillInfo
	// Images reports whether the model takes image parts; nil = yes. A
	// text-only model gets a text error from read instead of a refused
	// request that ends the turn.
	Images func() bool

	// Remember proposes a memory through the host's intake and autonomy
	// gate (stored reports auto-apply vs queued for review); Recall
	// searches memories. nil = memory tools error.
	Remember func(scope, kind, text string, tags []string) (stored bool, err error)
	Recall   func(query string, limit int) ([]*store.Memory, error)
	// EditMemory rewrites a memory by id, or deletes it when text is "",
	// and returns what happened. nil = memory tools error.
	EditMemory func(id, text string) (string, error)

	// Scratch reports a path inside the session scratchpad: writes there
	// pass plan mode. nil = no scratchpad.
	Scratch func(path string) bool
	// SearchURL returns the user's SearXNG endpoint for web_search; nil or
	// "" = DuckDuckGo Lite.
	SearchURL func() string

	// ScratchDir is the session scratchpad, where output cut from a result
	// is saved in full; nil or "" = nowhere.
	ScratchDir func() string

	// Seen maps absolute paths to the modification time the model read
	// them at: write tools refuse a file it has not seen as it is. nil = no
	// check.
	Seen *sync.Map

	// Instructions returns the instruction file governing a file the model
	// just read, when it is not already in the prompt; nil = none.
	Instructions func(path string) string

	// AfterWrite returns text to append to a successful write, edit or
	// multiedit result (language server diagnostics); nil = nothing.
	AfterWrite func(path string) string

	// Diagnostics backs the diagnostics tool; nil = unavailable.
	Diagnostics func(path string) (string, error)

	// LSP returns the language server covering path, with the file synced
	// to it; nil = no lsp tool support.
	LSP func(ctx context.Context, path string) (LSPSession, error)

	// Denied reports a call the gate refused (the tool.denied event); nil
	// = silent.
	Denied func(tool string, req Request, v Verdict)

	// Plan reports plan mode: file writes are denied and allowed commands
	// ask instead unless they only read; nil = never.
	Plan func() bool

	// Checkpoint records a file's content before a write tool changes it,
	// so the turn can be rewound; nil = no checkpoints.
	Checkpoint func(path string)

	// RewriteLoosened marks a re-entry after a non-user rewrite: allow
	// decisions ask instead (never skips the gate, §6.4).
	RewriteLoosened bool

	// Sandbox is the policy a command a permission mode settled runs under;
	// nil or a nil result = unconfined (no backend, yolo).
	Sandbox func() *sandbox.Policy
	// SandboxParam gives bash, job_start and window the sandbox: false
	// escape; set while a backend is live and someone can answer the ask.
	SandboxParam bool
	// Sandboxed is this run's policy, set on the per-run copy once the gate
	// settled a command by mode; nil = unconfined.
	Sandboxed *sandbox.Policy
	// Guard runs before a sandboxed command starts; the func it returns runs
	// once the command exited, and its text ends the result. nil = none.
	Guard func(sandbox.Policy) func() string
}

// LSPSession is a language server ready for one file's queries.
type LSPSession struct {
	Client *lsp.Client
	Server string
	// Open sends a file's text from disk to the server, so edits the tool
	// made to other files are seen before the next query.
	Open func(path string) error
}

// Check decides one request; for tool.bash the store normalizes the
// command (segments, redirect gates, assignment gate) in one call.
func (p *Pipeline) Check(req Request) Verdict {
	plan := p.Plan != nil && p.Plan()
	if plan && req.Subject == "fs.write" && (p.Scratch == nil || !p.Scratch(req.Arg)) {
		return Verdict{perm.Deny, "plan mode is on: no file changes (tab switches mode)", "mode:plan"}
	}
	loosen := func(v Verdict) Verdict {
		switch {
		case v.Decision != perm.Allow:
		case p.RewriteLoosened:
			return p.resolveAsk(req, "rewritten input re-checked (allow flipped to ask)")
		// rule-allowed commands run unconfined, so only readers skip the ask
		case plan && req.Subject == "tool.bash" && perm.ReadCommand(req.Arg, nil):
		case plan && (req.Subject == "tool.bash" || strings.HasPrefix(req.Subject, "mcp.")):
			return p.resolveAsk(req, "plan mode: commands that change things ask")
		}
		return v
	}
	var o perm.Outcome
	if req.Subject == "tool.bash" {
		o = p.Store.CheckCommand(req.Arg, p.Principal)
	} else {
		o = p.Store.Evaluate(req.Subject, req.Arg, p.Principal)
	}
	switch o.Decision {
	case perm.Allow:
		return loosen(Verdict{perm.Allow, o.Reason, decidedBy(o)})
	case perm.Deny:
		return Verdict{perm.Deny, o.Reason, decidedBy(o)}
	}
	return p.resolveAsk(req, o.Reason)
}

// SandboxFor is the policy a checked command runs under: one a permission
// mode settled runs confined, rule-allowed and user-approved ones as they
// are. nil = unconfined.
func (p *Pipeline) SandboxFor(req Request, v Verdict) *sandbox.Policy {
	if p.Sandbox == nil || req.Subject != "tool.bash" || v.Decision != perm.Allow || !strings.HasPrefix(v.DecidedBy, "mode:") {
		return nil
	}
	return p.Sandbox()
}

// wrap confines a command this run starts; nil when the run is unconfined.
func (p *Pipeline) wrap() func(*exec.Cmd) error {
	if p == nil || p.Sandboxed == nil {
		return nil
	}
	pol := *p.Sandboxed
	return func(c *exec.Cmd) error { return sandbox.Wrap(c, pol) }
}

// guard starts Guard for a sandboxed run: nil when the run is unconfined,
// never nil otherwise.
func (p *Pipeline) guard() func() string {
	switch {
	case p == nil || p.Sandboxed == nil:
		return nil
	case p.Guard == nil:
		return func() string { return "" }
	}
	return p.Guard(*p.Sandboxed)
}

// sandboxHint is the line a confined command's failure gets when the
// sandbox may be why; "" otherwise. The sandbox: false retry is offered only
// where someone can answer its ask.
func (p *Pipeline) sandboxHint(out string, err error) string {
	if !sandbox.LikelyDenied(out, err) {
		return ""
	}
	if !p.SandboxParam {
		return "\nThe sandbox may have blocked this: " + sandboxLimits() + "."
	}
	return "\nThe sandbox may have blocked this: " + sandboxLimits() + ". If the command needs more, call it again with sandbox: false, which asks the user."
}

// sandboxLimits names what the live backend enforces. Every wrapped run
// goes through sandbox.Probe's backend, so that is the one to describe.
func sandboxLimits() string {
	b, _ := sandbox.Probe()
	// a backend that keeps .git read-only also gives loopback: seatbelt the
	// machine's, the Linux namespace variant one of its own; Landlock +
	// seccomp blocks every inet socket
	// NOTE: a refused connection to a host service matches no LikelyDenied
	// pattern, so on Linux it gets no hint; the limit text is all it gets.
	s := "no network, writes only in the scratchpad and $TMPDIR (a private one) (the project stays read-only)"
	switch {
	case b.EnforcesProtect && runtime.GOOS == "linux":
		s = "no network (localhost inside the sandbox only: servers already running on this machine are unreachable), writes only in the project (not in plan mode), the scratchpad and $TMPDIR (a private one), .git and .zemi read-only"
	case b.EnforcesProtect:
		s = "no network beyond localhost, writes only in the project (not in plan mode), the scratchpad and $TMPDIR (a private one), .git and .zemi read-only"
	}
	if b.HidesSecretFiles {
		return s + ", secret files unreadable"
	}
	return s + ", credential files in the home dir unreadable"
}

// SandboxProp is the escape bash, job_start and window take while a
// backend is live.
func SandboxProp() map[string]any {
	return map[string]any{"type": "boolean", "description": "commands a permission mode runs without asking run sandboxed (" + sandboxLimits() + "); false runs the command outside the sandbox and asks the user first. Use it only after a sandbox denial."}
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
	// gated: the path input is resolved before the gate and the tool opens
	// exactly that path (openGated)
	gated bool
}

func (t *tool) Info() fantasy.ToolInfo                     { return t.info }
func (t *tool) ProviderOptions() fantasy.ProviderOptions   { return nil }
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
	if t.gated {
		// resolved once: the gate judges and the tool opens the same path, so
		// a symlink a sandboxed command plants later cannot redirect the tool
		input = maps.Clone(input)
		input["path"] = perm.RealPath(str(input["path"]))
	}
	for _, req := range t.subjects(input) {
		req.Tool = t.info.Name
		v := pipe.Check(req)
		if v.Decision != perm.Allow && pipe.Denied != nil {
			pipe.Denied(t.info.Name, req, v)
		}
		if v.Decision == perm.Deny {
			return t.post(pipe, input, fantasy.NewTextErrorResponse("permission denied ("+v.DecidedBy+"): "+req.Subject+" "+req.Arg+": "+v.Reason)), nil
		}
		if v.Decision == perm.Ask {
			return t.post(pipe, input, fantasy.NewTextErrorResponse("permission unresolved (ask failed): "+req.Subject+" "+req.Arg+": "+v.Reason)), nil
		}
		if pol := pipe.SandboxFor(req, v); pol != nil {
			rp := *pipe
			rp.Sandboxed = pol
			pipe = &rp
		}
	}
	res, err := t.run(ctx, pipe, input)
	return t.post(pipe, input, res), err
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

func validate(info fantasy.ToolInfo, input map[string]any) error {
	return validateFields(info.Parameters, info.Required, input, "")
}

// validateFields checks required fields, then the required fields of every
// object in an array: a lax provider can drop a nested field, and a
// multiedit without new_string would delete the matched text.
func validateFields(props map[string]any, required []string, input map[string]any, at string) error {
	for _, r := range required {
		v, ok := input[r]
		if !ok || v == nil {
			return fmt.Errorf("missing required field %q", at+r)
		}
		if ps, ok := props[r].(map[string]any); ok {
			if t, ok := ps["type"].(string); ok && !typeOK(t, v) {
				return fmt.Errorf("field %q must be %s", at+r, t)
			}
		}
	}
	for k, v := range input {
		ps, _ := props[k].(map[string]any)
		items, _ := ps["items"].(map[string]any)
		req, _ := items["required"].([]string)
		list, _ := v.([]any)
		for i, e := range list {
			if m, ok := e.(map[string]any); ok && len(req) > 0 {
				iprops, _ := items["properties"].(map[string]any)
				if err := validateFields(iprops, req, m, fmt.Sprintf("%s%s[%d].", at, k, i)); err != nil {
					return err
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

// SchemaProperties returns what fantasy expects in ToolInfo.Parameters: it
// wraps the value as {"type": "object", "properties": value}, so a whole
// object schema there nests "type" as a property and strict providers
// reject the tool.
func SchemaProperties(schema map[string]any) map[string]any {
	if p, ok := schema["properties"].(map[string]any); ok {
		return p
	}
	return map[string]any{}
}

// ResolveSchemaRefs returns a schema's properties with local $ref pointers
// ("#/$defs/X", "#/definitions/X") replaced by what they point at. Only
// the properties map is shipped to the provider, so refs into sibling
// definitions would arrive dangling and strict providers reject the whole
// tool. A recursive or unresolvable ref is an error: the caller skips that
// tool rather than offering a broken schema.
func ResolveSchemaRefs(schema map[string]any) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	v, err := resolveRefs(props, schema, map[string]bool{}, 0)
	if err != nil {
		return nil, err
	}
	out, _ := v.(map[string]any)
	return out, nil
}

func resolveRefs(v, root any, seen map[string]bool, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("schema nests deeper than 64 levels")
	}
	switch v := v.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if seen[ref] {
				return nil, fmt.Errorf("recursive $ref %q", ref)
			}
			target, err := schemaPointer(root, ref)
			if err != nil {
				return nil, err
			}
			seen[ref] = true
			out, err := resolveRefs(target, root, seen, depth+1)
			delete(seen, ref)
			return out, err
		}
		out := make(map[string]any, len(v))
		for k, sub := range v {
			r, err := resolveRefs(sub, root, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, sub := range v {
			r, err := resolveRefs(sub, root, seen, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// schemaPointer walks a local JSON pointer; anything else (an http ref, a
// bare name) is refused rather than guessed at.
func schemaPointer(root any, ref string) (any, error) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	cur := root
	for seg := range strings.SplitSeq(rest, "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
		if cur, ok = m[seg]; !ok {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
	}
	return cur, nil
}

func gated(t fantasy.AgentTool) fantasy.AgentTool {
	t.(*tool).gated = true
	return t
}

// All returns the builtin tools bound to a pipeline.
func All(p *Pipeline) []fantasy.AgentTool {
	mk := func(name, desc string, parallel bool, props map[string]any, required []string,
		subjects func(map[string]any) []Request,
		run func(context.Context, *Pipeline, map[string]any) (fantasy.ToolResponse, error)) fantasy.AgentTool {
		if required == nil {
			required = []string{} // nil marshals as "required": null, which strict providers refuse
		}
		return &tool{
			info: fantasy.ToolInfo{
				Name: name, Description: desc,
				Parameters: props, Required: required, Parallel: parallel,
			},
			subjects: subjects,
			run:      run,
			pipe:     p,
		}
	}

	withSandbox := func(props map[string]any) map[string]any {
		if p != nil && p.SandboxParam {
			props["sandbox"] = SandboxProp()
		}
		return props
	}

	return []fantasy.AgentTool{
		mk("bash", `Run a command with sh -c in the project directory. It is POSIX sh, not bash: for [[ ]], arrays or pipefail, run bash -c '...'. Each call is a new shell, so cd and exports do not carry over. stdin is empty and there is no terminal. stdout and stderr are merged; output over 10000 characters keeps its start and end, and the whole output is saved to a file named in the result.

The call blocks until the command exits; at the timeout (default 60000 ms, up to 600000) it keeps running as a background job instead of being stopped, and the result gives you its id for job_read. Never use it for:
- servers and watchers (npm run dev, vite, go run ./cmd/server, tsc -w, jest --watch, tail -f): use job_start, read output with job_read, stop with job_kill. Appending & or nohup does not help: what the command started is stopped when the call returns.
- interactive programs (vim, less, REPLs, git rebase -i, git add -p, npm init, password or y/n prompts): use the non-interactive form, such as git --no-pager log, git commit -m "msg", npm init -y, --yes, CI=1.

Use read, grep, glob, edit and write instead of cat, grep, find, sed or echo: what a command prints here does not count as reading a file, so edit will still refuse it. Chain dependent steps with &&; send independent commands as separate calls in one response, and do not join them with echo separators. When one call repeats a long path, bind it to a short variable first: D=/long/path/to/dir; ls $D; grep -n 'func ' $D/main.go.`,
			false,
			withSandbox(map[string]any{
				"command":     map[string]any{"type": "string"},
				"description": map[string]any{"type": "string", "description": "what the command does in 5-10 words, shown to the user in place of the command, e.g. Run the unit tests"},
				"timeout_ms":  map[string]any{"type": "number", "description": "default 60000, max 600000; raise it for slow builds and test suites"},
			}),
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(str(in["command"]), in)
			},
			runBash),

		gated(mk("read", "Read a file (text or image). This is the read that edit and write require; cat in bash does not count. Each line is prefixed with its 1-based number and a tab; that prefix is not file content, so never copy it into edit. offset is a 1-based line and limit a line count. One call returns at most 2000 lines or 50000 characters and ends with the offset to continue from; lines over 2000 characters are cut. Read the region you need in one window rather than many small slices, and read several files in one response. Use it instead of cat, head, tail or sed.", true,
			map[string]any{
				"path":   map[string]any{"type": "string", "description": "file path, absolute or relative to the project root"},
				"offset": map[string]any{"type": "number", "description": "1-based line to start at"},
				"limit":  map[string]any{"type": "number", "description": "how many lines to read"},
			},
			[]string{"path"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "read file"}}
			},
			runRead)),

		gated(mk("write", "Create a file, or overwrite one with its full new content (parent directories are created). For changes to an existing file use edit; an existing file must have been read with the read tool this session, or the call is refused. Use it instead of echo or a heredoc in bash.", true,
			map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			[]string{"path", "content"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "write file", Preview: ChangeDiff("write", in)}}
			},
			runWrite)),

		gated(mk("edit", "Replace an exact substring in a file. The file must have been read with the read tool this session and be unchanged since, or the call is refused: then read it and retry, do not switch to bash. old_string must match exactly once, including indentation (never the line-number prefix read adds): add surrounding lines to make it unique, or set replace_all to change every match. Keep old_string as small as still unique. An old_string of whole lines also matches when only whitespace differs. If it fails to match, read that region again and copy it exactly; never retry with a guess. Do not re-read after success.", true,
			map[string]any{
				"path":        map[string]any{"type": "string"},
				"old_string":  map[string]any{"type": "string"},
				"new_string":  map[string]any{"type": "string"},
				"replace_all": map[string]any{"type": "boolean"},
			},
			[]string{"path", "old_string", "new_string"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "edit file", Preview: ChangeDiff("edit", in)}}
			},
			runEdit)),

		gated(mk("multiedit", "Apply several edits to one file in order, each seeing the result of the one before, with the same rules as edit (file read first, exact match). Prefer it over several edit calls to the same file. If any edit fails, nothing is written.", true,
			map[string]any{
				"path": map[string]any{"type": "string"},
				"edits": map[string]any{"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string":  map[string]any{"type": "string"},
						"new_string":  map[string]any{"type": "string"},
						"replace_all": map[string]any{"type": "boolean"},
					},
					"required": []string{"old_string", "new_string"},
				}},
			},
			[]string{"path", "edits"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "multiedit file", Preview: ChangeDiff("multiedit", in)}}
			},
			runMultiedit)),

		mk("glob", "List files matching a glob pattern under path (default: the project root), newest first, skipping .gitignored files when ripgrep is installed. A pattern without / matches file names at any depth: *.go, src/**/*.ts, *.{ts,tsx}. Use it instead of find or ls.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob such as *.go, src/**/*.ts or *.{ts,tsx}"},
				"path":    map[string]any{"type": "string", "description": "directory to search under; defaults to the project root"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: searchRoot(in), Reason: "glob files"}}
			},
			runGlob),

		mk("grep", "Search file contents under path (default: the project root), skipping .gitignored files when ripgrep is installed, with an RE2 regular expression (no lookaround or backreferences; (?i) ignores case); include limits the files by glob, such as *.go or *.{ts,tsx}. Returns up to 100 matching lines as path:line: text. Use it instead of grep or rg in bash, then read the region it points at. A match is not a read: edit still needs the read tool.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "RE2 regular expression matched against each line"},
				"path":    map[string]any{"type": "string", "description": "directory to search under; defaults to the project root"},
				"include": map[string]any{"type": "string", "description": "glob limiting which files are searched, such as *.go"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: searchRoot(in), Reason: "search files"}}
			},
			runGrep),

		mk("fetch", "Fetch a URL over http/https and return the response body as text (HTML is stripped, links kept as markdown). Every redirect hop is re-gated and private addresses are refused.",
			true,
			map[string]any{
				"url": map[string]any{"type": "string"},
			},
			[]string{"url"},
			func(in map[string]any) []Request {
				// the real host, so a rule (or an "always allow") matches the
				// per-hop check runFetch makes later
				host := "invalid"
				if u, err := ParseNetURL(str(in["url"])); err == nil {
					host = u.Hostname()
				}
				return []Request{{Subject: "net." + host, Arg: str(in["url"]), Reason: "fetch url"}}
			},
			runFetch),

		mk("todos", "Track work with several distinct steps you could lose, or several tasks from the user, in a list the user sees. Not for one change and its check, and never single-step lists. Pass the whole list every time; keep exactly one item in_progress and mark each done as soon as it is. Without todos it returns the list.",
			false,
			map[string]any{
				"todos": map[string]any{"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":     map[string]any{"type": "string"},
						"status":      map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done"}},
						"active_form": map[string]any{"type": "string", "description": "present tense shown while in progress, e.g. Running tests"},
					},
					"required": []string{"content", "status"},
				}},
			},
			nil,
			func(in map[string]any) []Request { return nil },
			runTodos),

		mk("open_in_editor", `Open a file in the user's own editor and wait until they close it, then continue with what they wrote. Use it when only the user can make the change: wording they own (a commit message, a README section, a config they maintain), a conflict you cannot resolve, a file you must not guess at, or when they ask to edit something themselves. Not for changes you can make: edit and write are the normal path, and this one stops everything until the user is done. The result is a diff of what they changed, and the file counts as read.`,
			false,
			map[string]any{
				"path":   map[string]any{"type": "string", "description": "the file to open; a missing one is created"},
				"line":   map[string]any{"type": "number", "description": "1-based line to start at"},
				"reason": map[string]any{"type": "string", "description": "what the user should change, in a few words; shown while the editor is open"},
			},
			[]string{"path", "reason"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "open in the user's editor"}}
			},
			runOpenInEditor),

		mk("question", `Ask the user up to 4 questions and wait for the answers. Use it only for what the user alone can decide: requirements, scope, expected behavior, preferences, tradeoffs between approaches. Never ask what reading code or a read-only command would tell you, never ask to approve a routine action (only one that is hard to undo or reaches beyond this machine), and never ask whether a plan is ready (use plan).

Put independent questions in one call; ask separately when one answer changes another question. Give 2-4 mutually exclusive options with 1-5 word labels and a description of what each means; put the one you recommend first and end its label with "(Recommended)". The user can always type their own answer, so never add an "Other" option; omit options for an open question.`,
			false,
			map[string]any{
				"questions": map[string]any{"type": "array", "description": "1 to 4 questions, shown as tabs the user can move between before submitting", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "the complete question, ending with a question mark"},
						"header":   map[string]any{"type": "string", "description": "a 1-3 word tab label, e.g. Auth method"},
						"options": map[string]any{"type": "array", "items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string"},
								"description": map[string]any{"type": "string"},
							},
							"required": []string{"label"},
						}},
						"multi_select": map[string]any{"type": "boolean", "description": "let the user pick several options"},
					},
					"required": []string{"question"},
				}},
			},
			[]string{"questions"},
			func(in map[string]any) []Request { return nil },
			runQuestion),

		mk("plan", `Present an implementation plan and wait for the user to approve it before changing anything. Call it in plan mode once nothing is left to decide, and outside plan mode only before a large or ambiguous change (a new feature, several reasonable designs, many files); a clear or small task is done, not planned. Ask open questions with the question tool first; do not use it to answer a research question.

The plan is markdown for someone who will implement it without guessing: the goal, what changes where (files, and existing code to reuse as path:line), how to verify, and the assumptions made. Only the approach you recommend, kept short.

The user approves it (implement it; in plan mode this also ends plan mode), asks for changes (revise it and call plan again), or dismisses it (stop and wait).`,
			false,
			map[string]any{"plan": map[string]any{"type": "string", "description": "the plan in markdown"}},
			[]string{"plan"},
			func(in map[string]any) []Request { return nil },
			runPlan),

		mk("web_search", "Search the web and return up to max_results (default 10, max 20) results as title, URL and snippet. Keep queries short and specific; fetch the results worth reading. For a question that needs several searches and pages, delegate to the web agent instead.",
			true,
			map[string]any{
				"query":       map[string]any{"type": "string"},
				"max_results": map[string]any{"type": "number"},
			},
			[]string{"query"},
			func(in map[string]any) []Request {
				u, _, err := searchURL(p, str(in["query"]))
				if err != nil {
					return []Request{{Subject: "net.invalid", Arg: err.Error(), Reason: "web search"}}
				}
				return []Request{{Subject: "net." + u.Hostname(), Arg: u.String(), Reason: "web search"}}
			},
			runWebSearch),

		mk("job_start", "Start a process that runs for a long time or never exits (dev server, file watcher, a slow build you do not need to wait on) and return a job id at once. Without args, command is a shell line run with sh -c; with args, command is the program. It runs in the project directory with secret-shaped environment variables removed. Read its output with job_read (check a server's ready line or send it a request, not sleep) and job_kill jobs you started once you no longer need them.",
			true,
			withSandbox(map[string]any{
				"command":     map[string]any{"type": "string"},
				"args":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":         map[string]any{"type": "object", "description": "extra environment variables"},
				"description": map[string]any{"type": "string", "description": "what the process is in 3-6 words, shown to the user, e.g. Dev server"},
			}),
			[]string{"command"},
			func(in map[string]any) []Request {
				env, _ := in["env"].(map[string]any)
				return bashRequests(jobLine(str(in["command"]), strList(in["args"]), env), in)
			},
			runJobStart),

		mk("job_read", "Read what a background job printed since your last read of it (everything on the first read, the first and last part when that is long) and whether it is still running.",
			true,
			map[string]any{
				"id": map[string]any{"type": "string"},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobRead),

		mk("job_list", "List background jobs with their state.",
			true,
			map[string]any{},
			nil,
			func(in map[string]any) []Request { return nil },
			runJobList),

		mk("job_kill", "Stop a background job and everything it started.",
			false,
			map[string]any{
				"id": map[string]any{"type": "string"},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobKill),

		mk("memory_add", "Save a durable lesson for future sessions: something the user taught you or you verified (kind: fact, gotcha, preference, convention or decision). One short, specific sentence written as a fact ('the user prefers X', not 'always do X'); never secrets. It may be queued for the user's review.",
			false,
			map[string]any{
				"kind":  map[string]any{"type": "string", "enum": []string{"fact", "gotcha", "preference", "convention", "decision"}},
				"text":  map[string]any{"type": "string"},
				"scope": map[string]any{"type": "string", "enum": []string{"project", "user"}, "description": "project (default): this repository only; user: every project"},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			[]string{"kind", "text"},
			func(in map[string]any) []Request { return nil },
			runMemoryAdd),

		mk("memory_search", "Search memories saved in earlier sessions (facts, gotchas, preferences, conventions, decisions).",
			true,
			map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "number"},
			},
			[]string{"query"},
			func(in map[string]any) []Request { return nil },
			runMemorySearch),

		mk("memory_update", "Rewrite a saved memory that is outdated or incomplete, by the id shown in your memory list or in memory_search results. Keep it one specific fact.",
			false,
			map[string]any{
				"id":   map[string]any{"type": "string"},
				"text": map[string]any{"type": "string", "description": "the whole new text"},
			},
			[]string{"id", "text"},
			func(in map[string]any) []Request { return nil },
			func(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
				if str(in["text"]) == "" {
					return fantasy.NewTextErrorResponse("text is empty; use memory_forget to delete a memory"), nil
				}
				return runMemoryEdit(p, str(in["id"]), str(in["text"]))
			}),

		mk("memory_forget", "Delete a saved memory that is wrong or no longer true, by its id.",
			false,
			map[string]any{"id": map[string]any{"type": "string"}},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			func(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
				return runMemoryEdit(p, str(in["id"]), "")
			}),

		mk("diagnostics", "Report compiler and linter diagnostics for one file from its language server (gopls, pyright, typescript-language-server, clangd or rust-analyzer when installed, or servers from config; none in an untrusted project). write, edit and multiedit already append errors and warnings for the file they change plus errors that appeared in other files; use this to check a file you did not change.",
			true,
			map[string]any{
				"path": map[string]any{"type": "string"},
			},
			[]string{"path"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "read diagnostics"}}
			},
			func(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
				if p.Diagnostics == nil {
					return fantasy.NewTextErrorResponse("diagnostics unavailable"), nil
				}
				out, err := p.Diagnostics(str(in["path"]))
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				return fantasy.NewTextResponse(out), nil
			}),

		mk("lsp", lspToolDesc, false, lspToolProps, []string{"action", "path"}, lspRequests, runLSP),

		mk("skill", "Load a skill's full instructions by name. Call it before the first edit, write or bash of any task a listed skill's description covers, even a small task you know how to do; the description is only the trigger, the body is the procedure. Not for a keyword match alone, and not for a skill already loaded in this conversation.",
			true,
			map[string]any{
				"name": map[string]any{"type": "string"},
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
	scope := str(in["scope"])
	if scope == "" {
		scope = "project"
	}
	// intake drops the empty tag a non-string becomes
	stored, err := p.Remember(scope, str(in["kind"]), str(in["text"]), strList(in["tags"]))
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
		fmt.Fprintf(&b, "[%s · %s · %s · %s] %s\n", m.ID[:min(8, len(m.ID))], m.Kind, m.Origin, m.Scope, m.Text)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runMemoryEdit(p *Pipeline, id, text string) (fantasy.ToolResponse, error) {
	if p.EditMemory == nil {
		return fantasy.NewTextErrorResponse("memory unavailable"), nil
	}
	msg, err := p.EditMemory(id, text)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(msg), nil
}

// ChangeDiff renders what write, edit or multiedit would do to its file as
// a unified diff against the current content. When an edit does not apply
// (the tool will report that), it falls back to ChangePreview.
func ChangeDiff(tool string, in map[string]any) string {
	path := str(in["path"])
	// it runs before the gate, on the model's path: a FIFO or /dev/zero
	// must not block or exhaust memory
	fi, err := os.Stat(path)
	if err == nil && (!fi.Mode().IsRegular() || fi.Size() > 1<<20) {
		return ChangePreview(in)
	}
	data, _ := os.ReadFile(path)
	old := lf(string(data))
	var updated string
	switch tool {
	case "write":
		updated = lf(str(in["content"]))
	case "edit", "multiedit":
		edits := []any{in}
		if tool == "multiedit" {
			edits, _ = in["edits"].([]any)
		}
		var msg string
		if updated, _, _, msg = applyEdits(old, edits, path); msg != "" {
			return ChangePreview(in)
		}
	default:
		return ChangePreview(in)
	}
	return udiff.Unified(path, path, old, updated)
}

// ChangePreview renders a file tool's change for the permission overlay
// from the input alone (the file is not read before the gate): "- " lines
// for replaced text, "+ " lines for new text.
func ChangePreview(in map[string]any) string {
	var lines []string
	add := func(prefix, text string) {
		if text == "" {
			return
		}
		for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
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
func bashRequests(cmd string, in map[string]any) []Request {
	return []Request{{Subject: "tool.bash", Arg: cmd, Reason: "run command", Description: str(in["description"]), Unsandboxed: in["sandbox"] == false}}
}

// jobLine is what job_start runs, as the shell line the gate checks. With
// args the program is exec'd, so every word is quoted where sh would split
// or expand it: sh -c 'cat ~/.ssh/id_rsa' must not check as a bare cat.
// Env extras lead as assignments, so GOFLAGS=-toolexec=x is seen too.
func jobLine(command string, args []string, env map[string]any) string {
	var words []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		words = append(words, k+"="+ShellQuote(fmt.Sprint(env[k])))
	}
	if len(args) == 0 {
		return strings.Join(append(words, command), " ")
	}
	words = append(words, ShellQuote(command))
	for _, a := range args {
		words = append(words, ShellQuote(a))
	}
	return strings.Join(words, " ")
}

// ShellQuote quotes s as one sh word, leaving words sh would not split or
// expand bare.
func ShellQuote(s string) string {
	const plain = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-./:@%+,="
	if s != "" && strings.Trim(s, plain) == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// searchRoot is where glob and grep look: path, or the project root.
func searchRoot(in map[string]any) string {
	if s := str(in["path"]); s != "" {
		return s
	}
	return "."
}

// TruncOut cuts tool output to at most n bytes and says so.
func TruncOut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n... (truncated)"
}
