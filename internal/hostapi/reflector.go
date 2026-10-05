// reflector.go: after a session closes, ten minutes past its last turn in
// an open zemi, or at startup once it sat idle, a tool-less model call
// reads what the session added since the last read, its failed tool calls,
// the instruction files and the whole memory index, and proposes memories
// (DESIGN §10.3). Each goes through intake and the autonomy gate like one
// the live agent writes.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
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
// reach it as the first reflectResultMax runes of the newest
// reflectFailedMax failed results. Instruction files get
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
- One declarative sentence, at most 300 characters, about the project or the user as they are now, naming the file, command or flag it is about: "the repo regenerates goldens with make golden", "the user wants commits made without being asked once checks pass". Never an instruction ("always run make golden"): an instruction is re-read as an order in a later session and overrides what the user asks then. Never a narrative of this session (what was done, changed, built, fixed, or how a bug was found) and never a description of a change ("ctrl+t, previously ctrl+shift+t"). No dates, ticket numbers or commit hashes: a memory must stand without the incident behind it.
- Worth keeping only if skipping it would cost a future session real work or a mistake: a gotcha the code does not say, with its mechanism in one clause; a command with a surprising flag; a convention the user enforces; a preference or decision the user stated. Not: anything the instruction files, recipes or project skills below say, how zemi's own tools behave, anything one file read reveals, failures caused by the environment (a missing binary, an unset credential, a fresh install), claims that a tool or feature does not work, errors a retry fixed, attempts that never worked.
- Scope "project" is this repository only: its layout, commands, conventions, decisions about it. Scope "user" holds in every project: what the user said about their habits, machine, tools, accounts or models. A fact about a repository is never "user"; a fact about the person is never "project".
- Anything about the user (a preference, a decision, a correction, a habit) carries "quote": the user's own words from this session, copied exactly, at most 160 characters. No quote, no memory: a discussion, an audit the user asked for, or your own advice is not a decision. A quote that is not in the transcript is discarded.
- A procedure is not a memory: a repository procedure is a recipe the live agent writes.
- Never secrets, credentials, tokens or key material; short identifiers at most, and an env var by name.

The index
- The index below is every memory the prompt of every session carries, as [id scope origin age], with each scope's budget use. Do not repeat one, or a memory awaiting review, in other words or from another angle. When this session refines one, replace it: set "replaces" to its id (the first field) and write the merged sentence. When the session shows one is no longer true and nothing replaces it, retire it: "replaces" with its id and "text" empty. The user reviews every retirement.
- A scope the index marks full takes no new memory unless it replaces one. To merge two lines: set replaces to one id and write the merged sentence, then retire the other with replaces and empty text.

Answer with a JSON object and nothing else, no code fences:
{"memories":[{"scope":"project","text":"...","quote":"","replaces":""}]}
When there is nothing worth keeping, answer {}.`

// reflectProposal is the schema-validated model output.
type reflectProposal struct {
	Memories []struct {
		Scope    store.Scope `json:"scope"`
		Text     string      `json:"text"`
		Quote    string      `json:"quote"`
		Replaces string      `json:"replaces"`
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
	// ReflectIdle is how long a session must have had no turn before the
	// startup sweep or an open zemi reflects it: a shorter pause is someone
	// reading, and each run pays the prompt's fixed overhead again.
	ReflectIdle = 10 * time.Minute
	// reflectSweepMax bounds the model calls one process's startup makes.
	// NOTE: one, since the sweep records every session it listed and a
	// session that grew is reflected at its close: a backlog a crash or a
	// `zemi run` left drains one session per process, deferred, never lost,
	// since the next sweep still sees it.
	reflectSweepMax = 1
	// reflectTries is how many failed runs a delta gets before it is marked
	// seen, so a provider that refuses it does not cost a call at every close.
	// NOTE: counted per session, not per delta: a delta that grows between
	// two failures is given up on after one try over its new rows.
	reflectTries = 2
)

// reflectIfDue reflects a closed or idle session when it has messages the
// reflector has not seen. The cross-process claim makes two windows on the
// same repository reflect those messages once between them, not once each,
// and this process's startup sweep keeps it to one round over the rows it
// already listed.
func (h *Host) reflectIfDue(sid string) {
	if claim := h.claimReflect(sid, h.reflectPending(sid)); claim != "" {
		h.GoSafe("reflector", func() { h.reflectClaimed(sid, claim) })
	}
}

// reflectPending is what reflectIfDue claims on: a session's message count
// when its delta is worth a call, 0 when it is not or when this process's
// startup sweep has already listed that very count.
func (h *Host) reflectPending(sid string) int {
	due, count := h.reflectDue(sid)
	if h.sweptUnchanged(sid, count) {
		return 0
	}
	return due
}

// ReflectLive reflects the live session; the TUI calls it once the session
// has had no turn for ReflectIdle, so its close finds the delta seen.
func (h *Host) ReflectLive() { h.GoSafe("reflector", func() { h.reflectIfDue(h.AgentSessionID()) }) }

// ReflectSweep runs at startup, in the background so quitting never waits
// on a model call (DESIGN §10.3): the newest due session no window has
// touched for ReflectIdle is reflected, which covers what a quit, a crash
// or a zemi run left unreflected, and every session the sweep listed is
// recorded, so this process's closes do not read those rows again.
func (h *Host) ReflectSweep() { h.reflectSweep(time.Now().Add(-ReflectIdle)) }

// reflectSweep reflects the due sessions quiet since before idle and
// records the count each listed session held, the ones it skips included.
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
		due, count := h.reflectDue(s.ID)
		h.markSwept(s.ID, count)
		if runs == reflectSweepMax || s.UpdatedAt > idle.UnixMilli() {
			continue
		}
		if claim := h.claimReflect(s.ID, due); claim != "" && h.reflectClaimed(s.ID, claim) {
			runs++
		}
	}
}

// markSwept remembers what the startup sweep saw.
func (h *Host) markSwept(sid string, count int) {
	h.sweptMu.Lock()
	defer h.sweptMu.Unlock()
	if h.swept == nil {
		h.swept = map[string]int{}
	}
	h.swept[sid] = count
}

// sweptUnchanged reports whether this process's startup sweep listed sid
// while it held this count: those rows were judged, so a close of it now
// would spend the sitting's second round on what the sweep already read.
func (h *Host) sweptUnchanged(sid string, count int) bool {
	h.sweptMu.Lock()
	defer h.sweptMu.Unlock()
	swept, listed := h.swept[sid]
	return listed && swept == count
}

// reflectDelta returns a session's messages, how many of them the
// reflector has seen (kv, so a restart does not re-reflect) and the id of
// the last seen one, which the tool-call queries start after. A seen count
// past the end (a rewind in another process) reads as none, since the
// alternative is to never reflect the session again.
func (h *Host) reflectDelta(sid string) (msgs []*store.Message, seen int, after string, err error) {
	if h.DB == nil {
		return nil, 0, "", fmt.Errorf("reflector: %w", errNoStore)
	}
	if msgs, err = h.DB.Messages(sid); err != nil {
		return nil, 0, "", err
	}
	v, _ := h.DB.GetKV(reflectSeenKey(sid))
	if seen, _ = strconv.Atoi(v); seen < 0 || seen > len(msgs) {
		seen = 0
	}
	if seen > 0 {
		after = msgs[seen-1].ID
	}
	return msgs, seen, after, nil
}

// reflectDue returns whether a session's delta is worth a model call (two
// user messages and a tool call, or one failed tool call) and its message
// count either way: one exchange that went fine teaches nothing and a
// compaction summary is not the user speaking, and the sweep records the
// count of every session it asks about.
func (h *Host) reflectDue(sid string) (due, count int) {
	if h.DB == nil {
		return 0, 0
	}
	// the sweep asks about many sessions and nearly all are seen: count
	// before loading a transcript over the store's one connection
	count, err := h.DB.MessageCount(sid)
	if seen, _ := h.DB.GetKV(reflectSeenKey(sid)); err != nil || seen == strconv.Itoa(count) {
		return 0, count
	}
	msgs, seen, after, err := h.reflectDelta(sid)
	if err != nil || seen == len(msgs) {
		return 0, count
	}
	users := 0
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
		return len(msgs), count
	}
	return 0, count
}

// claimReflect atomically claims the right to reflect sid up to its n-th
// message and returns the claim key when this process won it ("" to skip; n
// of 0 never claims). The claim is an INSERT OR IGNORE keyed by the
// session in the shared kv, so two processes that both see it as
// unreflected cannot run at once, whatever count each saw: only one
// inserts, and the other's delta waits for the next close or sweep. A
// claim its owner crashes leaving is reclaimed after reflectLease, so a
// failed run can be retried.
func (h *Host) claimReflect(sid string, n int) string {
	if n == 0 {
		return ""
	}
	key, v := reflectClaimKey(sid), strconv.Itoa(n)
	if claimed, _ := h.DB.SetKVOnce(key, v); claimed {
		return key
	}
	// a claim stands: another window is on it, or its owner crashed long
	// ago and the lease lets us take over
	if ok, _ := h.DB.ReclaimKVIfStale(key, v, time.Now().Add(-reflectLease).UnixMilli()); ok {
		return key
	}
	return ""
}

func reflectSeenKey(sid string) string  { return "reflector.seen." + sid }
func reflectTriesKey(sid string) string { return "reflector.tries." + sid }
func reflectClaimKey(sid string) string { return "reflector.claim." + sid }

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
// again (one reflector at a time). Progress reaches Lua as ctx.busy
// plus the reflector.start / reflector.done events (done carries error on
// failure; a run the user cancelled sends none). The claim is released when the run ends: on success the seen
// marker keeps the result, on failure a later run retries; a crash leaves
// it to the lease.
func (h *Host) reflectClaimed(sid, claim string) bool {
	release := func() {
		if _, err := h.DB.DelKV(claim); err != nil {
			h.Log.Warn("claim not released; its lease ends it", "origin", "reflector", "session", sid, "error", err)
		}
	}
	if !h.reflectMu.TryLock() {
		h.Log.Debug("run skipped: another run is active", "origin", "reflector", "session", sid)
		release()
		return false
	}
	defer h.reflectMu.Unlock()
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), reflectTimeout)
	defer cancel()
	defer h.Busy("Reflecting", cancel)()
	h.EmitAsync("reflector.start", map[string]any{"session": sid})
	tries, _ := h.DB.GetKV(reflectTriesKey(sid))
	try, _ := strconv.Atoi(tries)
	try++
	_ = h.DB.SetKV(reflectTriesKey(sid), strconv.Itoa(try))
	read, err := h.reflectSession(ctx, sid)
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, errRewound) {
		// the user's double esc or a rewind: no failure, so the try is given back
		_ = h.DB.SetKV(reflectTriesKey(sid), strconv.Itoa(try-1))
		return true
	}
	switch {
	case errors.Is(err, errNoProposal):
		// prose is the model's answer, not a run to repeat, and the /health
		// row's news rather than a zemi error the badge asks the agent to fix
	case err != nil && try >= reflectTries:
		if read != nil {
			h.cutMu.Lock()
			_ = h.reflectRead(sid, read)
			h.cutMu.Unlock()
		}
		err = fmt.Errorf("gave up after %d tries: %w", try, err)
		h.RecordError("reflector", err.Error())
	case err != nil:
		h.RecordError("reflector", err.Error())
	}
	h.agents.mu.Lock()
	h.agents.reflectErr = err
	h.agents.mu.Unlock()
	if err != nil {
		h.Log.Warn("run failed", "origin", "reflector", "session", sid, "error", err)
		h.EmitAsync("reflector.done", map[string]any{"session": sid, "error": err.Error()})
	}
	return true
}

// reflectSession runs one full reflect cycle for a session: prompt, turn,
// parse, route. Synchronous; the caller owns the timeout. It returns the
// rows the prompt read, nil when it read none.
func (h *Host) reflectSession(ctx context.Context, sid string) ([]*store.Message, error) {
	prompt, read, err := h.reflectPrompt(sid)
	if err != nil {
		return nil, err
	}
	res, err := h.generate(ctx, sid, reflectorSystem, prompt, nil, nil, "memory")
	if err != nil {
		return read, err
	}
	h.cutMu.Lock()
	defer h.cutMu.Unlock()
	if err := h.reflectRead(sid, read); err != nil {
		return read, err
	}
	proposal, err := parseReflectProposal(concatText(res.Response.Content))
	if err != nil {
		return read, err
	}
	stored, queued, superseded, skipped := h.applyReflectProposal(sid, proposal)
	result := map[string]any{
		"session": sid, "stored": stored, "queued": queued,
		"superseded": superseded, "skipped": skipped,
	}
	h.Log.Info("run complete", "origin", "reflector", "result", result)
	h.EmitAsync("reflector.done", result)
	return read, nil
}

// reflectRead marks sid reflected up to the rows read, what the prompt held
// rather than the count now, which the live session may have grown past. A
// rewind during the model call cut rows the reply may come from, so then it
// marks nothing (errRewound). cutMu held.
func (h *Host) reflectRead(sid string, read []*store.Message) error {
	seen := len(read)
	if rows, err := h.DB.Messages(sid); err != nil || len(rows) < seen || seen > 0 && rows[seen-1].ID != read[seen-1].ID {
		return cmp.Or(err, errRewound)
	}
	h.reflectSeen(sid, seen)
	return nil
}

// reflectPrompt builds the reflect prompt over what sid holds past the seen
// count and returns the stored messages that covers. Every transcript line
// is scrubbed before it leaves the machine.
func (h *Host) reflectPrompt(sid string) (string, []*store.Message, error) {
	all, seen, _, err := h.reflectDelta(sid)
	if err != nil {
		return "", nil, err
	}
	msgs := all[seen:]
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
	if s := h.instructionSections(h.instructionFiles(), reflectInstructionsMax); s != "" {
		b.WriteString("\n# Instruction files" + s + "\n")
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
		used := map[store.Scope]int{}
		for _, m := range mems {
			used[m.Scope] += learn.LineSize(m.Text)
		}
		fmt.Fprintf(&b, "\n# Memory index (project %s, user %s)\n",
			budgetUse(used[store.ScopeProject]), budgetUse(used[store.ScopeUser]))
		now := time.Now()
		for _, m := range mems {
			fmt.Fprintf(&b, "- [%s %s %s%s] %s\n", m.ID, m.Scope, m.Origin, learn.Age(m.UpdatedAt, now), m.Text)
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
	return b.String(), all, nil
}

// budgetUse renders a scope's budget use, marked full past 80%: the model
// is told which scope takes nothing new rather than left to do the
// arithmetic.
func budgetUse(used int) string {
	s := fmt.Sprintf("%.1f/%d KB", float64(used)/1024, learn.ScopeBudget/1024)
	if used > learn.ScopeBudget*4/5 {
		s += " (full)"
	}
	return s
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
		if m.Role == string(fantasy.MessageRoleUser) || m.Role == compactTailRole || json.Unmarshal([]byte(m.PartsJSON), &fm) != nil {
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
	if m.Role != string(fantasy.MessageRoleUser) && m.Role != string(fantasy.MessageRoleAssistant) {
		return ""
	}
	var fm fantasy.Message
	if err := json.Unmarshal([]byte(m.PartsJSON), &fm); err != nil {
		return ""
	}
	role, text := m.Role, concatText(fm.Content)
	if s, ok := summaryOf(fm); ok {
		role, text = "summary", s
	} else if role == string(fantasy.MessageRoleUser) {
		// what zemi appended is not the user's, and quoteInSession drops quotes from it
		text, _, _ = strings.Cut(text, agent.AttachMarker)
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

// truncRunes cuts text for the model or the log to n runes.
func truncRunes(s string, n int) string {
	if c := learn.Truncate(s, n); c != s {
		return c + "..."
	}
	return s
}

// clipRunes cuts text the user reads to n runes, ending in the one-cell
// ellipsis every screen cuts with.
func clipRunes(s string, n int) string {
	if c := learn.Truncate(s, n); c != s {
		return strings.TrimRight(c, " ") + "…"
	}
	return s
}

// clipLine is s on one line, its runs of space made one, cut as clipRunes cuts.
func clipLine(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), n)
}

// parseReflectProposal extracts the JSON object from the model output
// (prose and fences tolerated) and schema-decodes it.
// errNoProposal is a reflector reply that is not the JSON document asked
// for.
var errNoProposal = errors.New("returned no proposal")

// errRewound ends a run whose session a rewind cut during the model call;
// the cut's own delta is reflected later.
var errRewound = errors.New("the session was rewound during the run")

func parseReflectProposal(out string) (*reflectProposal, error) {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("%w: no JSON object in the output", errNoProposal)
	}
	var p reflectProposal
	if err := json.Unmarshal([]byte(out[start:end+1]), &p); err != nil {
		return nil, fmt.Errorf("%w: %w", errNoProposal, err)
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
			scope = store.ScopeProject
		}
		if !slices.Contains(store.Scopes, scope) {
			skipped++
			h.Log.Debug("memory skipped: bad scope", "origin", "reflector", "scope", scope)
			continue
		}
		if strings.TrimSpace(pm.Text) == "" && pm.Replaces != "" {
			if added, err := h.queueForget(sid, pm.Replaces); err != nil {
				skipped++
				h.Log.Debug("retirement skipped", "origin", "reflector", "id", pm.Replaces, "error", err)
			} else if added {
				queued++
			}
			continue
		}
		res, err := learn.Intake(scope, store.OriginLearned, pm.Text, pm.Quote, sid)
		if err != nil {
			skipped++
			h.Log.Debug("memory skipped by intake", "origin", "reflector", "error", err)
			continue
		}
		if pm.Replaces != "" {
			_, old, err := h.locateMemory(pm.Replaces)
			if err != nil {
				skipped++
				h.Log.Debug("memory skipped: replaces unknown", "origin", "reflector", "id", pm.Replaces)
				continue
			}
			res.Supersedes = old.ID
		}
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
			h.Log.Warn("apply failed", "origin", "reflector", "error", err)
		case out.Duplicate != nil || out.Dropped:
			skipped++
		case out.ID == "":
			queued++
		default:
			stored++
			if res.Supersedes != "" {
				superseded++
			}
		}
	}
	return stored, queued, superseded, skipped
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
		return Usagef("no session to reflect on")
	}
	msgs, seen, _, err := h.reflectDelta(sid)
	if err != nil {
		return err
	}
	if seen == len(msgs) {
		return Usagef("nothing new to reflect")
	}
	// the claim alone would let this run start, then skip under the lock
	// with only a log; a stale read here costs nothing but a repeat
	if !h.reflectMu.TryLock() {
		return Usagef("a reflect run is already active")
	}
	h.reflectMu.Unlock()
	claim := h.claimReflect(sid, len(msgs))
	if claim == "" {
		return Usagef("a reflect run is already active")
	}
	h.GoSafe("reflector", func() { h.reflectClaimed(sid, claim) })
	return nil
}
