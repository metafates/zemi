// reflector.go drives the §10.3 reflector: after a session closes, a
// headless tool-less model turn reads the transcript, tool stats, the
// memory index (ids, kinds, tags, a short gist) and the skills index, and
// answers with a JSON proposal document (v1: memories and supersedes
// only). Every proposed memory rides the §10.4 intake and the autonomy
// gate; Go stamps provenance with the reflected session id and the
// auto-apply decision never reads the proposal's own fields. Runs are
// serialized and detached with a timeout; the shutdown run detaches the UI
// hooks first so a review prompt can never block a dead program.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// reflectShutdownCap bounds the shutdown reflect so quitting never hangs
// long even with a generous configured timeout.
const reflectShutdownCap = 15 * time.Second

// Transcript budget for the reflect prompt: at most reflectMsgsMax
// messages, each truncated to reflectMsgMax runes, tool outputs harder.
const (
	reflectMsgsMax = 120
	reflectMsgMax  = 400
	reflectToolMax = 200
)

// reflectorSystem is the shipped reflector prompt (task profile in DESIGN
// §6.3; the profile itself ships with M5 subagents).
const reflectorSystem = `You are zemi's reflector. You read a finished coding session and propose durable improvements for future sessions in the same project.

Rules:
- Propose only durable, generalizable knowledge: facts, gotchas, preferences, conventions, decisions. Never propose transient details (file contents, session chatter, this conversation).
- Never propose secrets, credentials, tokens or key material; quote at most short identifiers, never whole values.
- Do not duplicate what is already in the memory index. Set "supersedes" to an existing memory's id only when your new memory genuinely replaces it.
- Each memory is one self-contained single line, at most 512 characters.
- Config patches may only add permission rules: an array of rule objects with subject, pattern, decision (deny|ask|allow), optional scope ("permanent" or "session"), optional not_match, and match / not_match_example strings (example commands the pattern must / must not match). Never put Lua, event handlers, tool registrations, keymaps or autonomy knobs inside a config patch.
- Propose nothing rather than something weak.

Answer with ONLY a JSON document, no prose and no code fences, shaped like:
{"memories":[{"scope":"project","kind":"gotcha","text":"...","tags":["x"],"supersedes":""}],"config":[{"action":"patch","title":"...","rules":[{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","scope":"permanent","match":"rm -rf build","not_match":"rm -f build"}],"risk":"safe"}]}
kind is one of: fact, gotcha, preference, convention, decision. scope is "project" or "user". Omit "memories" and "config" entirely when nothing is worth keeping.`

// reflectProposal is the schema-validated model output. Config patches
// stay raw at decode time: intakeConfigPatch strict-decodes each one so
// unknown fields refuse the patch (DESIGN §10.3).
type reflectProposal struct {
	Memories []struct {
		Scope      string   `json:"scope"`
		Kind       string   `json:"kind"`
		Text       string   `json:"text"`
		Tags       []string `json:"tags"`
		Supersedes string   `json:"supersedes"`
	} `json:"memories"`
	Config []json.RawMessage `json:"config"`
}

// reflectEveryN reads the trigger gate: reflect every N closed sessions,
// 0 disables.
func (h *Host) reflectEveryN() int {
	if v, ok := h.Settings.Get("reflector.every_n_sessions"); ok {
		if n, ok := v.(int); ok && n >= 0 {
			return n
		}
	}
	return 1
}

// reflectTimeout reads the run timeout.
func (h *Host) reflectTimeout() time.Duration {
	ms := 120000
	if v, ok := h.Settings.Get("reflector.timeout_ms"); ok {
		if n, ok := v.(int); ok && n > 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
}

// reflectModelID picks the reflector model: reflector.model, else the
// shared agent model.
func (h *Host) reflectModelID() string {
	if v, ok := h.Settings.Get("reflector.model"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return h.currentModelID(nil)
}

// reflectOnClose is the session-rollover trigger: a new session started,
// so the previous one closed. Reflects it when the closed-session counter
// hits the every-N gate.
func (h *Host) reflectOnClose(prevID string) {
	if prevID == "" {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	ag.reflected++
	count := ag.reflected
	ag.mu.Unlock()
	if n := h.reflectEveryN(); n == 0 || count%n != 0 {
		return
	}
	h.runReflectDetached(prevID)
}

// runReflectDetached starts one reflect run on its own goroutine and
// reports whether it started; a second concurrent run is skipped (one
// reflector at a time). Progress reaches Lua as ctx.reflecting plus the
// reflector.start / reflector.done events (done carries error on failure).
func (h *Host) runReflectDetached(sid string) bool {
	if !h.reflectMu.TryLock() {
		h.Log.Debug("reflector", "run skipped: another run is active", map[string]any{"session": sid})
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.reflectTimeout())
	h.reflecting.Store(true)
	h.refresh()
	h.EmitAsync("reflector.start", map[string]any{"session": sid})
	go func() {
		defer h.reflectMu.Unlock()
		defer cancel()
		defer h.refresh()
		defer h.reflecting.Store(false)
		if err := h.reflectSession(ctx, sid); err != nil {
			h.Log.Warn("reflector", "run failed", map[string]any{"session": sid, "error": err.Error()})
			h.EmitAsync("reflector.done", map[string]any{"session": sid, "error": err.Error()})
		}
	}()
	return true
}

// ShutdownReflect runs the reflector over the live session once, after the
// UI is gone: hooks are detached first (a review prompt would block
// forever on a dead program), the run is bounded by the configured timeout
// capped at reflectShutdownCap, and failures are logged. Under "ask"
// autonomy everywhere it is skipped entirely: the proposals would park in
// memory and die with the process (DESIGN §10.4), so the LLM call would be
// pure burn and would freeze the quit for seconds.
func (h *Host) ShutdownReflect() {
	sid := h.AgentSessionID()
	if sid == "" {
		return
	}
	if !h.shutdownReflectUseful() {
		h.Log.Info("reflector", "shutdown reflect skipped: autonomy ask parks proposals that die with the process", map[string]any{"session": sid})
		return
	}
	if !h.reflectMu.TryLock() {
		h.Log.Info("reflector", "shutdown reflect skipped: run already active", map[string]any{"session": sid})
		return
	}
	defer h.reflectMu.Unlock()
	h.SetUIHooks(nil)
	timeout := h.reflectTimeout()
	if timeout > reflectShutdownCap {
		timeout = reflectShutdownCap
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := h.reflectSession(ctx, sid); err != nil {
		h.Log.Warn("reflector", "shutdown reflect failed", map[string]any{"session": sid, "error": err.Error()})
	}
}

// ShutdownReflectPending reports whether ShutdownReflect would run: a live
// session exists and some autonomy knob could persist its proposals.
// main.go uses it to print a notice before the bounded wait.
func (h *Host) ShutdownReflectPending() bool {
	return h.AgentSessionID() != "" && h.shutdownReflectUseful()
}

// shutdownReflectUseful reports whether any autonomy knob is set to
// "auto": under "ask" the shutdown reflect's output dies with the process.
func (h *Host) shutdownReflectUseful() bool {
	for _, path := range []string{"memory.autonomy", "config.autonomy"} {
		if v, ok := h.Settings.Get(path); ok {
			if s, ok := v.(string); ok && s == "auto" {
				return true
			}
		}
	}
	return false
}

// reflectSession runs one full reflect cycle for a session: prompt, turn,
// parse, route. Synchronous; the caller owns the timeout.
func (h *Host) reflectSession(ctx context.Context, sid string) error {
	prompt, err := h.reflectPrompt(sid)
	if err != nil {
		return err
	}
	model, err := h.reflectModel()
	if err != nil {
		return err
	}
	out, err := h.reflectTurn(ctx, model, prompt)
	if err != nil {
		return err
	}
	proposal, err := parseReflectProposal(out)
	if err != nil {
		return err
	}
	stored, queued, superseded, skipped := h.applyReflectProposal(sid, proposal)
	cfgQueued, cfgApplied, cfgSkipped := h.applyConfigProposals(sid, proposal)
	h.Log.Info("reflector", "run complete", map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped,
		"config_queued": cfgQueued, "config_applied": cfgApplied,
		"config_skipped": cfgSkipped,
	})
	h.EmitAsync("reflector.done", map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped,
		"config_queued": cfgQueued, "config_applied": cfgApplied,
		"config_skipped": cfgSkipped,
	})
	return nil
}

// reflectPrompt assembles the reflector user message.
func (h *Host) reflectPrompt(sid string) (string, error) {
	if h.DB == nil {
		return "", errors.New("reflector: session store unavailable")
	}
	msgs, err := h.DB.Messages(sid)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Session transcript\n")
	shown := 0
	for _, m := range msgs {
		if shown == reflectMsgsMax {
			b.WriteString(fmt.Sprintf("... (%d more messages omitted)\n", len(msgs)-shown))
			break
		}
		line := reflectLine(m)
		if line == "" {
			continue
		}
		shown++
		b.WriteString(line + "\n")
	}
	if shown == 0 {
		return "", fmt.Errorf("reflector: session %s has no messages", sid)
	}
	if stats, err := h.DB.ToolCallStats(sid); err == nil && len(stats) > 0 {
		b.WriteString("\n# Tool usage\n")
		for _, s := range stats {
			line := fmt.Sprintf("- %s: %d calls", s.Tool, s.Calls)
			if s.Failed > 0 {
				line += fmt.Sprintf(", %d failed", s.Failed)
			}
			if s.Denied > 0 {
				line += fmt.Sprintf(", %d denied", s.Denied)
			}
			b.WriteString(line + "\n")
		}
	}
	if mems, err := h.memoryList(false); err == nil && len(mems) > 0 {
		b.WriteString("\n# Current memory index\n")
		for _, m := range mems {
			gist := m.Text
			if runes := []rune(gist); len(runes) > 80 {
				gist = string(runes[:80]) + "..."
			}
			line := fmt.Sprintf("- %s [%s/%s] %s", m.ID, m.Scope, m.Kind, gist)
			if len(m.Tags) > 0 {
				line += " #" + strings.Join(m.Tags, " #")
			}
			b.WriteString(line + "\n")
		}
	}
	if skills := h.skillIndex(); len(skills) > 0 {
		b.WriteString("\n# Skills\n")
		for _, s := range skills {
			b.WriteString("- " + s.Name + "\n")
		}
	}
	b.WriteString("\nProduce the proposal JSON now.")
	return b.String(), nil
}

// reflectLine renders one stored message as one prompt line.
func reflectLine(m *store.Message) string {
	switch m.Role {
	case "user", "assistant":
		var fm fantasy.Message
		if err := json.Unmarshal([]byte(m.PartsJSON), &fm); err != nil {
			return ""
		}
		text := strings.TrimSpace(concatText(fm.Content))
		if text == "" {
			return ""
		}
		return m.Role + ": " + truncRunes(text, reflectMsgMax)
	case "tool":
		var parts struct {
			Tool   string `json:"tool"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal([]byte(m.PartsJSON), &parts); err != nil {
			return ""
		}
		if parts.Tool == "" {
			return ""
		}
		return "tool " + parts.Tool + ": " + truncRunes(strings.TrimSpace(parts.Output), reflectToolMax)
	}
	return ""
}

// concatText joins the text parts of a fantasy content slice.
func concatText[T any](parts []T) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := any(p).(fantasy.TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// reflectModel resolves the reflector model; tests stub reflectModelFn.
func (h *Host) reflectModel() (fantasy.LanguageModel, error) {
	if h.reflectModelFn != nil {
		return h.reflectModelFn()
	}
	return h.Providers.Resolve(h.reflectModelID())
}

// reflectTurn runs the single-step tool-less headless turn.
func (h *Host) reflectTurn(ctx context.Context, model fantasy.LanguageModel, prompt string) (string, error) {
	ag := fantasy.NewAgent(model, fantasy.WithSystemPrompt(reflectorSystem))
	res, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages: []fantasy.Message{{
			Role:    fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: prompt}},
		}},
		StopWhen: []fantasy.StopCondition{fantasy.StepCountIs(1)},
	})
	if err != nil {
		return "", err
	}
	return concatText(res.Response.Content), nil
}

// parseReflectProposal extracts the JSON object from the model output
// (prose and fences tolerated) and schema-decodes it.
func parseReflectProposal(out string) (*reflectProposal, error) {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end <= start {
		return nil, errors.New("reflector: no JSON object in output")
	}
	var p reflectProposal
	if err := json.Unmarshal([]byte(out[start:end+1]), &p); err != nil {
		return nil, fmt.Errorf("reflector: bad proposal: %w", err)
	}
	return &p, nil
}

// applyReflectProposal routes every proposal memory through the §10.4
// intake and the autonomy gate, links supersedes for memories stored in
// this run, and returns counters. The model's scope/kind/text are intake
// inputs, re-validated and scrubbed there; a bad entry is skipped, never
// fatal.
func (h *Host) applyReflectProposal(sid string, p *reflectProposal) (stored, queued, superseded, skipped int) {
	type link struct {
		fresh store.Memory
		old   string
	}
	var links []link
	for _, pm := range p.Memories {
		scope := pm.Scope
		if scope == "" {
			scope = "project"
		}
		if scope != "project" && scope != "user" {
			skipped++
			h.Log.Debug("reflector", "memory skipped: bad scope", map[string]any{"scope": scope})
			continue
		}
		res, err := learn.Intake(scope, learn.OriginLearned, pm.Kind, pm.Text, pm.Tags, sid)
		if err != nil {
			skipped++
			h.Log.Debug("reflector", "memory skipped by intake", map[string]any{"error": err.Error()})
			continue
		}
		if h.memoryAutonomy() == "auto" && !res.InstructionShaped && h.autoApplyBudgetLeft() {
			if err := h.applyProposal(res); err != nil {
				skipped++
				h.Log.Warn("reflector", "apply failed", map[string]any{"error": err.Error()})
				continue
			}
			stored++
			if pm.Supersedes != "" {
				links = append(links, link{fresh: res.Memory, old: pm.Supersedes})
			}
		} else {
			h.queueProposal(res)
			queued++
		}
	}
	for _, l := range links {
		if h.linkSupersede(l.old, l.fresh) {
			superseded++
		}
	}
	return stored, queued, superseded, skipped
}

// applyConfigProposals routes each config patch through the strict §10.3
// intake (rule objects only, scrubbed leaves) and the config.autonomy
// gate: deny/ask session-scope patches may auto-apply under "auto",
// everything else queues for review. Returns counters.
func (h *Host) applyConfigProposals(sid string, p *reflectProposal) (queued, applied, skipped int) {
	for _, raw := range p.Config {
		prop, err := h.intakeConfigPatch(sid, raw)
		if err != nil {
			skipped++
			h.Log.Debug("reflector", "config patch skipped", map[string]any{"error": err.Error()})
			continue
		}
		if h.autoApplyCfgOK(prop) {
			h.applyCfgProposal(prop)
			ag := h.agents
			ag.mu.Lock()
			ag.cfgAuto += len(prop.Entries)
			ag.mu.Unlock()
			applied++
		} else {
			h.queueCfgProposal(prop)
			queued++
		}
	}
	return queued, applied, skipped
}

// linkSupersede marks an existing memory as replaced by one stored in this
// run; unknown ids and cross-scope links are dropped with a log.
func (h *Host) linkSupersede(oldID string, fresh store.Memory) bool {
	db, old, err := h.locateMemory(oldID)
	if err != nil {
		h.Log.Debug("reflector", "supersede skipped: old memory missing", map[string]any{"id": oldID})
		return false
	}
	if db != h.memoryDB(fresh.Scope) || old.Scope != fresh.Scope {
		h.Log.Debug("reflector", "supersede skipped: scope mismatch", map[string]any{
			"id": oldID, "old_scope": old.Scope, "new_scope": fresh.Scope,
		})
		return false
	}
	if err := db.SupersedeMemory(oldID, fresh.ID); err != nil {
		h.Log.Warn("reflector", "supersede failed", map[string]any{"id": oldID, "error": err.Error()})
		return false
	}
	return true
}

// ReflectorCommandsStep registers the Go-owned /reflect command (builtin
// origin, user config may rebind it).
func (h *Host) ReflectorCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "reflector commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name:   "reflect",
				Desc:   "distill this session into memories and config",
				GoFn:   func(h *Host, args string) error { return h.reflectCommand() },
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

// reflectCommand starts a detached reflect run over the live session. The
// cmdline runs on the UI goroutine, so the run must never block it.
func (h *Host) reflectCommand() error {
	sid := h.AgentSessionID()
	if sid == "" {
		return errors.New("no session to reflect on")
	}
	if !h.runReflectDetached(sid) {
		return errors.New("a reflect run is already active")
	}
	return nil
}
