// Package tools ships the builtin model tools. Every call runs schema
// validation, tool.pre, the permission gate, the tool, then tool.post;
// failures are tool errors, never panics.
package tools

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/aymanbagabas/go-udiff"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/store"
)

// ToolName is the name of a builtin tool, the constants below; ChatMessage
// carries one so a row's tool is checked, not compared by literal.
type ToolName string

// Builtin tool names, one definition each: everything that refers to a tool
// by name (the registry here, and the host's gate, prompt and history logic)
// uses these, so renaming a tool is one line, not a recount of literals.
const (
	ToolShell    ToolName = "shell"
	ToolRead     ToolName = "read"
	ToolWrite    ToolName = "write"
	ToolEdit     ToolName = "edit"
	ToolGlob     ToolName = "glob"
	ToolGrep     ToolName = "grep"
	ToolTodos    ToolName = "todos"
	ToolQuestion ToolName = "question"
	ToolPlan     ToolName = "plan"
	ToolSkill    ToolName = "skill"
	ToolJobStart ToolName = "job_start"
	ToolJobRead  ToolName = "job_read"
	ToolJobList  ToolName = "job_list"
	ToolJobKill  ToolName = "job_kill"
	ToolMemory   ToolName = "memory"
	// Host tools built by the host, not here, but named by the same rule.
	ToolAgent    ToolName = "agent"
	ToolWorkflow ToolName = "workflow"
	ToolLua      ToolName = "lua"
	ToolUpdate   ToolName = "update"
	ToolSearch   ToolName = "tool_search"
)

// Outcome is what a call did beyond its text: the job or run it handed the
// work to, a failure inside a call that still answered, and a stop the user
// asked for. A tool reports it while it runs, so a row never reads its own
// status out of prose.
type Outcome struct {
	Job     string // the job a shell call handed off to, or job_start's job
	Run     string // the background agent or workflow run a call started
	Warn    bool   // it answered, with a failure inside
	Stopped bool   // the user denied, dismissed or interrupted it
}

// Progress is a running call's output: Line is its last non-blank line,
// which the chat row shows under it, and Tail is its last ProgressTail lines,
// which a call view reads while the call runs.
type Progress struct {
	Line, Tail string
}

// ProgressTail is how many lines of a running call's output Progress carries.
const ProgressTail = 200

// Request is one permission check the pipeline needs answered.
type Request struct {
	Tool    string // the tool asking (shell, edit, mcp_github_search); set by the pipeline
	Subject string // tool.shell, fs.read, fs.write, lua.http.<method>
	Arg     string // path, url or command
	Reason  string // human-facing description for the overlay
	Preview string // a file tool's unified diff, a script, or a memory's text
	// Description is the model's own summary of a command; the command is
	// what gets checked and shown, this only helps the user read it
	Description string
	// Read marks a call the request's tool itself knows is read-only (an
	// MCP tool its server annotates so): it settles on its own
	Read bool
	// Script is the lua tool call this one came from (Pipeline.Script), "" for
	// a call the model made itself: it labels the ask and keeps the output whole
	Script string
	// ReadOnly marks a read-only agent's request (Pipeline.ReadOnly); set by the
	// pipeline
	ReadOnly bool
	// Agent is the subagent asking, as the prompt names it, "" for the main
	// agent; set by its pipeline's asker
	Agent string
	// Owner is whose code a Lua tool runs, as an overlay badge names it
	// ("plugin:jira"), "" for the user's and zemi's own
	Owner string
}

// Verdict is the pipeline outcome for one permission request.
type Verdict struct {
	Allowed   bool
	Reason    string
	DecidedBy string // a By value, or a user answer ("user")
}

// Who decided a Verdict.
const (
	ByDefault  = "default"
	ByNobody   = "nobody" // an ask with no UI to answer it
	ByUser     = "user"
	ByReadOnly = "read-only agent"
	// ByEvent is a verdict the permission.request veto decided
	ByEvent = "event"
)

// What a Veto hook answers with: deny the request, or run the ask path
// (the overlay). "" never happens at the gate: Check treats the veto's
// silence as "run".
const (
	VetoDeny = "deny"
	VetoAsk  = "ask"
)

// DeniedText and stopTurn build their user-stop wordings from these.
const (
	deniedPrefix  = "permission denied ("
	userDismissed = "The user dismissed"
)

// readOnlyVerdict is the gate's deny for a read-only agent, named once so
// the wording cannot drift from the self-executing actions that repeat it.
var readOnlyVerdict = Verdict{DecidedBy: ByReadOnly, Reason: "the read-only agent may not change anything"}

// DeniedText is the tool error a refused gate returns: who decided, what
// it was about, and the reason the asker gave.
func DeniedText(v Verdict, what string) string {
	s := deniedPrefix + v.DecidedBy + "): " + what
	if v.Reason == "" {
		return s
	}
	return s + ": " + v.Reason
}

// PreVerdict is a tool.pre answer; "" passes the call on.
type PreVerdict string

const (
	PreDeny    PreVerdict = "deny"
	PreHalt    PreVerdict = "halt"
	PreRewrite PreVerdict = "rewrite"
)

var PreVerdicts = []PreVerdict{PreDeny, PreHalt, PreRewrite}

// VerdictResponse is the result a deny or a halt leaves the model; done is
// false for any other verdict, which lets the call on. A halt is a result,
// not a Go error: that would fail the step and drop the calls that already
// ran in it from the history.
func VerdictResponse(v PreVerdict, reason string) (resp fantasy.ToolResponse, done bool) {
	switch v {
	case PreDeny:
		return fantasy.NewTextErrorResponse("denied by hook: " + reason), true
	case PreHalt:
		return stopTurn("halted by hook: " + reason), true
	default:
		return resp, false
	}
}

// PreHook runs the tool.pre Lua events. Rewritten carries the replacement
// input (already shallow-merged over the original).
type PreHook func(tool string, input map[string]any) (decision PreVerdict, reason string, rewritten map[string]any)

// Asker resolves an ask decision with the user; FIFO ordering is the
// caller's concern (one overlay at a time, host-side).
type Asker func(req Request) (allowed bool, decidedBy string)

// Pipeline wires tools to the host hooks: the veto and the ask settle
// every action, everything else runs.
type Pipeline struct {
	Pre PreHook // nil = no tool.pre events
	Ask Asker   // nil = no overlay to ask: the veto is skipped and actions run
	// Veto runs the permission.request event for an action request and
	// answers "deny" (with the handler's reason), "ask" or "". Fired only
	// when Ask is set: a gate without an asker (a Lua call from the actor
	// goroutine) cannot survive the veto's own interpreter round-trip.
	Veto func(req Request) (decision, reason string)
	// Log records every gate decision: allowed by default at debug, a user
	// or event decision at info, a refusal at warn; nil = silent.
	Log *slog.Logger
	// Post runs the tool.post events after a call the gate let through or
	// refused, and returns the output the model gets; nil = none.
	Post func(ctx context.Context, pad, tool string, input map[string]any, ok bool, output string, changed []string, took time.Duration) string

	// TodosRead/TodosWrite back the session todo list; nil = todos error.
	TodosRead  func() ([]store.Todo, error)
	TodosWrite func([]store.Todo) error

	// Question asks the user and returns one answer per question; false =
	// dismissed. nil = nobody can answer.
	Question func(qs []Question) (answers []Answer, ok bool)
	// ApprovePlan shows a plan and asks the user to approve it; false =
	// dismissed. nil = nobody can answer.
	ApprovePlan func(plan string) (PlanVerdict, bool)

	// Jobs owns model-started background processes; nil = jobs error.
	Jobs *Jobs

	// Skills resolves enabled skills for the skill tool; nil = skills error.
	Skills func() []SkillInfo
	// Images reports whether the model takes image parts; nil = yes. A
	// text-only model is told so in read's description, and read refuses an
	// image instead of sending a request that ends the turn. All calls it
	// once while it builds, so it must not take a lock All's caller holds.
	Images func() bool

	// Remember runs the memory tool through the host's intake and autonomy
	// gate and returns the answer: what became of it and what to do next; p
	// is the run asking, whose gate a forget goes through. Text mode keeps
	// one fact; a non-empty Path writes a private-partition brain file with
	// Content (DESIGN §10.4). nil = memory tool error.
	Remember func(p *Pipeline, call MemoryCall) (string, error)

	// Pad is the scratchpad this pipeline was bound to, "" for none: where
	// cut output is saved in full and the file tools read and write without
	// asking. Bound once, so a run that outlives its session keeps the pad
	// its prompt names.
	Pad string

	// ProjectDir is the run's project root: a command or job it starts reads
	// it as ZEMI_PROJECT_DIR, like an MCP server does. "" for none.
	ProjectDir string

	// Seen maps absolute paths to the modification time the model read
	// them at: write tools refuse a file it has not seen as it is. nil = no
	// check.
	Seen *sync.Map

	// Instructions returns text to append to a read result: the instruction
	// file governing a file the model just read, when it is not already in
	// the prompt, and a brain pointer when one maps the path; nil = none.
	Instructions func(path string) string

	// AfterWrite returns text to append to a successful write or edit
	// result (language server diagnostics, a brain pointer) and may rewrite
	// the file (a recipe's scrub); nil = nothing.
	AfterWrite func(path string) string

	// Denied reports a call the gate refused (the tool.denied event); nil
	// = silent.
	Denied func(tool string, req Request, v Verdict)

	// ReadOnly reports a read-only agent: actions are denied, its scratchpad
	// writes and its reads settle on their own; nil = never.
	ReadOnly func() bool

	// Checkpoint records a file's content before a write tool changes it,
	// so the turn can be rewound; nil = no checkpoints.
	Checkpoint func(path string)

	// Script is the lua tool call a run driven by a script came from, set on the
	// copy its calls run under: it labels their asks and keeps their output
	// whole, where a call the model made gets the cut. "" for the model's own.
	Script string
	// Path is a file tool's path input as the gate checked it, set on the
	// per-run copy. The tool opens it, never input["path"]: the string the
	// gate judged and the file the tool opens stay the same path.
	Path perm.Path
	// Changes runs before a shell command starts in dir; the func it returns
	// runs once it ended and reports the files it changed: a line for the
	// model, a diff for the row (the result's Metadata) and every path. nil = none.
	Changes func(dir string) func() (note, diff string, paths []string)
	// Progress shows a running call's last output line under its row;
	// nil = none.
	Progress func(callID string, p Progress)
	// Outcome reports what a call did beyond its text; the host keys it by
	// call id and reads it when the call ends.
	Outcome func(callID string, o Outcome)

	// changed is what a shell run found changed, kept on its per-run copy for post
	changed []string
}

// PathSubject reports a subject whose argument is a file path.
func PathSubject(subject string) bool {
	return subject == perm.SubjectRead || subject == perm.SubjectWrite
}

// CheckDenied is Check plus the tool.denied notification a refused verdict
// owes its handlers: tools that gate themselves outside Run use it, so an
// audit handler sees every refusal.
func (p *Pipeline) CheckDenied(req Request) Verdict {
	v := p.Check(req)
	if !v.Allowed && p.Denied != nil {
		p.Denied(req.Tool, req, v)
	}
	return v
}

// Check decides one request and logs the decision: a read-only agent's
// actions deny, its writes in the scratchpad aside; the permission.request
// veto settles next (a denial refuses, an ask decision runs the ask path);
// what survives runs.
func (p *Pipeline) Check(req Request) Verdict {
	req.ReadOnly, req.Script = p.ReadOnly != nil && p.ReadOnly(), p.Script
	v := p.check(req)
	if p.Log != nil {
		level := slog.LevelDebug
		switch {
		case !v.Allowed:
			level = slog.LevelWarn
		case v.DecidedBy != ByDefault:
			level = slog.LevelInfo
		}
		p.Log.Log(context.Background(), level, "tool gate",
			"tool", req.Tool, "subject", req.Subject, "arg", truncLine(req.Arg),
			"by", v.DecidedBy, "allowed", v.Allowed, "reason", v.Reason)
	}
	return v
}

// check answers a request whose pipeline fields (ReadOnly, Script) are set.
func (p *Pipeline) check(req Request) Verdict {
	if !req.Action() {
		return Verdict{Allowed: true, DecidedBy: ByDefault}
	}
	if req.ReadOnly {
		// the pad is the read-only agent's working directory, where its
		// prompt tells it to put its notes
		if PathSubject(req.Subject) && perm.InPad(p.Pad, req.Arg) {
			return Verdict{Allowed: true, DecidedBy: ByDefault}
		}
		return readOnlyVerdict
	}
	if p.Ask != nil && p.Veto != nil {
		decision, reason := p.Veto(req)
		switch decision {
		case VetoDeny:
			return Verdict{DecidedBy: ByEvent, Reason: truncLine(reason)}
		case VetoAsk:
			return p.resolveAsk(req)
		}
	}
	return Verdict{Allowed: true, DecidedBy: ByDefault}
}

// Action reports a request that changes something: the veto and the ask
// path cover these, and a read-only agent denies them. Everything else (a
// file read, a listing, a search) settles on its own.
func (r Request) Action() bool {
	return r.Subject != perm.SubjectRead && !r.Read
}

// resolveAsk puts the request to the user through Ask.
func (p *Pipeline) resolveAsk(req Request) Verdict {
	allowed, by := p.Ask(req)
	if allowed {
		return Verdict{Allowed: true, DecidedBy: by}
	}
	if by == ByNobody {
		return Verdict{DecidedBy: by, Reason: req.Reason}
	}
	return Verdict{DecidedBy: by}
}

// outcome reports a call's outcome to the host, when it listens.
func (p *Pipeline) outcome(ctx context.Context, o Outcome) {
	id, _ := ctx.Value(callIDKey{}).(string)
	if p.Outcome != nil && id != "" {
		p.Outcome(id, o)
	}
}

// callIDKey carries the tool call id to a run function, for Progress.
type callIDKey struct{}

// NoProviderOptions is the fantasy.AgentTool option pair of a tool that
// sends the provider none.
type NoProviderOptions struct{}

func (NoProviderOptions) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (NoProviderOptions) SetProviderOptions(fantasy.ProviderOptions) {}

// tool implements fantasy.AgentTool over a plain run function.
type tool struct {
	NoProviderOptions
	info fantasy.ToolInfo
	// subjects parses the input into the requests the gate answers, and
	// refuses input the tool could not act on (a URL that is no URL), so
	// the user is never asked about it; nil = none
	subjects func(p *Pipeline, input map[string]any) ([]Request, error)
	run      func(ctx context.Context, p *Pipeline, input map[string]any) (fantasy.ToolResponse, error)
	pipe     *Pipeline
	// gated: the path input is resolved before the gate and the tool opens
	// exactly that path (Pipeline.Path)
	gated bool
	// edits: the tool changes a file it must have read unchanged (write,
	// edit), checked before the gate
	edits bool
}

func (t *tool) Info() fantasy.ToolInfo { return t.info }

func (t *tool) Run(ctx context.Context, call fantasy.ToolCall) (res fantasy.ToolResponse, err error) {
	ctx = context.WithValue(ctx, callIDKey{}, call.ID)
	if t.edits {
		// the calls before this one on the same file land first, so the
		// stale check and the edit see what they wrote; released on every
		// return, or the next call on the file would wait out the turn
		defer turn(ctx, call.ID)()
	}
	input, err := ParseInput(t.info, call.Input, false)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	pipe := t.pipe
	if t.pipe.Pre != nil {
		decision, reason, rewritten := t.pipe.Pre(t.info.Name, input)
		if resp, done := VerdictResponse(decision, reason); done {
			return resp, nil
		}
		if decision == PreRewrite && rewritten != nil {
			// The rewritten input re-enters at the permission gate below
			// (never skips it); a per-run pipeline copy keeps
			// concurrent tools race-free.
			if err := Validate(t.info, rewritten, false); err != nil {
				return fantasy.NewTextErrorResponse("rewrite invalid: " + err.Error()), nil
			}
			input = rewritten
			rp := *t.pipe
			pipe = &rp
		}
	}
	start, run := time.Now(), *pipe
	defer func() { res = t.post(ctx, &run, input, res, time.Since(start)) }()
	if t.gated && unexpanded.MatchString(str(input["path"])) {
		return fantasy.NewTextErrorResponse(pipe.unexpandedText(str(input["path"]))), nil
	}
	if fi, err := os.Lstat(str(input["path"])); t.edits && err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// a write through the link would change a file the row does not name
		target := workRel(perm.RealPath(str(input["path"])))
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%s is a symbolic link to %s; edit %s instead", str(input["path"]), target, target)), nil
	}
	if t.gated {
		// resolved once: the gate judges and the tool opens the same path
		path, err := perm.Resolve(str(input["path"]))
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		run.Path = path
		input = maps.Clone(input)
		input["path"] = path.String()
	}
	if t.edits {
		// before the gate, so the user is never asked to approve a change
		// the tool then refuses; the tool checks again under the file lock
		switch fi, err := os.Stat(str(input["path"])); {
		case err == nil:
			if msg := pipe.stale(str(input["path"]), fi, ToolName(t.info.Name) == ToolEdit); msg != "" {
				return fantasy.NewTextErrorResponse(msg), nil
			}
		case ToolName(t.info.Name) != ToolWrite && errors.Is(err, os.ErrNotExist):
			// a missing file is write's normal case; an edit of one is a typo
			return fantasy.NewTextErrorResponse(pipe.notFound(str(input["path"]), err)), nil
		}
	}
	var reqs []Request
	if t.subjects != nil {
		// &run, not pipe: a subjects func that writes a pipeline field writes
		// the copy the run below gets
		if reqs, err = t.subjects(&run, input); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	for _, req := range reqs {
		req.Tool = t.info.Name
		v := run.CheckDenied(req)
		// a command is not echoed: the model has its call and the row shows it
		what := req.Subject
		if PathSubject(req.Subject) {
			what += " " + workRel(req.Arg) // as the ask showed it
		} else if req.Subject != perm.SubjectShell {
			what += " " + req.Arg
		}
		if !v.Allowed {
			if v.DecidedBy == ByUser {
				run.outcome(ctx, Outcome{Stopped: true})
			}
			return fantasy.NewTextErrorResponse(DeniedText(v, what)), nil
		}
	}
	return t.run(ctx, &run, input)
}

// unexpanded is a path a shell would expand ($TMPDIR/x, ~/x, %TEMP%\x) and
// the file tools would take as a directory of that name in the project.
var unexpanded = regexp.MustCompile(`^(\$|~|%\w+%)`)

// unexpandedText refuses such a path and names the real ones: nothing is
// expanded, so a guess would land where it did not mean.
func (p *Pipeline) unexpandedText(path string) string {
	first, _, _ := strings.Cut(filepath.ToSlash(path), "/")
	msg := fmt.Sprintf("%s: file tool paths are not expanded, so this would name a directory called %q in the project. Write the absolute path", path, first)
	home, _ := os.UserHomeDir()
	switch {
	case strings.HasPrefix(path, "~") && home != "":
		return msg + ": home is " + home + "."
	case p.Pad != "":
		return msg + ": the scratchpad is " + p.Pad + "."
	}
	return msg + "."
}

// post runs the tool.post events over a text summary of the response, which
// a handler may rewrite; a media response keeps its content.
func (t *tool) post(ctx context.Context, p *Pipeline, input map[string]any, res fantasy.ToolResponse, took time.Duration) fantasy.ToolResponse {
	if p.Post == nil {
		return res
	}
	changed := p.changed
	if t.edits && !res.IsError {
		changed = []string{str(input["path"])}
	}
	if out := p.Post(ctx, p.Pad, t.info.Name, input, !res.IsError, res.Content, changed, took); res.Type == "text" {
		res.Content = out
	}
	return res
}

// ParseInput decodes a call's input and runs Validate on it: the one place
// every tool, builtin or host-implemented, gets its type errors worded.
// A schema that admits fields it does not declare (additionalProperties,
// which the shipped properties map cannot carry) passes open.
func ParseInput(info fantasy.ToolInfo, input string, open bool) (map[string]any, error) {
	var m map[string]any
	if strings.TrimSpace(input) == "" {
		input = "{}"
	}
	err := json.Unmarshal([]byte(input), &m)
	// a JSON value that is no object at all, not a field-level type error
	if _, ok := errors.AsType[*json.SemanticError](err); ok {
		return nil, errors.New("input must be a JSON object")
	}
	if err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	return m, Validate(info, m, open)
}

// Validate is the schema check every tool call passes before its events and gate.
func Validate(info fantasy.ToolInfo, input map[string]any, open bool) error {
	return validateFields(info.Parameters, info.Required, input, "", open)
}

// SchemaOpen reports a schema that admits fields it does not declare.
func SchemaOpen(schema map[string]any) bool {
	v, ok := schema["additionalProperties"]
	return ok && v != false
}

// validateFields checks required fields, the type of every field sent, then
// the fields of every object in an array: a lax provider can drop a nested
// field.
// A field the schema does not declare is refused unless open: a misspelt
// one would otherwise run the call with its default and report success.
func validateFields(props map[string]any, required []string, input map[string]any, at string, open bool) error {
	for _, r := range required {
		if input[r] == nil {
			return fmt.Errorf("missing required field %q", at+r)
		}
	}
	for k, v := range input {
		ps, known := props[k].(map[string]any)
		// null is how some providers leave an optional field out
		if !known && !open && v != nil && len(props) > 0 {
			return fmt.Errorf("unknown field %q (have %s)", at+k, strings.Join(slices.Sorted(maps.Keys(props)), ", "))
		}
		if t, _ := ps["type"].(string); v != nil && !typeOK(t, v) {
			// a quoted number or boolean is read as meant, which saves the
			// round trip; the tools assert the type, so it is written back
			c, ok := unquote(t, v)
			if !ok {
				return fmt.Errorf("field %q must be %s", at+k, t)
			}
			input[k] = c
		}
		items, _ := ps["items"].(map[string]any)
		req, _ := items["required"].([]string)
		list, _ := v.([]any)
		for i, e := range list {
			if m, ok := e.(map[string]any); ok {
				iprops, _ := items["properties"].(map[string]any)
				if err := validateFields(iprops, req, m, fmt.Sprintf("%s%s[%d].", at, k, i), open || SchemaOpen(items)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// unquote reads a string that spells a finite number or a boolean exactly.
func unquote(want string, v any) (any, bool) {
	s, _ := v.(string)
	switch want {
	case "boolean":
		return s == "true", s == "true" || s == "false"
	case "number", "integer":
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && typeOK(want, f)
	}
	return nil, false
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
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
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
	v, err := resolveRefs(props, schema, set.Set[string]{}, 0)
	if err != nil {
		return nil, err
	}
	out, _ := v.(map[string]any)
	return out, nil
}

func resolveRefs(v, root any, seen set.Set[string], depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("schema nests deeper than 64 levels")
	}
	switch v := v.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if seen.Has(ref) {
				return nil, fmt.Errorf("recursive $ref %q", ref)
			}
			target, err := schemaPointer(root, ref)
			if err != nil {
				return nil, err
			}
			seen.Add(ref)
			out, err := resolveRefs(target, root, seen, depth+1)
			seen.Remove(ref)
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

func edits(t fantasy.AgentTool) fantasy.AgentTool {
	t.(*tool).edits = true
	return t
}

// All returns the builtin tools bound to a pipeline; nil gives their schemas.
func All(p *Pipeline) []fantasy.AgentTool {
	mk := func(name ToolName, desc string, parallel bool, args any,
		subjects func(*Pipeline, map[string]any) ([]Request, error),
		run func(context.Context, *Pipeline, map[string]any) (fantasy.ToolResponse, error),
	) fantasy.AgentTool {
		props, required := ArgsOf(args)
		return &tool{
			info: fantasy.ToolInfo{
				Name: string(name), Description: desc,
				Parameters: props, Required: required, Parallel: parallel,
			},
			subjects: subjects,
			run:      run,
			pipe:     p,
		}
	}

	// pathReq is the subjects func of a tool that makes one request about the
	// path its input names: how the input names it (searchRoot where a search
	// may name none) and why, for the ask.
	pathReq := func(subject string, arg func(map[string]any) string, reason string) func(*Pipeline, map[string]any) ([]Request, error) {
		return func(_ *Pipeline, in map[string]any) ([]Request, error) {
			return []Request{{Subject: subject, Arg: arg(in), Reason: reason}}, nil
		}
	}
	pathIn := func(in map[string]any) string { return str(in["path"]) }
	// changeReq is write's and edit's one request, previewing what it would do
	changeReq := func(tool ToolName) func(*Pipeline, map[string]any) ([]Request, error) {
		return func(_ *Pipeline, in map[string]any) ([]Request, error) {
			return []Request{{Subject: perm.SubjectWrite, Arg: pathIn(in), Preview: ChangeDiff(tool, in)}}, nil
		}
	}

	// a model told it reads images plans screenshots it can never look at
	readDesc := "Read a file (text or image), or list a directory one level deep."
	if p != nil && p.Images != nil && !p.Images() {
		readDesc = "Read a text file, or list a directory one level deep. This model cannot see images: do not take screenshots or read image files. Check visual work in its source and with the project's build or tests, or ask the user to look."
	}

	return []fantasy.AgentTool{
		mk(ToolShell, `Run a shell command. It runs bash when bash is on PATH and, on unix, falls back to zsh, dash, then sh, so a bashism can fail there. Every call already starts in the working directory: relative paths resolve there and a command never needs a cd to reach it. A cd or export lasts for its call only. There is no terminal and stdin is empty: use the non-interactive form (-m, --yes, CI=1), never an editor, pager or REPL. stdout and stderr are merged.
Not for what does not exit on its own (npm run dev, tsc -w, tail -f): what a command leaves running is stopped when it returns. Not for reading, searching or editing a file, but one perl -pi command makes the same substitution across many files.
Chain dependent steps with &&.`,
			false,
			shellArgs{},
			shellRequests,
			typed(runShell)),

		gated(mk(ToolRead, readDesc+" Each line starts with its number and a tab, which are not part of the file. Do not read lines you already have again unless something other than your own edits changed the file.", true,
			readArgs{},
			pathReq(perm.SubjectRead, pathIn, ""),
			typed(runRead))),

		gated(edits(mk(ToolWrite, "Create a file or replace its whole content; parent directories are created. An existing file must have been read first, and not changed since. To change part of a file use edit.", false,
			writeArgs{},
			changeReq(ToolWrite),
			typed(runWrite)))),

		gated(edits(mk(ToolEdit, "Replace an exact substring in a file. old_string must match exactly once, as written and without read's line-number prefix. Keep it as small as still unique; edits and writes sent together apply in order.", false,
			editArgs{},
			changeReq(ToolEdit),
			typed(runEdit)))),

		mk(ToolGlob, "List files matching a glob under path, newest first, skipping .gitignored ones. A pattern without / matches names at any depth; one with / is relative to path.", true,
			globArgs{},
			pathReq(perm.SubjectRead, searchRoot, ""),
			typed(runGlob)),

		mk(ToolGrep, "Search file contents under path, skipping .gitignored, binary and over 4 MB files, with an RE2 regular expression (no lookaround or backreferences; (?i) ignores case). Returns up to 100 matching lines as path:line: text.", true,
			grepArgs{},
			pathReq(perm.SubjectRead, searchRoot, ""),
			typed(runGrep)),

		mk(ToolTodos, "Track several distinct steps, or several tasks from the user, in a list the user sees; never a single step. Pass the whole list every time, with exactly one item in_progress while work remains, and mark an item done in the same response as the next step's first calls. An item left in_progress when the turn ends goes back to pending, so mark the next one in_progress to pick it up. Do not repeat the list in your reply.",
			false,
			todosArgs{},
			nil,
			typed(runTodos)),

		mk(ToolQuestion, `Ask the user up to 4 questions and wait for the answers. Only for what they alone can decide: requirements, scope, behavior, preferences, tradeoffs; never what reading code would tell you, approval for a routine action, or whether a plan is ready. Put independent questions in one call; ask separately when one answer changes another. Choice questions take 2-4 options with 1-5 word labels, the one you recommend first with its label ending in "(Recommended)". Never add an "Other" option: the user can always type their own answer.`,
			false,
			questionArgs{},
			nil,
			typed(runQuestion)),

		mk(ToolPlan, `Present an implementation plan and wait for the user's verdict before changing anything. Call it only before a large or ambiguous change (a clear task is done, not planned, and a question is answered), or as a read-only agent once nothing is left to decide.

The plan is short markdown someone could implement without guessing: the goal, what changes where (files, and code to reuse as path:line), how to verify, the assumptions, and only the approach you recommend. End with a Critical Files section naming the 3-5 files that matter most.`,
			false,
			planArgs{},
			nil,
			typed(runPlan)),

		mk(ToolJobStart, "Start a command that runs long or never exits (dev server, watcher, a slow build you need not wait on) and return a job id at once. There is no timeout field: a job runs until it exits or job_kill stops it. Read its output with job_read, whose wait_ms returns once a server's ready line prints, so never sleep; job_kill what you no longer need.",
			true,
			jobStartArgs{},
			shellRequests,
			typed(runJobStart)),

		mk(ToolJobRead, "Read what a background job printed since your last read of it, and whether it is still running.",
			true,
			jobReadArgs{},
			nil,
			typed(runJobRead)),

		mk(ToolJobList, "List background jobs with their state.",
			true,
			jobListArgs{},
			nil,
			typed(runJobList)),

		mk(ToolJobKill, "Stop a background job and everything it started.",
			false,
			jobKillArgs{},
			nil,
			typed(runJobKill)),

		mk(ToolMemory, "Keep one durable fact for future sessions: when the user says to remember something, states a preference or corrects you, or you hit a gotcha the code does not say. One sentence under 300 characters as it holds now, naming the file, command or flag it is about (\"the user wants commits made without being asked once checks pass\", \"the goldens are regenerated by make golden\"); never an instruction, a log of this session, a secret, or what a future session re-derives in seconds from AGENTS.md or one file read. Quote only the user's words; a repository procedure is a recipe. Path writes a private brain file (its frontmatter needs description and read_if; the body is read lazily, only the catalog injects); a user-partition brain file goes through the file tools instead.",
			false,
			MemoryCall{},
			nil,
			typed(runMemory)),

		mk(ToolSkill, "Load a listed skill's instructions by name. Not for a keyword match alone, and not for a skill already loaded.",
			true,
			skillArgs{},
			nil,
			typed(runSkill)),
	}
}

// MemoryCall is the memory tool's parsed input (DESIGN §10.4): text mode when
// Path and Promote and Query are empty, file mode - a private-partition
// brain file - on Path, promote mode on Promote, search mode on Query.
// The json and desc tags are its schema source.
type MemoryCall struct {
	Scope       store.Scope `json:"scope,omitempty" desc:"project (default): this repository only; user: holds in every project"`
	Text        string      `json:"text,omitempty"`
	Quote       string      `json:"quote,omitempty" desc:"the user's exact words from this session that this memory rests on"`
	Replaces    string      `json:"replaces,omitempty" desc:"the memory to rewrite, or to drop when text is empty: its id from the ## Memory section"`
	Path        string      `json:"path,omitempty" desc:"file mode: a freeform .md in the private brain (a bare file name lands there); promote mode: the .md file name in the promoted line's brain dir"`
	Content     string      `json:"content,omitempty" desc:"file mode: the file's full content, 32 KiB at most"`
	Promote     string      `json:"promote,omitempty" desc:"promote mode: the memory id to move into a brain file"`
	Description string      `json:"description,omitempty" desc:"promote mode: the catalog line's description (required, one line)"`
	ReadIf      string      `json:"read_if,omitempty" desc:"promote mode: when to read the file (required, one line)"`
	Query       string      `json:"query,omitempty" desc:"search mode: words to match against memory text or id, both scopes, live and ledger; the other fields are ignored"`
}

func runMemory(_ context.Context, p *Pipeline, call MemoryCall) (fantasy.ToolResponse, error) {
	if p.Remember == nil {
		return fantasy.NewTextErrorResponse("memory unavailable"), nil
	}
	if call.Scope == "" {
		call.Scope = store.ScopeProject
	}
	msg, err := p.Remember(p, call)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(msg), nil
}

// ChangeDiff renders what write or edit would do to its file as
// a unified diff against the current content. When an edit does not apply
// (the tool will report that), it falls back to changePreview.
func ChangeDiff(tool ToolName, in map[string]any) string {
	path := str(in["path"])
	// it runs before the gate, on the model's path: a FIFO or /dev/zero
	// must not block or exhaust memory
	fi, err := os.Stat(path)
	if err == nil && (!fi.Mode().IsRegular() || fi.Size() > 1<<20) {
		return changePreview(in)
	}
	data, _ := os.ReadFile(path)
	old := lf(string(data))
	var updated string
	switch tool {
	case ToolWrite:
		updated = lf(str(in["content"]))
	case ToolEdit:
		var msg string
		updated, msg = editText(old, str(in["old_string"]), str(in["new_string"]), workRel(path))
		if msg != "" {
			return changePreview(in)
		}
	default:
		return changePreview(in)
	}
	return udiff.Unified(path, path, old, updated)
}

// changePreview renders a file tool's change for the permission overlay
// from the input alone (the file is not read before the gate): a unified
// diff with one unnumbered hunk per replaced or new text.
func changePreview(in map[string]any) string {
	lines := []string{"--- " + str(in["path"]), "+++ " + str(in["path"])}
	hunk := func(old, new string) {
		if old == "" && new == "" {
			return
		}
		lines = append(lines, "@@")
		for _, p := range [][2]string{{"-", old}, {"+", new}} {
			if p[1] == "" {
				continue
			}
			for l := range strings.SplitSeq(strings.TrimRight(p[1], "\n"), "\n") {
				lines = append(lines, p[0]+l)
			}
		}
	}
	hunk("", str(in["content"]))
	hunk(str(in["old_string"]), str(in["new_string"]))
	return strings.Join(lines, "\n")
}

// shellRequests routes the whole command through the tool.shell subject.
func shellRequests(_ *Pipeline, in map[string]any) ([]Request, error) {
	return []Request{{
		Subject: perm.SubjectShell, Arg: str(in["command"]), Description: str(in["about"]),
	}}, nil
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

// searchRoot is where glob and grep look: path, or the working directory.
// NOTE: the walk starts from this raw string, not the resolved root the
// gate judged, so a directory swapped for a symlink after the check is walked.
func searchRoot(in map[string]any) string {
	if s := str(in["path"]); s != "" {
		return s
	}
	return "."
}

// TruncOut cuts tool output to at most n bytes, keeping the start and the
// larger end: a command prints its errors and its summary last. Both cuts
// move back to a line break within half their side, so no line shows half,
// and the marker names the cut lines, the offset to read them at in a spill
// file; one huge line (minified JSON) is cut by bytes.
func TruncOut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	h, t := n*3/10, len(s)-(n-n*3/10)
	hl, tl := strings.LastIndexByte(s[:h], '\n'), strings.IndexByte(s[t:], '\n')
	if hl < h/2 || tl < 0 || tl > (len(s)-t)/2 {
		return strings.ToValidUTF8(s[:h], "") +
			fmt.Sprintf("\n... (%d bytes cut) ...\n", t-h) +
			strings.ToValidUTF8(s[t:], "")
	}
	h, t = hl+1, t+tl+1
	a := strings.Count(s[:h], "\n") + 1
	return s[:h] + fmt.Sprintf("... (lines %d-%d of %d cut) ...\n", a, a+strings.Count(s[h:t], "\n")-1, strings.Count(strings.TrimSuffix(s, "\n"), "\n")+1) + s[t:]
}

// cutLinesMarker is the marker TruncOut leaves where it dropped whole lines.
var cutLinesMarker = regexp.MustCompile(`^\.\.\. \(lines (\d+)-(\d+) of \d+ cut\) \.\.\.$`)

// CutLines reads that marker back: the first and last line dropped, in the
// numbering of the text the cut came from.
func CutLines(line string) (from, to int, ok bool) {
	m := cutLinesMarker.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	from, _ = strconv.Atoi(m[1])
	to, _ = strconv.Atoi(m[2])
	return from, to, true
}
