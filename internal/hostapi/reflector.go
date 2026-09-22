// reflector.go: after a session closes, a tool-less model call reads the
// transcript, tool stats and memory and skill indexes, and proposes
// memories and skills (DESIGN §10.3). Memories go through intake and the
// autonomy gate; skills land as disabled drafts.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/store"
)

// Transcript budget for the reflect prompt: the last reflectMsgsMax user
// and assistant messages, each truncated to reflectMsgMax runes. Tool calls
// reach it only as usage stats.
// The memory index shows every live memory, text cut to reflectIndexText
// runes. Instruction files get reflectInstructionsMax bytes.
const (
	reflectMsgsMax         = 120
	reflectMsgMax          = 400
	reflectIndexText       = 160
	reflectInstructionsMax = 16 << 10
)

const reflectorSystem = `You are zemi's reflector. You read a finished coding session and propose durable improvements for future sessions.

Rules:
- Propose only knowledge that will change what a future session does and that it could not just re-derive: durable facts, gotchas, preferences, conventions and decisions about the project or the user. Reject a record of this session - what was done, which files changed, what got built or migrated or fixed. That value ends when the session does.
- A memory earns a proposal only when it is reusable: skipping it would cost a future session redoing work or making a mistake, and neither the code, the transcript nor the user could recover it in seconds. When in doubt, propose nothing.
- Scope every memory by where it holds. "project" is for this repository only (its layout, commands, conventions, decisions) and is shown in no other project. "user" is for what holds in every project: the user's own preferences and habits, their machine, tools and accounts, and models or providers. A fact about a repository is never "user" scope; a fact about the user or their setup is never "project" scope.
- Never propose secrets, credentials, tokens or key material; quote at most short identifiers, never whole values.
- Do not duplicate what is already in the memory index. Set "supersedes" to an existing memory's id only when your new memory genuinely replaces it. When the index holds many memories, merge overlapping ones into one that supersedes them instead of adding more.
- When the session shows an indexed memory is no longer true and nothing replaces it, retire it with an entry whose "supersedes" is its id and whose "text" is empty. The user reviews every retirement.
- Each memory is one self-contained single line, at most 300 characters, written as a fact about the project or the user ("the project regenerates golden files with make golden"), never as an instruction ("always run make golden", "you should ...").
- A "decision" or "preference" only when the user actually stated one this session. A discussion, an audit the user asked for (like "how would you improve X?"), or your own suggestion is not a decision; record what the user said or did, never what you inferred or advised.
- Skills are reusable procedures for a task that will recur (how to run the integration tests, how to cut a release): propose one only when the session shows the procedure clearly. name is lowercase-kebab-case, description says when to use it, body is concise markdown steps with exact commands. No code to execute and no secrets. Never propose a skill already listed under Skills, enabled or not. A procedure specific to this repository is a recipe the live agent writes: do not draft a skill for it.
- Do not capture what turns into a wrong rule once things change: failures caused by the environment (missing binaries or packages, unset credentials, fresh-install errors), claims that a tool or feature does not work, errors a retry fixed, one-off task narratives, attempts that never worked written up as a workflow, or anything the instruction files below or tool descriptions already say.
- Propose nothing rather than something weak.

Answer with ONLY a JSON document, no prose and no code fences, shaped like:
{"memories":[{"scope":"project","text":"...","supersedes":""}],"skills":[{"name":"run-integration-tests","description":"when asked to run the local integration tests","body":"1. docker compose up -d db\n2. go test -tags integration ./..."}]}
scope is "project" or "user". Omit "memories" and "skills" entirely when nothing is worth keeping.`

// reflectProposal is the schema-validated model output.
type reflectProposal struct {
	Memories []struct {
		Scope      string `json:"scope"`
		Text       string `json:"text"`
		Supersedes string `json:"supersedes"`
	} `json:"memories"`
	Skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	} `json:"skills"`
}

const (
	reflectTimeout = 2 * time.Minute
	// reflectLease is how long a reflection claim may stand without its
	// owner: a process that took the claim and then crashed leaves one,
	// and after this long another run may reclaim it.
	// NOTE: the claim is not refreshed while a run is live, so a process
	// paused past the lease (a long laptop sleep, SIGSTOP) mid-run could be
	// reclaimed and both runs would apply their proposals. Proposals are
	// deduped by text, so the cost is a duplicate, not corruption.
	reflectLease = 30 * time.Minute
)

// reflectOnClose is the session-close trigger (/new, /resume, startup):
// the closed session is reflected when it has messages the reflector has
// not seen. The cross-process claim makes two windows on the same
// repository reflect those messages once between them, not once each.
func (h *Host) reflectOnClose(prevID string) {
	if claim := h.claimReflect(prevID); claim != "" {
		h.runReflectDetached(prevID, claim)
	}
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

// reflectDue returns a session's message count when the reflector has not
// seen it at that count, 0 otherwise; the seen count lives in kv, so a
// restart does not re-reflect. One exchange that went fine teaches nothing
// and would cost a model call at every launch, so it waits for a second
// user message or a tool failure.
func (h *Host) reflectDue(sid string) int {
	if h.DB == nil {
		return 0
	}
	msgs, err := h.DB.Messages(sid)
	if err != nil || len(msgs) == 0 {
		return 0
	}
	if seen, _ := h.DB.GetKV(reflectSeenKey(sid)); seen == strconv.Itoa(len(msgs)) {
		return 0
	}
	users := 0
	for _, m := range msgs {
		if m.Role == "user" {
			users++
		}
	}
	if users < 2 {
		stats, _ := h.DB.ToolCallStats(sid)
		if !slices.ContainsFunc(stats, func(s store.ToolStat) bool { return s.Failed > 0 }) {
			return 0
		}
	}
	return len(msgs)
}

// claimReflect atomically claims the right to reflect the unseen messages
// of sid and returns the claim key when this process won it ("" to skip).
// The claim is an INSERT OR IGNORE keyed by the message count in the
// shared kv, so two processes that both see the session as unreflected can
// still not reflect it twice: only one inserts. A claim its owner crashes
// leaving is reclaimed after reflectLease, so a failed run can be retried.
func (h *Host) claimReflect(sid string) string {
	n := h.reflectDue(sid)
	if n == 0 {
		return ""
	}
	key := reflectClaimKey(sid, n)
	if claimed, _ := h.DB.SetKVOnce(key, "1"); claimed {
		return key
	}
	// a claim for this count stands: another window is on it, or its owner
	// crashed long ago and the lease lets us take over
	if ok, _ := h.DB.ReclaimKVIfStale(key, "1", time.Now().Add(-reflectLease).UnixMilli()); ok {
		return key
	}
	return ""
}

func reflectSeenKey(sid string) string { return "reflector.seen." + sid }
func reflectClaimKey(sid string, n int) string {
	return "reflector.claim." + sid + ":" + strconv.Itoa(n)
}

// runReflectDetached starts one reflect run on its own goroutine and
// reports whether it started; a second concurrent run is skipped (one
// reflector at a time). Progress reaches Lua as ctx.reflecting plus the
// reflector.start / reflector.done events (done carries error on failure).
// claim is the claim key this run will release when it ends, or "" when
// there is none to release.
func (h *Host) runReflectDetached(sid, claim string) bool {
	if !h.reflectMu.TryLock() {
		h.Log.Debug("reflector", "run skipped: another run is active", map[string]any{"session": sid})
		// this run never starts, so a just-won claim must not be left to
		// the lease: release it so a later close can reflect again
		if claim != "" {
			h.DB.DelKV(claim)
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), reflectTimeout)
	h.reflecting.Store(true)
	h.refresh()
	h.EmitAsync("reflector.start", map[string]any{"session": sid})
	h.GoSafe("reflector", func() {
		defer h.reflectMu.Unlock()
		defer cancel()
		defer h.refresh()
		defer h.reflecting.Store(false)
		if claim != "" {
			// released on success (the seen marker keeps the result) and on
			// failure (so a later run can retry); a crash leaves it to the lease
			defer h.DB.DelKV(claim)
		}
		err := h.reflectSession(ctx, sid)
		h.agents.mu.Lock()
		h.agents.reflectErr = err
		h.agents.mu.Unlock()
		if err != nil {
			h.Log.Warn("reflector", "run failed", map[string]any{"session": sid, "error": err.Error()})
			// a model that answered in prose is the /health row's news,
			// not a zemi error the badge asks the agent to fix
			if !errors.Is(err, errNoProposal) {
				h.RecordError("reflector", err.Error())
			}
			h.EmitAsync("reflector.done", map[string]any{"session": sid, "error": err.Error()})
		}
	})
	return true
}

// reflectSession runs one full reflect cycle for a session: prompt, turn,
// parse, route. Synchronous; the caller owns the timeout.
func (h *Host) reflectSession(ctx context.Context, sid string) error {
	prompt, seen, err := h.reflectPrompt(sid)
	if err != nil {
		return err
	}
	res, err := h.generate(ctx, sid, reflectorSystem, prompt, nil, "reflector.model", "models.small")
	if err != nil {
		return err
	}
	proposal, err := parseReflectProposal(concatText(res.Response.Content))
	if err != nil {
		return err
	}
	stored, queued, superseded, skipped := h.applyReflectProposal(sid, proposal)
	drafted := h.applySkillDrafts(sid, proposal)
	// what the prompt held, not the count now: the live session may have
	// grown during the model call
	_ = h.DB.SetKV(reflectSeenKey(sid), strconv.Itoa(seen))
	result := map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped, "skills_drafted": drafted,
	}
	h.Log.Info("reflector", "run complete", result)
	h.EmitAsync("reflector.done", result)
	return nil
}

// reflectPrompt builds the reflect prompt for sid and returns how many of
// its stored messages that covers.
func (h *Host) reflectPrompt(sid string) (string, int, error) {
	if h.DB == nil {
		return "", 0, errors.New("reflector: session store unavailable")
	}
	all, err := h.DB.Messages(sid)
	if err != nil {
		return "", 0, err
	}
	// a session reflected before only shows what came after that run
	msgs := all
	seen, _ := h.DB.GetKV(reflectSeenKey(sid))
	if n, err := strconv.Atoi(seen); err == nil && n <= len(msgs) {
		msgs = msgs[n:]
	}
	var lines []string
	for _, m := range msgs {
		if line := reflectLine(m); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", 0, fmt.Errorf("reflector: session %s has no messages", sid)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Project\n%s\n\n", h.Session().CWD)
	b.WriteString("# Session transcript\n")
	if len(lines) > reflectMsgsMax {
		fmt.Fprintf(&b, "... (%d earlier messages omitted)\n", len(lines)-reflectMsgsMax)
		lines = lines[len(lines)-reflectMsgsMax:]
	}
	b.WriteString(strings.Join(lines, "\n") + "\n")
	if stats, err := h.DB.ToolCallStats(sid); err == nil && len(stats) > 0 {
		b.WriteString("\n# Tool usage\n")
		for _, s := range stats {
			line := fmt.Sprintf("- %s: %d calls", s.Tool, s.Calls)
			if s.Failed > 0 {
				line += fmt.Sprintf(", %d failed", s.Failed)
			}
			b.WriteString(line + "\n")
		}
	}
	if s := h.instructionSections(h.instructionFiles(), reflectInstructionsMax); s != "" {
		b.WriteString("\n# Instruction files\nEvery session's prompt carries these already." + s + "\n")
	}
	// only this session's: memoryAuto(sid) stores what comes back, and
	// another session's suggestion may carry outside text it queued for
	props := slices.DeleteFunc(h.memoryProposals(), func(m memoryProposal) bool { return m.ProvenanceSessionID != sid })
	if len(props) > 0 {
		b.WriteString("\n# Memories awaiting review\n")
		for _, m := range props {
			verb := ""
			if m.Forget {
				verb = "forget: "
			}
			fmt.Fprintf(&b, "- [%s] %s%s\n", m.Scope, verb, truncRunes(m.Text, reflectIndexText))
		}
	}
	if mems, _ := h.memoryList(false); len(mems) > 0 {
		b.WriteString("\n# Current memory index\n")
		for _, m := range mems {
			fmt.Fprintf(&b, "- %s [%s] %s\n", m.ID[:8], m.Scope, truncRunes(m.Text, reflectIndexText))
		}
	}
	// disabled drafts count too, or the reflector drafts them again
	if all := append(h.skillsReg.list(), h.projectOnlySkills()...); len(all) > 0 {
		b.WriteString("\n# Skills\n")
		for _, s := range all {
			state := "disabled"
			if !s.Source.Stored() || h.skillsReg.isEnabled(s.Name) {
				state = "enabled"
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", s.Name, state, truncRunes(s.Description, reflectIndexText))
		}
	}
	b.WriteString("\nProduce the proposal JSON now.")
	return b.String(), len(all), nil
}

func reflectLine(m *store.Message) string {
	if m.Role != "user" && m.Role != "assistant" {
		return ""
	}
	var fm fantasy.Message
	if err := json.Unmarshal([]byte(m.PartsJSON), &fm); err != nil {
		return ""
	}
	text := strings.TrimSpace(concatText(fm.Content))
	if text == "" {
		return ""
	}
	return m.Role + ": " + truncRunes(text, reflectMsgMax)
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
	if c := learn.Truncate(s, n); c != s {
		return c + "..."
	}
	return s
}

// parseReflectProposal extracts the JSON object from the model output
// (prose and fences tolerated) and schema-decodes it.
// errNoProposal is a reflector reply that is not the JSON document asked
// for.
var errNoProposal = errors.New("returned no proposal")

func parseReflectProposal(out string) (*reflectProposal, error) {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("%w: no JSON object in the output", errNoProposal)
	}
	var p reflectProposal
	if err := json.Unmarshal([]byte(out[start:end+1]), &p); err != nil {
		return nil, fmt.Errorf("%w: %v", errNoProposal, err)
	}
	return &p, nil
}

// applyReflectProposal routes every proposal memory through the §10.4
// intake and the autonomy gate and returns counters; supersedes link when
// the memory is stored, now or on accept. The model's scope and text are intake
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
		if strings.TrimSpace(pm.Text) == "" && pm.Supersedes != "" {
			if added, err := h.queueForget(sid, pm.Supersedes); err != nil {
				skipped++
				h.Log.Debug("reflector", "retirement skipped", map[string]any{"id": pm.Supersedes, "error": err.Error()})
			} else if added {
				queued++
			}
			continue
		}
		res, err := learn.Intake(scope, learn.OriginLearned, pm.Text, "", sid)
		if err != nil {
			skipped++
			h.Log.Debug("reflector", "memory skipped by intake", map[string]any{"error": err.Error()})
			continue
		}
		res.Supersedes = pm.Supersedes
		if stored >= memoryAutoApplyCap {
			// the run carries its own budget: it stores for a closed session
			if h.queueProposal(res) {
				queued++
			}
			continue
		}
		switch out, err := h.gateMemory(res); {
		case err != nil:
			skipped++
			h.Log.Warn("reflector", "apply failed", map[string]any{"error": err.Error()})
		case out.Duplicate != "" || out.Dropped:
			skipped++
		case out.ID == "":
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
// one, in either scope; unknown ids are dropped with a log.
func (h *Host) linkSupersede(oldID string, fresh store.Memory) {
	db, old, err := h.locateMemory(oldID)
	if err != nil {
		h.Log.Debug("reflector", "supersede skipped: old memory missing", map[string]any{"id": oldID})
		return
	}
	if err := db.SupersedeMemory(old.ID, fresh.ID); err != nil {
		h.Log.Warn("reflector", "supersede failed", map[string]any{"id": oldID, "error": err.Error()})
	}
}

// reflectorCommands is /reflect.
func (h *Host) reflectorCommands() []*Command {
	return []*Command{{
		Name: "reflect", Hidden: true,
		Desc: "turn this session into memories and skills",
		GoFn: func(h *Host, args string) error { return h.reflectCommand() },
	}}
}

// reflectCommand starts a detached reflect run over the live session. Go
// commands run on the key loop, so the run must never block it.
func (h *Host) reflectCommand() error {
	sid := h.AgentSessionID()
	if sid == "" {
		return errors.New("no session to reflect on")
	}
	if !h.runReflectDetached(sid, "") {
		return errors.New("a reflect run is already active")
	}
	return nil
}
