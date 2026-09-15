// Package tools ships the builtin model tools. Every call runs schema
// validation, tool.pre, the permission gate, the tool, then tool.post;
// failures are tool errors, never panics.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aymanbagabas/go-udiff"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// Request is one permission check the pipeline needs answered.
type Request struct {
	Tool    string // the tool asking (bash, edit, mcp_github_search); set by the pipeline
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

	// Question asks the user and returns one answer per question; false =
	// dismissed. nil = nobody can answer (headless).
	Question func(qs []Question) (answers []string, ok bool)
	// ApprovePlan shows a plan and asks the user to approve it; false =
	// dismissed. nil = nobody can answer (headless).
	ApprovePlan func(plan string) (PlanVerdict, bool)

	// Jobs owns model-started background processes; nil = jobs error.
	Jobs *Jobs

	// Skills resolves enabled skills for the skill tool; nil = skills error.
	Skills func() []SkillInfo

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
	// ScratchDir is the session scratchpad, where output cut from a result
	// is saved in full; nil or "" = nowhere.
	ScratchDir func() string

	// Seen maps absolute paths to the modification time the model read
	// them at: write tools refuse a file it has not seen as it is. nil = no
	// check.
	Seen *sync.Map

	// AfterWrite returns text to append to a successful write, edit or
	// multiedit result (language server diagnostics); nil = nothing.
	AfterWrite func(path string) string

	// Diagnostics backs the diagnostics tool; nil = unavailable.
	Diagnostics func(path string) (string, error)

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
		case plan && req.Subject == "tool.bash" && perm.SafeCommand(req.Arg, nil):
		case plan && (req.Subject == "tool.bash" || req.Subject == "tool.lua" || strings.HasPrefix(req.Subject, "mcp.")):
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

// All returns the M1+M2 builtin set bound to a pipeline.
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

	return []fantasy.AgentTool{
		mk("bash", `Run a command with sh -c in the project directory. It is POSIX sh, not bash: for [[ ]], arrays or pipefail, run bash -c '...'. Each call is a new shell, so cd and exports do not carry over. stdin is empty and there is no terminal. stdout and stderr are merged; output over 10000 characters keeps its start and end, and the whole output is saved to a file named in the result.

The call blocks until the command exits; at the timeout (default 60000 ms, up to 600000) it is stopped with everything it started. Never use it for:
- servers and watchers (npm run dev, vite, go run ./cmd/server, tsc -w, jest --watch, tail -f): use job_start, read output with job_read, stop with job_kill. Appending & or nohup does not help: what the command started is stopped when the call returns.
- interactive programs (vim, less, REPLs, git rebase -i, git add -p, npm init, password or y/n prompts): use the non-interactive form, such as git --no-pager log, git commit -m "msg", npm init -y, --yes, CI=1.

Use read, grep, glob, edit and write instead of cat, grep, find, sed or echo. Chain dependent steps with &&; send independent commands as separate calls in one response.`,
			false,
			map[string]any{
				"command":     map[string]any{"type": "string"},
				"description": map[string]any{"type": "string", "description": "what the command does in 5-10 words, shown to the user in place of the command, e.g. Run the unit tests"},
				"timeout_ms":  map[string]any{"type": "number", "description": "default 60000, max 600000; raise it for slow builds and test suites"},
			},
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(str(in["command"]))
			},
			runBash),

		mk("read", "Read a file. Each line is prefixed with its 1-based number and a tab; that prefix is not file content, so never copy it into edit. offset is a 1-based line and limit a line count. One call returns at most 2000 lines or 50000 characters and ends with the offset to continue from; lines over 2000 characters are cut. When you know the part you need, read only that part.", true,
			map[string]any{
				"path":   map[string]any{"type": "string", "description": "file path, absolute or relative to the project root"},
				"offset": map[string]any{"type": "number", "description": "1-based line to start at"},
				"limit":  map[string]any{"type": "number", "description": "how many lines to read"},
			},
			[]string{"path"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"]), Reason: "read file"}}
			},
			runRead),

		mk("write", "Create a file, or overwrite one with its full new content (parent directories are created). Prefer edit for changes to an existing file, and read a file before overwriting it.", false,
			map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			[]string{"path", "content"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Reason: "write file", Preview: ChangeDiff("write", in)}}
			},
			runWrite),

		mk("edit", "Replace an exact substring in a file you have read. old_string must match exactly once, including indentation (never the line-number prefix read adds): add surrounding lines to make it unique, or set replace_all to change every match. A multi-line old_string also matches when only whitespace differs.", false,
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
			runEdit),

		mk("multiedit", "Apply several edits to one file in order, each seeing the result of the one before, with the same matching rules as edit. If any edit fails, nothing is written.", false,
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
			runMultiedit),

		mk("glob", "List files matching a glob pattern under path (default: the project root), newest first, skipping .gitignored files. A pattern without / matches file names at any depth: *.go, src/**/*.ts, *.{ts,tsx}. Use it instead of find or ls.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob such as *.go, src/**/*.ts or *.{ts,tsx}"},
				"path":    map[string]any{"type": "string", "description": "directory to search under; defaults to the project root"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: argOr(in["path"], "."), Reason: "glob files"}}
			},
			runGlob),

		mk("grep", "Search file contents under path (default: the project root), skipping .gitignored files, with an RE2 regular expression (no lookaround or backreferences; (?i) ignores case); include limits the files by glob, such as *.go or *.{ts,tsx}. Returns up to 100 matching lines as path:line: text. Use it instead of grep or rg in bash.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "RE2 regular expression matched against each line"},
				"path":    map[string]any{"type": "string", "description": "directory to search under; defaults to the project root"},
				"include": map[string]any{"type": "string", "description": "glob limiting which files are searched, such as *.go"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: argOr(in["path"], "."), Reason: "search files"}}
			},
			runGrep),

		mk("fetch", "Fetch a URL over http/https and return the response body as text (HTML is stripped). Every redirect hop is re-gated and private addresses are refused.",
			true,
			map[string]any{
				"url": map[string]any{"type": "string"},
			},
			[]string{"url"},
			func(in map[string]any) []Request {
				// the real host, so a rule (or an "always allow") matches the
				// per-hop check runFetch makes later
				host := "invalid"
				if u, err := parseNetURL(str(in["url"])); err == nil {
					host = u.Hostname()
				}
				return []Request{{Subject: "net." + host, Arg: str(in["url"]), Reason: "fetch url"}}
			},
			runFetch),

		mk("todos", "Track multi-step work (three or more steps, or several tasks from the user) in a list the user sees. Pass the whole list every time; keep exactly one item in_progress and mark each done as soon as it is. Without todos it returns the list.",
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

		mk("question", `Ask the user up to 4 questions and wait for the answers. Use it only for what the user alone can decide: requirements, scope, expected behavior, preferences, tradeoffs between approaches. Never ask what reading code or a read-only command would tell you, never ask for permission (risky actions already ask), and never ask whether a plan is ready (use plan).

Put independent questions in one call; ask separately when one answer changes another question. Give 2-4 mutually exclusive options with 1-5 word labels and a description of what each means; put the one you recommend first and end its label with "(Recommended)". The user can always type their own answer, so never add an "Other" option; omit options for an open question.`,
			false,
			map[string]any{
				"questions": map[string]any{"type": "array", "description": "1 to 4 questions, shown one after another", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string", "description": "the complete question, ending with a question mark"},
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

		mk("plan", `Present an implementation plan and wait for the user to approve it before changing anything. Call it in plan mode once nothing is left to decide, and outside plan mode before a large or ambiguous change (a new feature, several reasonable designs, many files). Ask open questions with the question tool first; do not use it to answer a research question.

The plan is markdown for someone who will implement it without guessing: the goal, what changes where (files, and existing code to reuse as path:line), how to verify, and the assumptions made. Only the approach you recommend, kept short.

The user approves it (implement it; in plan mode this also ends plan mode), asks for changes (revise it and call plan again), or dismisses it (stop and wait).`,
			false,
			map[string]any{"plan": map[string]any{"type": "string", "description": "the plan in markdown"}},
			[]string{"plan"},
			func(in map[string]any) []Request { return nil },
			runPlan),

		mk("sourcegraph", "Search public open-source repositories on sourcegraph.com.",
			true,
			map[string]any{
				"query": map[string]any{"type": "string"},
				"count": map[string]any{"type": "number"},
			},
			[]string{"query"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "net.sourcegraph.com", Arg: "sourcegraph.com", Reason: "sourcegraph search"}}
			},
			runSourcegraph),

		mk("job_start", "Start a process that runs for a long time or never exits (dev server, file watcher, a slow build you do not need to wait on) and return a job id at once. Without args, command is a shell line run with sh -c; with args, command is the program. It runs in the project directory with secret-shaped environment variables removed. Read its output with job_read (check a server's ready line or send it a request, not sleep) and job_kill jobs you started once you no longer need them.",
			true,
			map[string]any{
				"command":     map[string]any{"type": "string"},
				"args":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":         map[string]any{"type": "object", "description": "extra environment variables"},
				"description": map[string]any{"type": "string", "description": "what the process is in 3-6 words, shown to the user, e.g. Dev server"},
			},
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(strings.Join(append([]string{str(in["command"])}, strList(in["args"])...), " "))
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

		mk("memory_add", "Save a durable lesson for future sessions in this project: something the user taught you or you verified (kind: fact, gotcha, preference, convention or decision). One short, specific sentence written as a fact ('the user prefers X', not 'always do X'); never secrets. It may be queued for the user's review.",
			false,
			map[string]any{
				"kind":  map[string]any{"type": "string", "enum": []string{"fact", "gotcha", "preference", "convention", "decision"}},
				"text":  map[string]any{"type": "string"},
				"scope": map[string]any{"type": "string", "enum": []string{"project", "user"}},
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

		mk("diagnostics", "Report compiler and linter diagnostics for one file from its language server (gopls, pyright, rust-analyzer, typescript-language-server or clangd when installed, or servers from config). write, edit and multiedit already append diagnostics for the file they change; use this to check other files.",
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

		mk("skill", "Load a skill's full instructions by name. Load it before acting whenever a skill's description matches the task.",
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
	data, _ := os.ReadFile(path)
	old, updated := lf(string(data)), ""
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
	return strings.ToValidUTF8(s[:max], "") + "\n... (truncated)"
}
