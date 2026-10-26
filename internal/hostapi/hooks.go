// hooks.go runs Claude plugin hooks (DESIGN §5): hooks.json entries mapped
// to the zemi events that consult them and fired as shell children, fire and
// forget, off the caller's goroutine. Their output is logged, never
// interpreted; only a SessionStart capture reads stdout, for session context.

package hostapi

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
)

// hookTimeout is the longest a hook runs: Claude's default, and the cap on
// one that asked for more. A SessionStart capture holds its hooks to
// hookCaptureWait, so no send path can wait on one, and keeps the text it
// gathers to hookContextMax, the scale of a skill body worth of prompt.
const (
	hookTimeout     = 60 * time.Second
	hookCaptureWait = 10 * time.Second
	hookContextMax  = 16 << 10
)

// hookSessionStart keys the registry's SessionStart capture. Nothing emits
// it: it is not an event, and captureSessionContext is its only reader.
const hookSessionStart = "session.start"

// hookEventMap maps the Claude events zemi runs to the zemi events that
// consult them. Everything else is refused at load: PreToolUse and
// PermissionRequest would need deny semantics zemi's gate does not give a
// subprocess (a hook's output is logged, never interpreted),
// UserPromptSubmit would tax every submit (SessionStart is the supported
// route), and the rest have no counterpart.
var hookEventMap = map[string]string{
	"PostToolUse":      EventToolPost,
	"PermissionDenied": EventToolDenied,
	"Stop":             EventAgentFinish,
	"SessionEnd":       EventSessionClose,
	"SubagentStart":    EventAgentSubagentStart,
	"SubagentStop":     EventAgentSubagentFinish,
	"SessionStart":     hookSessionStart,
}

// hookEntry is one hook command the registry fires.
type hookEntry struct {
	root, data string // the child's cwd; CLAUDE_PLUGIN_ROOT and _DATA
	event      string // the Claude event name, as the payload names it
	command    string
	timeout    time.Duration
	match      func(string) bool // the tool a tool event must name, nil = all
}

// hookReg holds one generation's plugin hooks by the zemi event that
// consults them; the plugins step rebuilds it from the plugin dirs, and a
// reload purges the old generation's before the replay.
type hookReg struct {
	mu     sync.RWMutex
	gen    Gen
	events map[string][]hookEntry
}

func (r *hookReg) set(gen Gen, events map[string][]hookEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen, r.events = gen, events
}

func (r *hookReg) handlers(event string) []hookEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.events[event]
}

func (r *hookReg) purge(gen Gen) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen == gen {
		r.gen, r.events = 0, nil
	}
}

// buildHooks translates one plugin's Claude hook groups into registry
// entries; a refused or skipped entry is named in the log and the plugin
// otherwise loads.
func (h *Host) buildHooks(name, root, data string, groups []plugin.ClaudeHookGroup) map[string][]hookEntry {
	var events map[string][]hookEntry
	add := func(event string, e hookEntry) {
		events[event] = append(events[event], e)
	}
	for _, g := range groups {
		zevent, ok := hookEventMap[g.Event]
		if !ok {
			h.Log.Warn("plugin hook event has no zemi equivalent, dropped", "origin", "hooks", "plugin", name, "event", g.Event)
			continue
		}
		for _, m := range g.Matchers {
			// a SessionStart matcher names the start's source, not a tool
			match := hookMatcherFor(m.Matcher, g.Event != "SessionStart")
			for _, hk := range m.Hooks {
				switch {
				case hk.Type != "command":
					h.Log.Warn("plugin hook type not supported, skipped", "origin", "hooks", "plugin", name, "event", g.Event, "type", hk.Type)
				case hk.If != "":
					h.Log.Warn("plugin hook is gated by an if condition, skipped", "origin", "hooks", "plugin", name, "event", g.Event)
				case hk.Command == "":
					h.Log.Warn("plugin hook has no command, skipped", "origin", "hooks", "plugin", name, "event", g.Event)
				default:
					if events == nil {
						events = map[string][]hookEntry{}
					}
					timeout := hookTimeout
					if hk.Timeout > 0 {
						timeout = min(time.Duration(hk.Timeout)*time.Second, hookTimeout)
					}
					add(zevent, hookEntry{
						root: root, data: data, event: g.Event, command: hk.Command,
						timeout: timeout, match: match,
					})
				}
			}
		}
	}
	return events
}

// hookMatcherList is the exact-or-pipe matcher form, whose tokens name tools.
var hookMatcherList = regexp.MustCompile(`^[a-zA-Z0-9_|]+$`)

// hookMatcherFor builds one matcher's match: "" or "*" takes everything, an
// exact-or-pipe list names the tokens as written (lowercased, tools through
// the alias map when mapTool), anything else is a regular expression, an
// invalid one matching nothing (as Claude's do).
func hookMatcherFor(m string, mapTool bool) func(string) bool {
	if m == "" || m == "*" {
		return nil
	}
	if hookMatcherList.MatchString(m) {
		want := set.Set[string]{}
		for tok := range strings.SplitSeq(m, "|") {
			tok = strings.ToLower(tok)
			if mapTool {
				tok = string(claudeTool(tok))
			}
			want.Add(tok)
		}
		return func(name string) bool { return want.Has(name) }
	}
	re, err := regexp.Compile(m)
	if err != nil {
		return func(string) bool { return false }
	}
	return re.MatchString
}

// claudeToolAlias maps the Claude tool names zemi knows to theirs; anything
// else lowercases.
var claudeToolAlias = map[string]tools.ToolName{
	"bash": tools.ToolShell,
	"task": tools.ToolAgent, "todowrite": tools.ToolTodos, "todoread": tools.ToolTodos,
}

// claudeTool maps a Claude tool name to zemi's.
func claudeTool(name string) tools.ToolName {
	if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
		if m, ok := claudeToolAlias[n]; ok {
			return m
		}
		return tools.ToolName(n)
	}
	return ""
}

// consultHooks fires one zemi event's plugin hooks, fire-and-forget: one
// goroutine runs the matching entries in turn, each as a shell child. Safe
// from any goroutine, the UI's included; an inert boot starts no child.
func (h *Host) consultHooks(event string, payload map[string]any) {
	entries := h.hookReg.handlers(event)
	if len(entries) == 0 || proc.Refused() != nil {
		return
	}
	h.GoSafe("plugin hooks "+event, func() {
		sid := h.AgentSessionID()
		for _, e := range entries {
			if tool, _ := payload["tool_name"].(string); e.match != nil && !e.match(tool) {
				continue
			}
			full := make(map[string]any, len(payload)+4)
			maps.Copy(full, payload)
			// Claude hooks read a permission_mode from the payload; zemi
			// has none for them to steer
			full["cwd"], full["permission_mode"], full["hook_event_name"] = h.Dirs.ProjectDir, "bypassPermissions", e.event
			// the payload may name the session it is about, not the live one
			if _, ok := full["session_id"]; !ok {
				full["session_id"] = sid
			}
			h.runHook(e, marshalHookPayload(full), e.timeout)
		}
	})
}

func marshalHookPayload(payload map[string]any) []byte {
	data, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return nil
	}
	return data
}

// runHook runs one hook command: the payload on stdin, the plugin root as
// the working directory and the plugin vars in the environment. Output is
// logged, never interpreted; the stdout returns for the SessionStart
// capture.
func (h *Host) runHook(e hookEntry, input []byte, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := proc.ShellCommandContext(ctx, e.command)
	cmd.Dir = e.root
	cmd.Env = slices.Concat(os.Environ(), proc.NonInteractiveEnv,
		[]string{"CLAUDE_PLUGIN_ROOT=" + e.root, "CLAUDE_PLUGIN_DATA=" + e.data})
	cmd.Stdin = bytes.NewReader(input)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := proc.Run(cmd)
	h.Log.Debug("plugin hook ran", "origin", "hooks", "event", e.event, "command", clipRunes(e.command, 200),
		"stdout", truncRunes(strings.TrimSpace(out.String()), 2048), "stderr", truncRunes(strings.TrimSpace(errOut.String()), 500),
		"error", errString(err))
	return out.String()
}

// captureSessionContext runs the plugins' SessionStart hooks for sid and
// keeps what they print for the session's next prompt, beside the project
// skills index. kind is the session start's source (startup, resume), what
// the hooks' matchers name. It starts off the session-creation path, so no
// send waits on it; a capture that lands drops the pin unconditionally, so
// one in flight stores no prompt without the text, and the next send
// rebuilds with it. A reload never re-runs it: this is session state, not
// registry state.
func (h *Host) captureSessionContext(sid, kind string) {
	entries := h.hookReg.handlers(hookSessionStart)
	if len(entries) == 0 || proc.Refused() != nil {
		return
	}
	h.GoSafe("plugin sessionstart hooks", func() {
		var texts []string
		for _, e := range entries {
			if e.match != nil && !e.match(kind) {
				continue
			}
			if out := hookContextOut(h.runHook(e, marshalHookPayload(h.hookBase(e)), min(e.timeout, hookCaptureWait))); out != "" {
				texts = append(texts, out)
			}
		}
		if len(texts) == 0 {
			return
		}
		text := "\n\n# Plugin context\n" + strings.Join(texts, "\n\n")
		if len(text) > hookContextMax {
			// ToValidUTF8: the cut can land mid-rune, and this text joins
			// the prompt and the store
			text = strings.ToValidUTF8(text[:hookContextMax], "") +
				fmt.Sprintf("\n(plugin context cut here: %d bytes left out)", len(text)-hookContextMax)
		}
		ag := h.agents
		ag.mu.Lock()
		defer ag.mu.Unlock()
		if ag.sessionID != sid { // the session moved on while the hooks ran
			return
		}
		ag.hookCtx = text
		// a build in flight holds pin.key as empty too: only a drop counted
		// by it makes that build throw its text-less prompt away
		ag.dropPin()
	})
}

// hookBase is the session part of the payload a SessionStart hook reads.
func (h *Host) hookBase(e hookEntry) map[string]any {
	return map[string]any{
		"session_id": h.AgentSessionID(), "cwd": h.Dirs.ProjectDir,
		"permission_mode": "bypassPermissions", "hook_event_name": e.event,
	}
}

// hookContextOut is what one SessionStart hook's stdout contributes: the
// additionalContext of a hookSpecificOutput, else the text itself.
func hookContextOut(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	var parsed struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(out), &parsed) == nil && parsed.HookSpecificOutput.AdditionalContext != "" {
		return parsed.HookSpecificOutput.AdditionalContext
	}
	return out
}
