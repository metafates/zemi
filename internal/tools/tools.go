// Package tools ships the builtin model tools. Every call runs schema
// validation, tool.pre, the permission gate, the tool, then tool.post;
// failures are tool errors, never panics.
package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
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

// Builtin tool names, one definition each: everything that refers to a tool
// by name (the registry here, and the host's gate, prompt and history logic)
// uses these, so renaming a tool is one line, not a recount of literals.
const (
	ToolBash        = "bash"
	ToolRead        = "read"
	ToolWrite       = "write"
	ToolEdit        = "edit"
	ToolMultiedit   = "multiedit"
	ToolGlob        = "glob"
	ToolGrep        = "grep"
	ToolFetch       = "fetch"
	ToolWebSearch   = "web_search"
	ToolTodos       = "todos"
	ToolQuestion    = "question"
	ToolPlan        = "plan"
	ToolSkill       = "skill"
	ToolJobStart    = "job_start"
	ToolJobRead     = "job_read"
	ToolJobList     = "job_list"
	ToolJobKill     = "job_kill"
	ToolMemory      = "memory"
	ToolDiagnostics = "diagnostics"
	ToolLSP         = "lsp"
	// Host tools built by the host, not here, but named by the same rule.
	ToolAgent    = "agent"
	ToolWorkflow = "workflow"
	ToolLua      = "lua"
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
	// Unsandboxed is the model's sandbox: false, a Lua tool script that
	// widens what zemi allows, or a model memory edit zemi does not apply on
	// its own: no mode settles the request, the user answers it
	Unsandboxed bool
	// Network is the model's network: true, which keeps the sandbox and adds
	// outbound sockets. The URL is the model's, so no mode settles it either.
	Network bool
}

// Verdict is the pipeline outcome for one permission request.
type Verdict struct {
	Decision  perm.Decision
	Reason    string
	DecidedBy string     // rule, hook, user, overlay, default
	Rule      *perm.Rule // the rule behind a "rule" verdict, nil otherwise
}

// DeniedText is the tool error a refused gate returns: who decided, what
// it was about, and the reason the asker gave.
func DeniedText(v Verdict, what string) string {
	s := "permission denied (" + v.DecidedBy + "): " + what
	if v.Reason == "" {
		return s
	}
	return s + ": " + v.Reason
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
// reason is why it asked when that is not the request's own reason ("not
// settled: this project is not trusted (/trust)"), "" otherwise.
type Asker func(req Request) (allowed bool, decidedBy, reason string)

// Pipeline wires tools to the permission store and the host hooks.
type Pipeline struct {
	Store     *perm.Store
	Pre       PreHook // nil = no tool.pre events
	Ask       Asker   // nil = asks fail closed
	Post      func(tool string, input map[string]any, ok bool, output string, changed []string)
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
	// Images reports whether the model takes image parts; nil = yes. A
	// text-only model gets a text error from read instead of a refused
	// request that ends the turn.
	Images func() bool

	// Remember runs the memory tool through the host's intake and autonomy
	// gate and returns the answer: what became of the memory and what to do
	// next. nil = memory tool error.
	Remember func(scope, text, quote, replaces string) (string, error)

	// Scratch reports a path inside the session scratchpad: writes there
	// pass a read-only agent. nil = no scratchpad.
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
	// multiedit result (language server diagnostics) and may rewrite the
	// file (a recipe's scrub); nil = nothing.
	AfterWrite func(path string) string

	// Diagnostics backs the diagnostics tool; nil = unavailable.
	Diagnostics func(path string) (string, error)

	// LSP returns the language server covering path, with the file synced
	// to it; nil = no lsp tool support.
	LSP func(ctx context.Context, path string) (LSPSession, error)

	// Denied reports a call the gate refused (the tool.denied event); nil
	// = silent.
	Denied func(tool string, req Request, v Verdict)

	// Plan reports a read-only agent: file writes are denied and allowed commands
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
	// SandboxParam gives bash and job_start the sandbox: false escape; set
	// while a backend is live and someone can answer the ask.
	SandboxParam bool
	// Sandboxed is this run's policy, set on the per-run copy once the gate
	// settled a command by mode; nil = unconfined.
	Sandboxed *sandbox.Policy
	// Protected reports a write to .git, a .zemi or zemi's own dirs: a rule
	// allow for it still asks, since broad "always" globs cannot exclude
	// every such path; nil = none.
	Protected func(path string) bool
	// Secret reports a secret-shaped path (.env, *.pem, id_rsa): an
	// "always" answer allows a whole directory, and its label does not say
	// "including every secret in it"; nil = none.
	Secret func(path string) bool
	// Guard runs before a sandboxed command starts; the func it returns runs
	// once the command exited, and its text ends the result. nil = none.
	Guard func(sandbox.Policy) func() string
	// Changes runs before a bash command starts in dir; the func it returns
	// runs once it ended and reports the files it changed: a line for the
	// model, a diff for the row (the result's Metadata) and every path. nil = none.
	Changes func(dir string) func() (note, diff string, paths []string)

	// changed is what a bash run found changed, kept on its per-run copy for post
	changed []string
	// verdict is the gate's answer to the run's last request, kept on the
	// per-run copy: fetch reads which rule allowed the URL
	verdict Verdict
}

// LSPSession is a language server ready for one file's queries.
type LSPSession struct {
	Client *lsp.Client
	Server string
	// Open sends a file's text from disk to the server, so edits the tool
	// made to other files are seen before the next query.
	Open func(path string) error
}

// PathSubject reports a subject whose argument is a file path.
func PathSubject(subject string) bool { return subject == "fs.read" || subject == "fs.write" }

// CheckDenied is Check plus the tool.denied notification a refused verdict
// owes its handlers: tools that gate themselves outside Run use it, so an
// audit handler sees every refusal.
func (p *Pipeline) CheckDenied(req Request) Verdict {
	v := p.Check(req)
	if v.Decision != perm.Allow && p.Denied != nil {
		p.Denied(req.Tool, req, v)
	}
	return v
}

// Check decides one request; for tool.bash the store normalizes the
// command (segments, redirect gates, assignment gate) in one call.
func (p *Pipeline) Check(req Request) Verdict {
	plan := p.Plan != nil && p.Plan()
	if plan && req.Subject == "fs.write" && (p.Scratch == nil || !p.Scratch(req.Arg)) {
		return Verdict{Decision: perm.Deny, Reason: "the user is planning: describe this change in the plan instead of making it", DecidedBy: "profile:readonly"}
	}
	// read-only holds under every mode: yolo would settle the escape and
	// run the command unconfined, with the project writable
	if plan && req.Subject == "tool.bash" && req.Unsandboxed {
		return Verdict{Decision: perm.Deny, Reason: "the user is planning: commands stay in the sandbox, which keeps the project read-only", DecidedBy: "profile:readonly"}
	}
	// rule is the one that allowed the request, nil when a default did
	loosen := func(v Verdict, rule *perm.Rule) Verdict {
		switch {
		case v.Decision != perm.Allow:
		case p.RewriteLoosened:
			return p.resolveAsk(req, "rewritten input re-checked (allow flipped to ask)")
		case req.Subject == "fs.write" && p.Protected != nil && p.Protected(req.Arg):
			return p.resolveAsk(req, "this changes .git, .zemi or zemi's own files")
		// an "always" answer allows the file's whole directory; a rule the
		// user wrote in init.lua says what it means and stays authoritative
		case PathSubject(req.Subject) && rule != nil && rule.Principal == "model" && p.Secret != nil && p.Secret(req.Arg):
			return p.resolveAsk(req, "this file looks like it holds secrets")
		// "allow cat" was answered for a file the user saw; a project file
		// line names the program alone. The user's init.lua rule stays.
		case req.Subject == "tool.bash" && rule != nil && (rule.Principal == "model" || rule.Source == "permissions") && p.Secret != nil && perm.ReadsSecret(req.Arg, p.Secret):
			return p.resolveAsk(req, "this file looks like it holds secrets")
		// rule-allowed commands run unconfined, so only readers skip the ask
		case plan && req.Subject == "tool.bash" && perm.ReadCommand(req.Arg, nil):
		case plan && (req.Subject == "tool.bash" || strings.HasPrefix(req.Subject, "mcp.")):
			return p.resolveAsk(req, "read-only agent: commands that change things ask")
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
		return loosen(Verdict{perm.Allow, o.Reason, decidedBy(o), o.Rule}, o.Rule)
	case perm.Deny:
		return Verdict{perm.Deny, o.Reason, decidedBy(o), o.Rule}
	}
	return p.resolveAsk(req, o.Reason)
}

// SandboxFor is the policy a checked command runs under: one a permission
// mode settled runs confined, rule-allowed and user-approved ones as they
// are. nil = unconfined.
func (p *Pipeline) SandboxFor(req Request, v Verdict) *sandbox.Policy {
	if p.Sandbox == nil || req.Subject != "tool.bash" || v.Decision != perm.Allow || req.Unsandboxed {
		return nil
	}
	// network: true keeps the sandbox, which is what the model and the user
	// were told, so it stays confined whoever allowed it; no mode settles it,
	// so the verdict is always the user's
	if !strings.HasPrefix(v.DecidedBy, "mode:") && !req.Network {
		return nil
	}
	pol := p.Sandbox()
	if pol != nil && req.Network {
		pol.Network = true
	}
	return pol
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
	limits := sandboxLimits()
	if p.Sandboxed != nil && len(p.Sandboxed.Write) > 0 {
		// the exact paths, so a retry does not guess at a sibling of the
		// scratchpad or a temp dir the policy never named
		limits += ". Writable, these directories and what is below them only: " + strings.Join(p.Sandboxed.Write, ", ")
	}
	if !p.SandboxParam {
		return "\nThe sandbox may have blocked this: " + limits + "."
	}
	if p.Plan != nil && p.Plan() {
		// sandbox: false is denied for a read-only agent, so it is not offered
		return "\nThe sandbox may have blocked this: " + limits + ". The project stays read-only while planning; network: true (which asks the user) adds only the network."
	}
	retry := " If the command needs the network, call it again with network: true, which keeps the sandbox and asks the user; sandbox: false, which drops every limit, is the last resort."
	if !sandbox.LikelyNetwork(out, err) {
		retry = " If the command needs more, call it again with network: true (the network, sandbox kept) or sandbox: false (no limits at all); both ask the user."
	}
	return "\nThe sandbox may have blocked this: " + limits + "." + retry
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
	s := "no network, writes only in the scratchpad and a private $TMPDIR, the project read-only"
	switch {
	case b.EnforcesProtect && runtime.GOOS == "linux":
		s = "no network at all, not even to servers running on this machine; writes only in the project (except for a read-only agent), the scratchpad and a private $TMPDIR; .git and .zemi read-only"
	case b.EnforcesProtect:
		s = "no network beyond localhost, writes only in the project (except for a read-only agent), the scratchpad and a private $TMPDIR, .git and .zemi read-only"
	}
	if b.HidesSecretFiles {
		return s + ", secret files unreadable"
	}
	return s + ", home credential files unreadable"
}

// NetworkProp is the middle tier: the sandbox stays, the command gets
// sockets. It is the answer to almost every sandbox denial, since the file
// rules are the ones a settled command is meant to keep.
func NetworkProp() map[string]any {
	return map[string]any{"type": "boolean", "description": "keeps the sandbox and adds the network (go mod download, npm install, curl); asks the user, so say why in about. Try this before sandbox: false, which drops every limit."}
}

// SandboxProp is the escape bash and job_start take while a backend is live.
func SandboxProp() map[string]any {
	return map[string]any{"type": "boolean", "description": "a command zemi settles on its own runs sandboxed (" + sandboxLimits() + "); false runs it unconfined and asks the user, so say why in about. Only after network: true was not enough."}
}

func decidedBy(o perm.Outcome) string {
	if o.Rule != nil {
		return "rule"
	}
	return "default"
}

func (p *Pipeline) resolveAsk(req Request, reason string) Verdict {
	if p.Ask == nil {
		// say why it asked: nobody will, and the caller retries blind otherwise
		if req.Reason != "" {
			reason += " (asked because " + req.Reason + ")"
		}
		return Verdict{Decision: perm.Ask, Reason: reason + "; no overlay available, failing closed", DecidedBy: "default"}
	}
	allowed, by, why := p.Ask(req)
	if allowed {
		return Verdict{Decision: perm.Allow, DecidedBy: by}
	}
	// who said no and why it asked; the rule text ("no rule matched") says neither
	who := "denied (" + by + ")"
	switch {
	case strings.HasPrefix(by, "user"):
		// they saw the request and refused it: the ask's reason says why it
		// asked, which after a no is not why the call was denied
		if why != "" {
			return Verdict{Decision: perm.Deny, Reason: "denied by the user (it asked because " + why + ")", DecidedBy: by}
		}
		return Verdict{Decision: perm.Deny, Reason: "denied by the user", DecidedBy: by}
	case by == "default" || by == "unsandboxed":
		who = "nobody could answer the ask"
		// nobody saw the ask, so the reason it asked is all the caller gets
		if why == "" {
			why = req.Reason
		}
	case by == "event":
		who = "denied by a permission.request handler"
	case strings.HasPrefix(by, "mode:"):
		who = "denied by permission mode " + strings.TrimPrefix(by, "mode:")
	}
	if why != "" {
		who += ": " + why
	}
	return Verdict{Decision: perm.Deny, Reason: who, DecidedBy: by}
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
	// edits: the tool changes a file it must have read unchanged (write,
	// edit, multiedit), checked before the gate
	edits bool
	// check refuses input the tool could not act on (a URL that is no
	// URL) before the gate, so the user is never asked about it; nil = none
	check func(p *Pipeline, input map[string]any) error
}

func (t *tool) Info() fantasy.ToolInfo                     { return t.info }
func (t *tool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *tool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *tool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.edits {
		// the calls before this one on the same file land first, so the
		// stale check and the edit see what they wrote; released on every
		// return, or the next call on the file would wait out the step
		defer turn(call.ID)()
	}
	input, err := ParseInput(t.info, call.Input)
	if err != nil {
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
				if err := Validate(t.info, rewritten); err != nil {
					return fantasy.NewTextErrorResponse("rewrite invalid: " + err.Error()), nil
				}
				input = rewritten
				rp := *t.pipe
				rp.RewriteLoosened = loosened
				pipe = &rp
			}
		}
	}
	if fi, err := os.Lstat(str(input["path"])); t.edits && err == nil && fi.Mode()&os.ModeSymlink != 0 {
		// a write through the link would change a file the row does not name
		target := workRel(perm.RealPath(str(input["path"])))
		return t.post(pipe, input, fantasy.NewTextErrorResponse(fmt.Sprintf("%s is a symbolic link to %s; edit %s instead", str(input["path"]), target, target))), nil
	}
	if t.gated {
		// resolved once: the gate judges and the tool opens the same path, so
		// a symlink a sandboxed command plants later cannot redirect the tool
		input = maps.Clone(input)
		input["path"] = perm.RealPath(str(input["path"]))
	}
	if t.edits {
		// before the gate, so the user is never asked to approve a change
		// the tool then refuses; the tool checks again under the file lock
		switch fi, err := os.Stat(str(input["path"])); {
		case err == nil:
			if msg := pipe.stale(str(input["path"]), fi); msg != "" {
				return t.post(pipe, input, fantasy.NewTextErrorResponse(msg)), nil
			}
		case t.info.Name != ToolWrite && errors.Is(err, os.ErrNotExist):
			// a missing file is write's normal case; an edit of one is a typo
			return t.post(pipe, input, fantasy.NewTextErrorResponse(pipe.notFound(str(input["path"]), err))), nil
		}
	}
	if t.check != nil {
		if err := t.check(pipe, input); err != nil {
			return t.post(pipe, input, fantasy.NewTextErrorResponse(err.Error())), nil
		}
	}
	run := *pipe
	for _, req := range t.subjects(input) {
		req.Tool = t.info.Name
		v := pipe.CheckDenied(req)
		run.verdict = v
		if v.Decision == perm.Deny {
			what := req.Arg
			if PathSubject(req.Subject) {
				what = workRel(what) // as the ask showed it
			}
			return t.post(pipe, input, fantasy.NewTextErrorResponse(DeniedText(v, req.Subject+" "+what))), nil
		}
		if v.Decision == perm.Ask {
			return t.post(pipe, input, fantasy.NewTextErrorResponse("permission unresolved (ask failed): "+req.Subject+" "+req.Arg+": "+v.Reason)), nil
		}
		if pol := pipe.SandboxFor(req, v); pol != nil {
			run.Sandboxed = pol
		}
	}
	res, err := t.run(ctx, &run, input)
	return t.post(&run, input, res), err
}

// post delivers the tool.post notification with a text summary of the
// response; Post is optional.
func (t *tool) post(p *Pipeline, input map[string]any, res fantasy.ToolResponse) fantasy.ToolResponse {
	if p.Post == nil {
		return res
	}
	changed := p.changed
	if t.edits && !res.IsError {
		changed = []string{str(input["path"])}
	}
	p.Post(t.info.Name, input, !res.IsError, res.Content, changed)
	return res
}

// ParseInput decodes a call's input and runs Validate on it: the one place
// every tool, builtin or host-implemented, gets its type errors worded.
func ParseInput(info fantasy.ToolInfo, input string) (map[string]any, error) {
	var m map[string]any
	if strings.TrimSpace(input) == "" {
		input = "{}"
	}
	var typeErr *json.UnmarshalTypeError
	switch err := json.Unmarshal([]byte(input), &m); {
	case errors.As(err, &typeErr):
		return nil, errors.New("input must be a JSON object")
	case err != nil:
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	return m, Validate(info, m)
}

// Validate is the schema check every tool call passes before its events and gate.
func Validate(info fantasy.ToolInfo, input map[string]any) error {
	return validateFields(info.Parameters, info.Required, input, "")
}

// validateFields checks required fields, the type of every field sent, then
// the fields of every object in an array: a lax provider can drop a nested
// field, and a multiedit without new_string would delete the matched text.
func validateFields(props map[string]any, required []string, input map[string]any, at string) error {
	for _, r := range required {
		if input[r] == nil {
			return fmt.Errorf("missing required field %q", at+r)
		}
	}
	for k, v := range input {
		ps, _ := props[k].(map[string]any)
		// null is how some providers leave an optional field out
		if t, _ := ps["type"].(string); v != nil && !typeOK(t, v) {
			return fmt.Errorf("field %q must be %s", at+k, t)
		}
		items, _ := ps["items"].(map[string]any)
		req, _ := items["required"].([]string)
		list, _ := v.([]any)
		for i, e := range list {
			if m, ok := e.(map[string]any); ok {
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

func edits(t fantasy.AgentTool) fantasy.AgentTool {
	t.(*tool).edits = true
	return t
}

func checked(t fantasy.AgentTool, check func(*Pipeline, map[string]any) error) fantasy.AgentTool {
	t.(*tool).check = check
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
			props["sandbox"], props["network"] = SandboxProp(), NetworkProp()
		}
		return props
	}

	return []fantasy.AgentTool{
		mk(ToolBash, `Run a command with sh -c in the project directory. POSIX sh, not bash: for [[ ]], arrays or pipefail run bash -c '...'. Each call is a new shell, so cd and exports do not carry over; stdin is empty and there is no terminal. stdout and stderr are merged and long output is cut, with the whole of it saved to a file the result names. At the timeout the command is not killed but handed on as a background job whose id the result gives you.
Not for anything that does not exit on its own (npm run dev, tsc -w, tail -f): & and nohup do not help. Not for interactive programs (vim, less, REPLs, git rebase -i): use the non-interactive form (git --no-pager, -m, --yes, CI=1). Not for files when you have read, grep, glob, edit and write: what a command prints does not count as reading a file.
Chain dependent steps with &&; independent commands are separate calls in one response.`,
			false,
			withSandbox(map[string]any{
				// "about", not "description": a schema's properties reach the model
				// sorted by name, and this one has to stream before the command
				"about":      map[string]any{"type": "string", "description": "what the command does in 5-10 words, shown to the user in place of the command, e.g. Run the unit tests; with network: true or sandbox: false, also what it needs that for"},
				"command":    map[string]any{"type": "string"},
				"timeout_ms": map[string]any{"type": "number", "description": "default 60000, max 600000; raise it for slow builds and test suites"},
			}),
			[]string{"command"},
			func(in map[string]any) []Request {
				return bashRequests(str(in["command"]), in)
			},
			runBash),

		gated(mk(ToolRead, "Read a file (text or image), or list a directory one level deep. This is the read that edit and write require; cat in bash does not count. Each line comes prefixed with its 1-based number and a tab, which is not file content: never copy it into edit. One call returns at most 2000 lines or 50000 characters and ends with the offset to continue from; lines over 2000 characters are cut. Once read, do not read a file again unless it changed.", true,
			map[string]any{
				"path":   map[string]any{"type": "string", "description": "absolute, or relative to the project root"},
				"offset": map[string]any{"type": "number", "description": "1-based line to start at"},
				"limit":  map[string]any{"type": "number", "description": "how many lines to read"},
			},
			[]string{"path"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: str(in["path"])}}
			},
			runRead)),

		gated(edits(mk(ToolWrite, "Create a file, or overwrite one with its full new content (parent directories are created). Prefer edit for an existing file, which must have been read this session or the call is refused.", true,
			map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			[]string{"path", "content"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Preview: ChangeDiff("write", in)}}
			},
			runWrite))),

		gated(edits(mk(ToolEdit, "Replace an exact substring in a file. The file must have been read with the read tool this session and be unchanged since; otherwise read it and retry. old_string must match exactly once, indentation included (never read's line-number prefix): add surrounding lines to make it unique, or set replace_all. Keep it as small as still unique; whole lines also match when only whitespace differs. A failed match means the text differs: read that region again and copy it, never guess. Do not re-read after success.", true,
			map[string]any{
				"path":        map[string]any{"type": "string"},
				"old_string":  map[string]any{"type": "string"},
				"new_string":  map[string]any{"type": "string"},
				"replace_all": map[string]any{"type": "boolean"},
			},
			[]string{"path", "old_string", "new_string"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Preview: ChangeDiff("edit", in)}}
			},
			runEdit))),

		gated(edits(mk(ToolMultiedit, "Apply several edits to one file in order, each seeing the result of the one before, under edit's rules. Prefer it over several edit calls to one file. If any edit fails, nothing is written.", true,
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
				return []Request{{Subject: "fs.write", Arg: str(in["path"]), Preview: ChangeDiff("multiedit", in)}}
			},
			runMultiedit))),

		mk(ToolGlob, "List files matching a glob under path, newest first, skipping .gitignored ones. A pattern without / matches names at any depth.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob such as *.go, src/**/*.ts or *.{ts,tsx}"},
				"path":    map[string]any{"type": "string", "description": "directory to search under; default the project root"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: searchRoot(in)}}
			},
			runGlob),

		mk(ToolGrep, "Search file contents under path, skipping .gitignored ones, with an RE2 regular expression (no lookaround or backreferences; (?i) ignores case). Returns up to 100 matching lines as path:line: text. A match is not a read: edit still needs the read tool.", true,
			map[string]any{
				"pattern": map[string]any{"type": "string", "description": "RE2 regular expression matched against each line"},
				"path":    map[string]any{"type": "string", "description": "file or directory to search; default the project root"},
				"include": map[string]any{"type": "string", "description": "glob limiting which files are searched, such as *.go"},
			},
			[]string{"pattern"},
			func(in map[string]any) []Request {
				return []Request{{Subject: "fs.read", Arg: searchRoot(in)}}
			},
			runGrep),

		checked(mk(ToolFetch, "Fetch a URL over http/https and return the body as text (an HTML page becomes its readable article as markdown, without the navigation and boilerplate). Private addresses are refused unless the user's config allows the host.",
			true,
			map[string]any{
				"url": map[string]any{"type": "string"},
			},
			[]string{"url"},
			func(in map[string]any) []Request {
				// the real host, so a rule (or an "always allow") matches the
				// per-hop check runFetch makes later; check refused a bad URL
				u, _ := ParseNetURL(str(in["url"]))
				return []Request{{Subject: "net." + u.Hostname(), Arg: str(in["url"])}}
			},
			runFetch), func(_ *Pipeline, in map[string]any) error {
			_, err := ParseNetURL(str(in["url"]))
			return err
		}),

		mk(ToolTodos, "Track several distinct steps, or several tasks from the user, in a list the user sees. Never a single-step list. Pass the whole list every time; keep exactly one item in_progress while work remains; mark each done the moment it finishes, as the very next tool call, never batching the updates, so the visible list always equals your current state - never report a step done in your reply before the list shows it. Without todos it returns the list.",
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

		mk(ToolQuestion, `Ask the user up to 4 questions and wait for the answers. Only for what they alone can decide: requirements, scope, expected behavior, preferences, tradeoffs. Never ask what reading code would tell you, never ask to approve a routine action, and never ask whether a plan is ready (use plan). Put independent questions in one call; ask separately when one answer changes another. Every question needs a type (see the schema): yes_no for a yes/no, single_choice and multi_choice for selectable options (2-4, 1-5 word labels, the one you recommend first ending in "(Recommended)"; multi_choice lets the user mark several), free_text for an open answer. Add a short markdown description when a question needs context the options cannot carry. Never add an "Other": the user can always type their own answer.`,
			false,
			map[string]any{
				"questions": map[string]any{"type": "array", "description": "1 to 4, shown as tabs the user moves between before submitting", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":        map[string]any{"type": "string", "enum": []string{"yes_no", "single_choice", "multi_choice", "free_text"}, "description": "how the question is asked"},
						"question":    map[string]any{"type": "string", "description": "the complete question, ending with a question mark"},
						"description": map[string]any{"type": "string", "description": "optional markdown shown under the question"},
						"header":      map[string]any{"type": "string", "description": "a 1-3 word tab label, e.g. Auth method"},
						"options": map[string]any{"type": "array", "description": "required for yes_no, single_choice and multi_choice", "items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string"},
								"description": map[string]any{"type": "string"},
							},
							"required": []string{"label"},
						}},
					},
					"required": []string{"type", "question"},
				}},
			},
			[]string{"questions"},
			func(in map[string]any) []Request { return nil },
			runQuestion),

		mk(ToolPlan, `Present an implementation plan and wait for the user to approve it before changing anything. As a read-only agent, call it once nothing is left to decide; outside it, only before a large or ambiguous change. A clear task is done, not planned, and a research question is answered, not planned; ask open questions with the question tool first.

The plan is short markdown someone could implement without guessing: the goal, what changes where (files, and code to reuse as path:line), how to verify, the assumptions, and only the approach you recommend. End with a Critical Files section naming the 3-5 files that matter most. The user approves it (implement it, and you return to the agent that ran before), asks for changes (revise and call again), or dismisses it (stop and wait).`,
			false,
			map[string]any{"plan": map[string]any{"type": "string", "description": "the plan in markdown"}},
			[]string{"plan"},
			func(in map[string]any) []Request { return nil },
			runPlan),

		checked(mk(ToolWebSearch, "Search the web: up to max_results (default 10, max 20) results as title, URL and snippet. Keep queries short; fetch the results worth reading. A question needing several searches and pages goes to the web agent instead.",
			true,
			map[string]any{
				"query":       map[string]any{"type": "string"},
				"max_results": map[string]any{"type": "number"},
			},
			[]string{"query"},
			func(in map[string]any) []Request {
				// check refused a bad web.search_url
				u, _, _ := searchURL(p, str(in["query"]))
				return []Request{{Subject: "net." + u.Hostname(), Arg: u.String(), Reason: "web search"}}
			},
			runWebSearch), func(p *Pipeline, in map[string]any) error {
			// a bad search URL is the user's config, not a request to gate
			_, _, err := searchURL(p, str(in["query"]))
			return err
		}),

		mk(ToolJobStart, "Start a process that runs long or never exits (dev server, watcher, a slow build you need not wait on) and return a job id at once. Without args, command is a shell line run with sh -c; with args, it is the program. Read its output with job_read (a server's ready line, not sleep) and job_kill what you no longer need.",
			true,
			withSandbox(map[string]any{
				"about":   map[string]any{"type": "string", "description": "what the process is in 3-6 words, shown to the user, e.g. Dev server; with network: true or sandbox: false, also what it needs that for"},
				"command": map[string]any{"type": "string"},
				"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"env":     map[string]any{"type": "object", "description": "extra environment variables"},
			}),
			[]string{"command"},
			func(in map[string]any) []Request {
				env, _ := in["env"].(map[string]any)
				return bashRequests(jobLine(str(in["command"]), strList(in["args"]), env), in)
			},
			runJobStart),

		mk(ToolJobRead, "Read what a background job printed since your last read of it, and whether it is still running.",
			true,
			map[string]any{
				"id": map[string]any{"type": "string"},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobRead),

		mk(ToolJobList, "List background jobs with their state.",
			true,
			map[string]any{},
			nil,
			func(in map[string]any) []Request { return nil },
			runJobList),

		mk(ToolJobKill, "Stop a background job and everything it started.",
			false,
			map[string]any{
				"id": map[string]any{"type": "string"},
			},
			[]string{"id"},
			func(in map[string]any) []Request { return nil },
			runJobKill),

		mk(ToolMemory, "Keep one durable fact for future sessions: call it when the user says to remember something, states a preference or corrects you, and when you hit a gotcha the code does not say. One sentence, at most 300 characters, as it holds now, naming the file, command or flag it is about (\"the user wants commits made without being asked once checks pass\", \"make golden regenerates the goldens\"); never an instruction, a log of this session or a description of a change. Only what the user taught you or you verified, and only what a future session could not re-derive in seconds: not what AGENTS.md, a tool description or one file read already says. Anything about the user needs quote: their exact words from this session. A repository procedure is a recipe, not a memory. Never a secret: name the env var that holds it. replaces=<id> rewrites that line under # Memory; empty text with replaces drops it. A memory shows from the next session on; it may be queued for the user's review.",
			false,
			map[string]any{
				"text":     map[string]any{"type": "string"},
				"scope":    map[string]any{"type": "string", "enum": []string{"project", "user"}, "description": "project (default): this repository only; user: holds in every project"},
				"quote":    map[string]any{"type": "string", "description": "the user's exact words this memory rests on"},
				"replaces": map[string]any{"type": "string", "description": "the memory to rewrite, or to drop when text is empty: its id from # Memory, the first 8 characters or more"},
			},
			[]string{"text"},
			func(in map[string]any) []Request { return nil },
			runMemory),

		mk(ToolDiagnostics, "Report a file's compiler and linter diagnostics from its language server. write, edit and multiedit already append errors for the file they change, so use this for a file you did not change.",
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

		mk(ToolLSP, lspToolDesc, false, lspToolProps, []string{"action", "path"}, lspRequests, runLSP),

		mk(ToolSkill, "Load a skill's full instructions by name. Call it before the first edit, write or bash of any task a listed skill covers, even a small one you know how to do: the description is the trigger, the body is the procedure. Not for a keyword match alone, and not for a skill already loaded.",
			true,
			map[string]any{
				"name": map[string]any{"type": "string"},
			},
			[]string{"name"},
			func(in map[string]any) []Request { return nil },
			runSkill),
	}
}

func runMemory(_ context.Context, p *Pipeline, in map[string]any) (fantasy.ToolResponse, error) {
	if p.Remember == nil {
		return fantasy.NewTextErrorResponse("memory unavailable"), nil
	}
	scope := str(in["scope"])
	if scope == "" {
		scope = "project"
	}
	msg, err := p.Remember(scope, str(in["text"]), str(in["quote"]), str(in["replaces"]))
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
		if updated, _, _, msg = applyEdits(old, edits, workRel(path)); msg != "" {
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
	return []Request{{Subject: "tool.bash", Arg: cmd, Description: cmp.Or(str(in["about"]), str(in["description"])),
		Unsandboxed: in["sandbox"] == false, Network: in["network"] == true}}
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

// TruncOut cuts tool output to at most n bytes, keeping the start and the
// larger end: a command prints its errors and its summary last.
func TruncOut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := n * 3 / 10
	return strings.ToValidUTF8(s[:head], "") +
		fmt.Sprintf("\n... (%d characters cut) ...\n", len(s)-n) +
		strings.ToValidUTF8(s[len(s)-(n-head):], "")
}
