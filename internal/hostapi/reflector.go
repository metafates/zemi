// reflector.go: after a session closes, or at startup once it sat idle, a
// tool-less model call reads what the session added since the last read,
// its failed tool calls, the instruction files and the whole memory index,
// and proposes memories (DESIGN §10.3). Each goes through intake and the
// autonomy gate like one the live agent writes.
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

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/store"
)

// Transcript budget for the reflect prompt: the last reflectMsgsMax user
// and assistant messages, each truncated to reflectMsgMax runes. Tool calls
// reach it as usage stats plus the first reflectResultMax runes of the
// newest reflectFailedMax failed results. Instruction files get
// reflectInstructionsMax bytes.
const (
	reflectMsgsMax         = 120
	reflectMsgMax          = 400
	reflectFailedMax       = 10
	reflectResultMax       = 200
	reflectInstructionsMax = 16 << 10
)

const reflectorSystem = `You are zemi's reflector. You read what one coding session added since it was last read and decide what a future session in this project, or any project for this user, must know that it could not re-derive in seconds. Most sessions teach nothing durable; answering with no memories is the normal result.

What a memory is
- One declarative sentence, at most 300 characters, about the project or the user as they are now: "the repo regenerates goldens with make golden", "the user wants commits made without being asked once checks pass". Never an instruction ("always run make golden"): an instruction is re-read as an order in a later session and overrides what the user asks then. Never a narrative of this session (what was done, changed, built, fixed, or how a bug was found) and never a description of a change ("ctrl+t, previously ctrl+shift+t"). No dates, ticket numbers or commit hashes: a memory must stand without the incident behind it.
- Worth keeping only if skipping it would cost a future session real work or a mistake: a gotcha the code does not say, with its mechanism in one clause; a command with a surprising flag; a convention the user enforces; a preference or decision the user stated. Not: anything the instruction files below say, anything a tool description says, anything one file read reveals, failures caused by the environment (a missing binary, an unset credential, a fresh install), claims that a tool or feature does not work, errors a retry fixed, attempts that never worked.
- Scope "project" is this repository only: its layout, commands, conventions, decisions about it. Scope "user" holds in every project: the user's habits, machine, tools, accounts, models. A fact about a repository is never "user"; a fact about the person is never "project".
- Anything about the user (a preference, a decision, a correction, a habit) carries "quote": the user's own words from this session, copied exactly, at most 160 characters. No quote, no memory: a discussion, an audit the user asked for, or your own advice is not a decision. A quote that is not in the transcript is discarded.
- A procedure is not a memory: a repository procedure is a recipe the live agent writes.
- Never secrets, credentials, tokens or key material; short identifiers at most, and an env var by name.

The index
- The index below is every memory the prompt of every session carries, with each scope's budget use. Do not repeat one, in other words or from another angle. When this session refines one, replace it: set "replaces" to its id and write the merged sentence. When the session shows one is no longer true and nothing replaces it, retire it: "replaces" with its id and "text" empty. The user reviews every retirement.
- A scope over 80% of its budget takes no new memory unless it replaces one; merge the overlapping ones into a single sentence that replaces them, or retire the weakest.

Answer with ONLY a JSON object, no prose and no code fences:
{"memories":[{"scope":"project","text":"...","quote":"","replaces":""}]}
Omit "memories" when there is nothing worth keeping.`

// reflectProposal is the schema-validated model output.
type reflectProposal struct {
	Memories []struct {
		Scope    string `json:"scope"`
		Text     string `json:"text"`
		Quote    string `json:"quote"`
		Replaces string `json:"replaces"`
	} `json:"memories"`
}

const (
	reflectTimeout = 2 * time.Minute
	// reflectLease is how long a reflection claim may stand without its
	// owner: a run ends at reflectTimeout, so an older claim belongs to a
	// process that died or was paused, and another run may take it over.
	// NOTE: a paused process that resumes applies its proposals too; the
	// near-duplicate check turns the repeat into a skip, not a second row.
	reflectLease = reflectTimeout + time.Minute
	// reflectIdle is how long a session must have been quiet before the
	// startup sweep reflects it: another window may still be typing in it.
	reflectIdle = 10 * time.Minute
	// reflectSweepMax bounds the model calls one startup makes.
	reflectSweepMax = 3
	// reflectTries is how many failed runs a delta gets before it is marked
	// seen, so a provider that refuses it does not cost a call at every close.
	// NOTE: counted per session, not per delta: a delta that grows between
	// two failures is given up on after one try over its new rows.
	reflectTries = 2
)

// reflectOnClose is the session-close trigger (/new, /resume): the closed
// session is reflected when it has messages the reflector has not seen.
// The cross-process claim makes two windows on the same repository reflect
// those messages once between them, not once each.
func (h *Host) reflectOnClose(prevID string) {
	if n, claim := h.claimReflect(prevID, h.reflectDue(prevID)); claim != "" {
		h.GoSafe("reflector", func() { h.reflectClaimed(prevID, claim, n) })
	}
}

// ReflectSweep runs at startup, in the background so quitting never waits
// on a model call (DESIGN §10.3): the newest due sessions that no window
// has touched for reflectIdle are reflected in turn, which covers what a
// quit, a crash or a -p run left unreflected.
func (h *Host) ReflectSweep() { h.reflectSweep(time.Now().Add(-reflectIdle)) }

// reflectSweep reflects the due sessions quiet since before idle.
func (h *Host) reflectSweep(idle time.Time) {
	if h.DB == nil {
		return
	}
	list, err := h.DB.ListSessions(20)
	if err != nil {
		return
	}
	runs := 0
	for _, s := range list {
		if runs == reflectSweepMax {
			return
		}
		if s.UpdatedAt > idle.UnixMilli() {
			continue
		}
		if n, claim := h.claimReflect(s.ID, h.reflectDue(s.ID)); claim != "" && h.reflectClaimed(s.ID, claim, n) {
			runs++
		}
	}
}

// reflectDelta returns a session's messages and how many of them the
// reflector has seen (kv, so a restart does not re-reflect). A seen count
// past the end (a rewind in another process) reads as none, since the
// alternative is to never reflect the session again.
func (h *Host) reflectDelta(sid string) ([]*store.Message, int, error) {
	if h.DB == nil {
		return nil, 0, errors.New("reflector: session store unavailable")
	}
	msgs, err := h.DB.Messages(sid)
	if err != nil {
		return nil, 0, err
	}
	seen, _ := h.DB.GetKV(reflectSeenKey(sid))
	n, _ := strconv.Atoi(seen)
	if n < 0 || n > len(msgs) {
		n = 0
	}
	return msgs, n, nil
}

// reflectDue returns a session's message count when what came after the
// seen count is worth a model call, 0 otherwise: two user messages and a
// tool call, or one failed tool call. One exchange that went fine teaches
// nothing, and a compaction summary is not the user speaking.
func (h *Host) reflectDue(sid string) int {
	if h.DB == nil {
		return 0
	}
	// the sweep asks about many sessions and nearly all are seen: count
	// before loading a transcript over the store's one connection
	count, err := h.DB.MessageCount(sid)
	if seen, _ := h.DB.GetKV(reflectSeenKey(sid)); err != nil || seen == strconv.Itoa(count) {
		return 0
	}
	msgs, seen, err := h.reflectDelta(sid)
	if err != nil || seen == len(msgs) {
		return 0
	}
	users, after := 0, ""
	if seen > 0 {
		after = msgs[seen-1].ID
	}
	for _, m := range msgs[seen:] {
		if _, ok := userText(m); ok {
			users++
		}
	}
	stats, _ := h.DB.ToolCallStats(sid, after)
	calls, failed := 0, 0
	for _, s := range stats {
		calls, failed = calls+s.Calls, failed+s.Failed
	}
	if failed > 0 || (users >= 2 && calls > 0) {
		return len(msgs)
	}
	return 0
}

// claimReflect atomically claims the right to reflect sid up to its n-th
// message and returns n with the claim key when this process won it ("" to
// skip; n of 0 never claims). The claim is an INSERT OR IGNORE keyed by the
// message count in the shared kv, so two processes that both see the
// session as unreflected can still not reflect it twice: only one inserts.
// A claim its owner crashes leaving is reclaimed after reflectLease, so a
// failed run can be retried.
func (h *Host) claimReflect(sid string, n int) (int, string) {
	if n == 0 {
		return 0, ""
	}
	key := reflectClaimKey(sid, n)
	if claimed, _ := h.DB.SetKVOnce(key, "1"); claimed {
		return n, key
	}
	// a claim for this count stands: another window is on it, or its owner
	// crashed long ago and the lease lets us take over
	if ok, _ := h.DB.ReclaimKVIfStale(key, "1", time.Now().Add(-reflectLease).UnixMilli()); ok {
		return n, key
	}
	return 0, ""
}

func reflectSeenKey(sid string) string  { return "reflector.seen." + sid }
func reflectTriesKey(sid string) string { return "reflector.tries." + sid }
func reflectClaimKey(sid string, n int) string {
	return "reflector.claim." + sid + ":" + strconv.Itoa(n)
}

// reflectSeen marks sid reflected up to n messages and forgets its failed
// tries, so the next delta gets its own.
func (h *Host) reflectSeen(sid string, n int) {
	_ = h.DB.SetKV(reflectSeenKey(sid), strconv.Itoa(n))
	_, _ = h.DB.DelKV(reflectTriesKey(sid))
}

// clampReflectSeen keeps the seen count within a session a rewind cut, so
// what comes after the cut is a delta of its own.
func (h *Host) clampReflectSeen(sid string, n int) {
	if seen, _ := h.DB.GetKV(reflectSeenKey(sid)); seen != "" {
		if s, _ := strconv.Atoi(seen); s > n {
			_ = h.DB.SetKV(reflectSeenKey(sid), strconv.Itoa(n))
		}
	}
}

// reflectClaimed runs one reflect run over the claim's session, synchronous
// on the caller's goroutine, and reports whether it ran: a run while another
// is active is skipped and its claim released, so a later close can reflect
// again (one reflector at a time). Progress reaches Lua as ctx.reflecting
// plus the reflector.start / reflector.done events (done carries error on
// failure). The claim is released when the run ends: on success the seen
// marker keeps the result, on failure a later run retries; a crash leaves
// it to the lease. n is the message count the claim covers.
func (h *Host) reflectClaimed(sid, claim string, n int) bool {
	if !h.reflectMu.TryLock() {
		h.Log.Debug("reflector", "run skipped: another run is active", map[string]any{"session": sid})
		h.DB.DelKV(claim)
		return false
	}
	defer h.reflectMu.Unlock()
	defer h.DB.DelKV(claim)
	ctx, cancel := context.WithTimeout(context.Background(), reflectTimeout)
	defer cancel()
	h.reflecting.Store(true)
	h.refresh()
	defer h.refresh()
	defer h.reflecting.Store(false)
	h.EmitAsync("reflector.start", map[string]any{"session": sid})
	tries, _ := h.DB.GetKV(reflectTriesKey(sid))
	try, _ := strconv.Atoi(tries)
	try++
	_ = h.DB.SetKV(reflectTriesKey(sid), strconv.Itoa(try))
	err := h.reflectSession(ctx, sid)
	switch {
	case errors.Is(err, errNoProposal):
		// prose is the model's answer, not a run to repeat, and the /health
		// row's news rather than a zemi error the badge asks the agent to fix
		h.reflectSeen(sid, n)
	case err != nil && try >= reflectTries:
		h.reflectSeen(sid, n)
		err = fmt.Errorf("gave up after %d tries: %w", try, err)
		h.RecordError("reflector", err.Error())
	case err != nil:
		h.RecordError("reflector", err.Error())
	}
	h.agents.mu.Lock()
	h.agents.reflectErr = err
	h.agents.mu.Unlock()
	if err != nil {
		h.Log.Warn("reflector", "run failed", map[string]any{"session": sid, "error": err.Error()})
		h.EmitAsync("reflector.done", map[string]any{"session": sid, "error": err.Error()})
	}
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
	// what the prompt held, not the count now: the live session may have
	// grown during the model call
	h.reflectSeen(sid, seen)
	result := map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped,
	}
	h.Log.Info("reflector", "run complete", result)
	h.EmitAsync("reflector.done", result)
	return nil
}

// reflectPrompt builds the reflect prompt over what sid holds past the seen
// count and returns how many of its stored messages that covers. Every
// transcript line is scrubbed before it leaves the machine.
func (h *Host) reflectPrompt(sid string) (string, int, error) {
	all, seen, err := h.reflectDelta(sid)
	if err != nil {
		return "", 0, err
	}
	msgs, after := all[seen:], ""
	if seen > 0 {
		after = all[seen-1].ID
	}
	var lines []string
	for _, m := range msgs {
		if line := reflectLine(m); line != "" {
			lines = append(lines, line)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Project\n%s\n\n", h.Session().CWD)
	b.WriteString("# Session transcript\n")
	if len(lines) > reflectMsgsMax {
		fmt.Fprintf(&b, "... (%d earlier messages omitted)\n", len(lines)-reflectMsgsMax)
		lines = lines[len(lines)-reflectMsgsMax:]
	}
	b.WriteString(strings.Join(lines, "\n") + "\n")
	if failed := failedCalls(msgs); len(failed) > 0 {
		b.WriteString("\n# Failed tool calls\n")
		for _, f := range failed[max(0, len(failed)-reflectFailedMax):] {
			b.WriteString("- " + f + "\n")
		}
	}
	if stats, err := h.DB.ToolCallStats(sid, after); err == nil && len(stats) > 0 {
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
			fmt.Fprintf(&b, "- [%s] %s%s\n", m.Scope, verb, m.Text)
		}
	}
	if mems, _ := h.memoryList(false); len(mems) > 0 {
		used := map[string]int{}
		for _, m := range mems {
			used[m.Scope] += learn.LineSize(m.Text)
		}
		fmt.Fprintf(&b, "\n# Memory index (every memory the prompt carries; project %.1f/%d KB, user %.1f/%d KB)\n",
			float64(used["project"])/1024, learn.ScopeBudget/1024, float64(used["user"])/1024, learn.ScopeBudget/1024)
		now := time.Now()
		for _, m := range mems {
			fmt.Fprintf(&b, "- [%s %s %s%s] %s\n", m.ID[:8], m.Scope, m.Origin, learn.Age(m.UpdatedAt, now), m.Text)
			if m.Quote != "" {
				fmt.Fprintf(&b, "  quote: %q\n", m.Quote)
			}
		}
	}
	if own := h.projectOnlySkills(); len(own) > 0 {
		b.WriteString("\n# Recipes and project skills\n")
		for _, s := range own {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}
	b.WriteString("\nProduce the JSON now.")
	return b.String(), len(all), nil
}

// failedCalls lists the failed tool results among msgs, oldest first, as
// "<tool>: <first line>". The telemetry rows hold no text, so it reads the
// history rows the turn persisted, naming the tool from its call part; a
// compaction's tail rows are copies of rows still in the table.
func failedCalls(msgs []*store.Message) []string {
	names := map[string]string{}
	var out []string
	for _, m := range msgs {
		var fm fantasy.Message
		if m.Role == "user" || m.Role == compactTailRole || json.Unmarshal([]byte(m.PartsJSON), &fm) != nil {
			continue
		}
		for _, part := range fm.Content {
			switch p := part.(type) {
			case fantasy.ToolCallPart:
				names[p.ToolCallID] = p.ToolName
			case fantasy.ToolResultPart:
				if p.Output != nil && p.Output.GetType() == fantasy.ToolResultContentTypeError {
					line, _, _ := strings.Cut(strings.TrimSpace(agent.ToolOutput(p.Output)), "\n")
					out = append(out, names[p.ToolCallID]+": "+truncRunes(learn.ScrubShapes(line), reflectResultMax))
				}
			}
		}
	}
	return out
}

func reflectLine(m *store.Message) string {
	if m.Role != "user" && m.Role != "assistant" {
		return ""
	}
	var fm fantasy.Message
	if err := json.Unmarshal([]byte(m.PartsJSON), &fm); err != nil {
		return ""
	}
	role, text := m.Role, concatText(fm.Content)
	if s, ok := summaryOf(fm); ok {
		role, text = "summary", s
	}
	if text = strings.TrimSpace(text); text == "" {
		return ""
	}
	return role + ": " + truncRunes(learn.ScrubShapes(text), reflectMsgMax)
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
// intake and the autonomy gate and returns counters; a replaced memory is
// superseded when the new one is stored, now or on accept. The model's
// scope, text and quote are intake inputs, re-validated and scrubbed there;
// a bad entry, a near-duplicate or a quote the user never wrote is skipped,
// never fatal.
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
		if strings.TrimSpace(pm.Text) == "" && pm.Replaces != "" {
			if added, err := h.queueForget(sid, pm.Replaces); err != nil {
				skipped++
				h.Log.Debug("reflector", "retirement skipped", map[string]any{"id": pm.Replaces, "error": err.Error()})
			} else if added {
				queued++
			}
			continue
		}
		res, err := learn.Intake(scope, learn.OriginLearned, pm.Text, pm.Quote, sid)
		if err != nil {
			skipped++
			h.Log.Debug("reflector", "memory skipped by intake", map[string]any{"error": err.Error()})
			continue
		}
		res.Supersedes = pm.Replaces
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
			if _, old, err := h.locateMemory(pm.Replaces); pm.Replaces != "" && err == nil && old.SupersededBy == res.Memory.ID {
				superseded++
			}
		}
	}
	return stored, queued, superseded, skipped
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
		Desc: "turn this session into memories",
		GoFn: func(h *Host, args string) error { return h.reflectCommand() },
	}}
}

// reflectCommand starts a detached reflect run over the live session at
// its current count, floor or not. Go commands run on the key loop, so the
// run must never block it.
func (h *Host) reflectCommand() error {
	sid := h.AgentSessionID()
	if sid == "" {
		return errors.New("no session to reflect on")
	}
	msgs, seen, err := h.reflectDelta(sid)
	if err != nil {
		return err
	}
	if seen == len(msgs) {
		return errors.New("nothing new to reflect")
	}
	// the claim alone would let this run start, then skip under the lock
	// with only a log; a stale read here costs nothing but a repeat
	if h.reflecting.Load() {
		return errors.New("a reflect run is already active")
	}
	n, claim := h.claimReflect(sid, len(msgs))
	if claim == "" {
		return errors.New("a reflect run is already active")
	}
	h.GoSafe("reflector", func() { h.reflectClaimed(sid, claim, n) })
	return nil
}
