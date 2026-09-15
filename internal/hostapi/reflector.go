// reflector.go: after a session closes, a tool-less model call reads the
// transcript, tool stats and memory and skill indexes, and proposes
// memories, config patches and skills (DESIGN §10.3). Every proposal goes
// through intake and the autonomy gate.
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

// Transcript budget for the reflect prompt: at most reflectMsgsMax
// messages, each truncated to reflectMsgMax runes, tool outputs harder.
const (
	reflectMsgsMax = 120
	reflectMsgMax  = 400
	reflectToolMax = 200
)

// reflectorSystem is the reflector prompt.
const reflectorSystem = `You are zemi's reflector. You read a finished coding session and propose durable improvements for future sessions in the same project.

Rules:
- Propose only durable, generalizable knowledge: facts, gotchas, preferences, conventions, decisions. Never propose transient details (file contents, session chatter, this conversation).
- Never propose secrets, credentials, tokens or key material; quote at most short identifiers, never whole values.
- Do not duplicate what is already in the memory index. Set "supersedes" to an existing memory's id only when your new memory genuinely replaces it.
- Each memory is one self-contained single line, at most 500 characters.
- Config patches may only add permission rules: an array of rule objects with subject, pattern, decision (deny|ask|allow), optional scope ("permanent" or "session"), optional not_match, and match / not_match_example strings (example commands the pattern must / must not match). Never put Lua, event handlers, tool registrations, keymaps or autonomy knobs inside a config patch.
- Skills are reusable procedures for a task that will recur (how to run the integration tests, how to cut a release): propose one only when the session shows the procedure clearly. name is lowercase-kebab-case, description says when to use it, body is concise markdown steps with exact commands. No code to execute and no secrets.
- Propose nothing rather than something weak.

Answer with ONLY a JSON document, no prose and no code fences, shaped like:
{"memories":[{"scope":"project","kind":"gotcha","text":"...","tags":["x"],"supersedes":""}],"config":[{"action":"patch","title":"...","rules":[{"subject":"tool.bash","pattern":"rm -rf *","decision":"deny","scope":"permanent","match":"rm -rf build","not_match":"rm -f build"}],"risk":"safe"}],"skills":[{"name":"run-integration-tests","description":"when asked to run the local integration tests","body":"1. docker compose up -d db\n2. go test -tags integration ./..."}]}
kind is one of: fact, gotcha, preference, convention, decision. scope is "project" or "user". Omit "memories", "config" and "skills" entirely when nothing is worth keeping.`

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
	Skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	} `json:"skills"`
}

// reflectEveryN reads the trigger gate: reflect every N closed sessions,
// 0 disables.
func (h *Host) reflectEveryN() int {
	if n := h.setInt("reflector.every_n_sessions", 1); n >= 0 {
		return n
	}
	return 1
}

// reflectOnClose is the session-close trigger (/new, /resume, startup):
// the closed session is reflected when the closed-session counter hits
// the every-N gate and it has messages the reflector has not seen.
func (h *Host) reflectOnClose(prevID string) {
	ag := h.agents
	ag.mu.Lock()
	ag.reflected++
	count := ag.reflected
	ag.mu.Unlock()
	if n := h.reflectEveryN(); n == 0 || count%n != 0 {
		return
	}
	if !h.reflectDue(prevID) {
		return
	}
	h.runReflectDetached(prevID)
}

// ReflectLastSession runs at startup: the most recent session gets the
// session-close treatment, so quitting never waits on a model call and
// its proposals land in the persistent review queue (DESIGN §10.3).
func (h *Host) ReflectLastSession() {
	if h.DB == nil {
		return
	}
	if list, err := h.DB.ListSessions(1); err == nil && len(list) == 1 {
		h.reflectOnClose(list[0].ID)
	}
}

// reflectDue reports whether a session has messages the reflector has not
// seen; the seen count lives in kv, so a restart does not re-reflect.
func (h *Host) reflectDue(sid string) bool {
	if h.DB == nil {
		return false
	}
	msgs, err := h.DB.Messages(sid)
	if err != nil || len(msgs) == 0 {
		return false
	}
	seen, _ := h.DB.GetKV(reflectSeenKey(sid))
	return seen != fmt.Sprint(len(msgs))
}

func reflectSeenKey(sid string) string { return "reflector.seen." + sid }

// runReflectDetached starts one reflect run on its own goroutine and
// reports whether it started; a second concurrent run is skipped (one
// reflector at a time). Progress reaches Lua as ctx.reflecting plus the
// reflector.start / reflector.done events (done carries error on failure).
func (h *Host) runReflectDetached(sid string) bool {
	if !h.reflectMu.TryLock() {
		h.Log.Debug("reflector", "run skipped: another run is active", map[string]any{"session": sid})
		return false
	}
	ms := h.setInt("reflector.timeout_ms", 120000)
	if ms <= 0 {
		ms = 120000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(ms)*time.Millisecond)
	h.reflecting.Store(true)
	h.refresh()
	h.EmitAsync("reflector.start", map[string]any{"session": sid})
	h.GoSafe("reflector", func() {
		defer h.reflectMu.Unlock()
		defer cancel()
		defer h.refresh()
		defer h.reflecting.Store(false)
		err := h.reflectSession(ctx, sid)
		h.agents.mu.Lock()
		h.agents.reflectErr = errString(err)
		h.agents.mu.Unlock()
		if err != nil {
			h.Log.Warn("reflector", "run failed", map[string]any{"session": sid, "error": err.Error()})
			h.EmitAsync("reflector.done", map[string]any{"session": sid, "error": err.Error()})
		}
	})
	return true
}

// reflectSession runs one full reflect cycle for a session: prompt, turn,
// parse, route. Synchronous; the caller owns the timeout.
func (h *Host) reflectSession(ctx context.Context, sid string) error {
	prompt, err := h.reflectPrompt(sid)
	if err != nil {
		return err
	}
	model, err := h.modelFor("reflector.model")
	if err != nil {
		return err
	}
	out, err := generateText(ctx, model, reflectorSystem, prompt)
	if err != nil {
		return err
	}
	proposal, err := parseReflectProposal(out)
	if err != nil {
		return err
	}
	stored, queued, superseded, skipped := h.applyReflectProposal(sid, proposal)
	cfgQueued, cfgApplied, cfgSkipped := h.applyConfigProposals(sid, proposal)
	drafted := h.applySkillDrafts(sid, proposal)
	if msgs, err := h.DB.Messages(sid); err == nil {
		_ = h.DB.SetKV(reflectSeenKey(sid), fmt.Sprint(len(msgs)))
	}
	result := map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped,
		"config_queued": cfgQueued, "config_applied": cfgApplied,
		"config_skipped": cfgSkipped, "skills_drafted": drafted,
	}
	h.Log.Info("reflector", "run complete", result)
	h.EmitAsync("reflector.done", result)
	return nil
}

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
	if props := h.memoryProposals(); len(props) > 0 {
		b.WriteString("\n# Memories awaiting review\n")
		for _, m := range props {
			b.WriteString(fmt.Sprintf("- [%s/%s] %s\n", m.Scope, m.Kind, truncRunes(m.Text, 80)))
		}
	}
	if mems, _ := h.memoryList(false); len(mems) > 0 {
		b.WriteString("\n# Current memory index\n")
		for _, m := range mems {
			line := fmt.Sprintf("- %s [%s/%s] %s", m.ID, m.Scope, m.Kind, truncRunes(m.Text, 80))
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
	// Stored messages hold TextPart; model responses hold TextContent.
	for _, p := range parts {
		switch t := any(p).(type) {
		case fantasy.TextPart:
			b.WriteString(t.Text)
		case fantasy.TextContent:
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func truncRunes(s string, n int) string {
	if c := clipRunes(s, n); c != s {
		return c + "..."
	}
	return s
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// generateText runs one tool-less, single-step model call (reflector,
// compaction) and returns its text.
func generateText(ctx context.Context, model fantasy.LanguageModel, system, prompt string) (string, error) {
	ag := fantasy.NewAgent(model, fantasy.WithSystemPrompt(system))
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
// intake and the autonomy gate and returns counters; supersedes link when
// the memory is stored, now or on accept. The model's scope/kind/text are intake
// inputs, re-validated and scrubbed there; a bad entry is skipped, never
// fatal.
func (h *Host) applyReflectProposal(sid string, p *reflectProposal) (stored, queued, superseded, skipped int) {
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
		res.Supersedes = pm.Supersedes
		switch ok, err := h.gateMemory(res); {
		case err != nil:
			skipped++
			h.Log.Warn("reflector", "apply failed", map[string]any{"error": err.Error()})
		case !ok:
			queued++
		default:
			stored++
			if _, old, err := h.locateMemory(pm.Supersedes); pm.Supersedes != "" && err == nil && old.SupersededBy == res.Memory.ID {
				superseded++
			}
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
			applied++
		} else {
			h.queueCfgProposal(prop)
			queued++
		}
	}
	return queued, applied, skipped
}

// applySkillDrafts writes each proposed skill as a disabled learned draft;
// invalid or already-taken names are skipped with a log.
func (h *Host) applySkillDrafts(sid string, p *reflectProposal) (drafted int) {
	for _, s := range p.Skills {
		if err := h.draftLearnedSkill(sid, s.Name, s.Description, s.Body); err != nil {
			h.Log.Debug("reflector", "skill draft skipped", map[string]any{"name": s.Name, "error": err.Error()})
			continue
		}
		h.EmitAsync("skill.learned", map[string]any{"name": s.Name, "session": sid})
		drafted++
	}
	if drafted > 0 {
		h.syncSkills()
	}
	return drafted
}

// linkSupersede marks an existing memory as replaced by a freshly stored
// one; unknown ids and cross-scope links are dropped with a log.
func (h *Host) linkSupersede(oldID string, fresh store.Memory) {
	db, old, err := h.locateMemory(oldID)
	if err != nil {
		h.Log.Debug("reflector", "supersede skipped: old memory missing", map[string]any{"id": oldID})
		return
	}
	if db != h.memoryDB(fresh.Scope) || old.Scope != fresh.Scope {
		h.Log.Debug("reflector", "supersede skipped: scope mismatch", map[string]any{
			"id": oldID, "old_scope": old.Scope, "new_scope": fresh.Scope,
		})
		return
	}
	if err := db.SupersedeMemory(old.ID, fresh.ID); err != nil {
		h.Log.Warn("reflector", "supersede failed", map[string]any{"id": oldID, "error": err.Error()})
	}
}

// ReflectorCommandsStep registers the Go-owned /reflect command (builtin
// origin, user config may rebind it).
func (h *Host) ReflectorCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "reflector commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "reflect", Hidden: true,
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
