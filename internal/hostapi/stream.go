package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// The stream adapter: agent.StreamHooks into UI events and stream state, and
// the snapshot the widgets read while a turn runs.

// streamHooks adapts agent.StreamHooks into UI events and stream state.
func (h *Host) streamHooks() agent.StreamHooks {
	return &streamAdapter{h: h}
}

type streamAdapter struct{ h *Host }

func (s *streamAdapter) TextDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	ag.retryAt = time.Time{}
	ag.textBuf += text
	ag.mu.Unlock()
	s.h.agentStream()
	// NOTE: one queue round trip per delta; fine for the few configs that
	// listen, coalesce here if one ever streams into something slow.
	s.h.EmitAsync(EventAgentTextDelta, map[string]any{"text": text})
}

func (s *streamAdapter) StepStart() {
	rows := len(s.h.Session().Transcript)
	ag := s.h.agents
	ag.mu.Lock()
	// capped, so a pending row appended later cannot write into the saved slice
	ag.stepText, ag.stepReason, ag.stepTools, ag.stepRows, ag.stepThought, ag.stepTurnThought = ag.textBuf, ag.reasonBuf, slices.Clip(ag.tools), rows, ag.thought, ag.turnThought
	ag.stretchMS = nil
	ag.stepThinking = 0
	// this step's rows are unlogged until its OnStepFinish; a step that only
	// streams text never opens a call, so openToolRow would never re-arm this
	ag.stepSettled = false
	ag.mu.Unlock()
}

// Retrying also takes back what a call opened in the failed attempt moved
// into the transcript (openToolRow), or its text shows twice above a failed row.
func (s *streamAdapter) Retrying(err error, delay time.Duration) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.textBuf, ag.reasonBuf, ag.tools, ag.thought, ag.turnThought, ag.thinking, ag.stepThinking = ag.stepText, ag.stepReason, ag.stepTools, ag.stepThought, ag.stepTurnThought, false, 0
	ag.stretchMS = nil // the failed attempt's stretches re-stream and queue again
	rows := ag.stepRows
	ag.retries++
	ag.retryAt = s.h.now().Add(delay)
	ag.retryWhy, _ = s.h.explain(err) // the advice is to wait, which the countdown says
	ag.mu.Unlock()
	s.h.cutAssistantRows(rows)
	s.h.Log.Warn("provider retry", "origin", "agent", "error", errString(err), "delay_ms", delay.Milliseconds())
	s.h.agentStream()
}

func (s *streamAdapter) ReasoningDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	// a new step thinks: what the last one showed goes above it. Reasoning
	// after text with no call between, or while the step's calls are still
	// open, is a late chunk of the same thought (providers send the last
	// token after the reply or the call starts), so it joins the block above
	// and neither the reply nor the thought is cut. Only a finished call
	// closes a thought: a step cannot end while one of its calls runs.
	// NOTE: reasoning a provider interleaves after a call still open in the
	// same step draws above that call live and below it on resume; an
	// ordered live list would fix it.
	if slices.ContainsFunc(ag.tools, func(t StreamTool) bool { return t.Status != CallRun && t.Status != CallWait }) {
		s.h.flushOutput(TurnStat{})
	}
	// reasonBuf first: with a thought open the chunk is its own, not a late one
	if ag.reasonBuf != "" || !s.h.joinReasoning(text) {
		ag.beginThinking()
		ag.reasonBuf += text
	}
	ag.retryAt = time.Time{}
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.EmitAsync(EventAgentReasoningDelta, map[string]any{"text": text})
}

// ToolCallPending shows the call while the model is still streaming its
// arguments: a write's file body or a long heredoc takes seconds that a
// spinner alone says nothing about.
func (s *streamAdapter) ToolCallPending(id, name string) {
	s.openToolRow(StreamTool{ID: id, Name: name, Status: CallWait})
}

func (s *streamAdapter) ToolCallStart(id, name, input string) {
	s.h.Log.Debug("tool call start", "origin", "agent", "tool", name, "id", id)
	row := StreamTool{ID: id, Name: name, Input: toolSummary(name, input), Raw: input, Label: toolLabel(input), Status: CallWait, Diff: inputDiff(name, input)}
	ag := s.h.agents
	ag.mu.Lock()
	i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id })
	if i < 0 {
		ag.mu.Unlock()
		s.openToolRow(row) // this provider reports no input start
		return
	}
	ag.tools[i] = row
	ag.mu.Unlock()
	s.h.agentStream()
}

// ToolCallRunning starts a row's clock: until then it draws no timer, so a
// call queued behind a serial one does not count time it has not run.
func (s *streamAdapter) ToolCallRunning(id string) {
	ag := s.h.agents
	ag.mu.Lock()
	if i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id && t.Status == CallWait }); i >= 0 {
		ag.tools[i].Status, ag.tools[i].Started = CallRun, s.h.now()
	}
	ag.mu.Unlock()
	s.h.agentStream()
}

// openToolRow appends a live tool row. Text streamed before the call
// belongs above it: it and the earlier (finished) tool rows move into the
// transcript, in order, so the next step's text starts its own block
// instead of running on.
func (s *streamAdapter) openToolRow(row StreamTool) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	ag.retryAt = time.Time{}
	// the thought the call closes belongs above it, with the call rather than
	// later with the run's digest
	s.h.flushOutput(TurnStat{})
	ag.tools = append(ag.tools, row)
	ag.stepSettled = false // the call is not in the log until its step finishes
	ag.mu.Unlock()
	s.h.agentStream()
}

func (s *streamAdapter) ToolCallEnd(id, name string, result fantasy.ToolResultOutputContent, changed string) {
	ag := s.h.agents
	turn := s.h.turn.Load()
	ok, output := !agent.ErrResult(result), agent.ToolOutput(result)
	o := s.h.takeToolOutcome(id)
	row := StreamTool{ID: id, Name: name}
	ag.mu.Lock()
	for i := len(ag.tools) - 1; i >= 0; i-- {
		t := &ag.tools[i]
		if t.ID == id && (t.Status == CallRun || t.Status == CallWait) {
			t.Status = toolStatus(result, turn != nil && turn.ctx.Err() != nil, o)
			t.Output, t.Job = rowOutput(name, output), o.Job
			if o.Run != "" {
				t.Run = o.Run
			}
			if !ok {
				t.Diff = "" // the change did not happen
			}
			if changed != "" {
				t.Diff = changed // shell: what the command changed, failed or not
			}
			if name == string(tools.ToolShell) {
				t.Detail, t.Tail = "", "" // its last line and tail, which the result now holds
			}
			row = *t
			break
		}
	}
	ag.mu.Unlock()
	s.h.setCallRow(id, rowMeta{Status: row.Status, Run: row.Run, Job: row.Job})
	s.h.agentStream()
	s.h.recordToolCall(s.h.AgentSessionID(), row, ok)
	if !ok {
		s.h.Log.Warn("tool call failed", "origin", "agent", "tool", name, "id", id,
			"status", string(row.Status), "error", clipLine(output, 200))
	} else {
		var ms int64
		if !row.Started.IsZero() {
			ms = s.h.now().Sub(row.Started).Milliseconds()
		}
		s.h.Log.Info("tool call", "origin", "agent", "tool", name, "id", id,
			"status", string(row.Status), "ms", ms)
	}
	if name == string(tools.ToolTodos) {
		ag.mu.Lock()
		ag.todoTouched = true
		ag.mu.Unlock()
	}
}

// StreamDone bills a model call as its stream finishes, so the token count
// moves during the turn instead of only at TurnDone. The conversation bills
// to the "main" anchor row; a subagent run keeps its own rows.
func (s *streamAdapter) StreamDone(u fantasy.Usage) {
	ag := s.h.agents
	in := billedInput(u)
	ag.mu.Lock()
	model := s.h.turnModelID()
	// the context the latest step carried: the statusline's ctx meter reads it
	ag.lastIn = in
	ag.mu.Unlock()
	s.h.bill(store.MainSessionID, model, u)
	// the resume's meter reads it back instead of estimating
	if db := s.h.DB; db != nil {
		if err := db.SetSessionLastIn(store.MainSessionID, in); err != nil {
			s.h.Log.Warn("usage not logged", "origin", "model", "error", err)
		}
	}
}

// StepDone counts the step for the todo nag unless a todos call ran in it.
func (s *streamAdapter) StepDone(st agent.Step) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	model, touched := h.turnModelID(), ag.todoTouched
	ag.todoTouched = false
	ag.stepSettled = true // the step's calls and results reached the log
	for i := range ag.tools {
		ag.tools[i].logged = true
	}
	ag.mu.Unlock()
	h.emitStep(store.MainSessionID, model, st)
	if !touched {
		h.maybeNagTodos()
	}
}

func (s *streamAdapter) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	// a held error is resent without what the provider refused; the turn's
	// finish, the notification and the error row wait for the resend
	hold := ag.holdImages && imagesRefused(err) || ag.holdCap && maxTokensRefused(err) || ag.holdOverflow && contextTooLarge(err)
	ag.holdOverflow, ag.holdImages, ag.holdCap = false, false, false
	now := h.now()
	model := h.turnModelID()
	text, live := ag.textBuf, ag.tools
	// the reply row's stat carries the turn's whole thought: the live digest
	// reads it the way the rebuilt window reads the log's row
	stat := TurnStat{In: usage.InputTokens, Out: usage.OutputTokens, Ended: now.Unix(), Model: model, ThoughtMS: (ag.turnThought + ag.thought).Milliseconds()}
	if !ag.turnStart.IsZero() {
		stat.ElapsedMS = now.Sub(ag.turnStart).Milliseconds()
	}
	ag.turnStart = time.Time{}
	// the retry state is the turn's too: a turn that ended during a backoff
	// (esc, or the last retry failing) reports no retry while idle
	ag.retries, ag.retryAt, ag.retryWhy = 0, time.Time{}, ""
	// a call still open here was cut off: by the user, or by the error that
	// ended the turn
	var cut []StreamTool
	for i := range ag.tools {
		t := &ag.tools[i]
		open := t.Status == CallRun || t.Status == CallWait
		if open {
			t.Status, t.Diff = map[bool]CallStatus{true: CallStopped, false: CallErr}[errors.Is(err, context.Canceled)], ""
		}
		// the pair a settled step logged is already in the log; the cut path
		// logs only what no StepDone ever wrote, and says how the call ended
		if open || !ag.stepSettled && !t.logged {
			cut = append(cut, *t)
		}
	}
	// the last step's calls reach the log only when its OnStepFinish runs; a
	// step that never finished (esc mid-command: the tool returned its stopped
	// result before the step died) leaves its whole rail unlogged. The same
	// holds for the reasoning it drew and the reply it streamed. Log the rows
	// here, so a resumed window
	// draws what the live one drew; the rows land before the flush, so the
	// pairing pairs them at once. A retry re-streams the step and logs its own.
	if !hold && h.chat != nil {
		if !ag.stepSettled {
			// the stretches the step already flushed draw unnamed in the
			// transcript - reasoning and reply text alike; the open ones are
			// still in the buffers. A late stretch a provider interleaved
			// after a call logs below that call here, the way a rebuild
			// draws it (see the NOTE on ReasoningDelta)
			h.tMu.Lock()
			tr := h.session.Transcript
			for _, m := range tr[min(ag.stepRows, len(tr)):] {
				// this step's rows only: reasoning rows never stamp, so an
				// unbounded scan re-logs every earlier turn's thoughts; and a
				// queued user row draws unnamed too, but is not ours to log
				if m.Seq != NoSeq || m.Role != fantasy.MessageRoleAssistant {
					continue
				}
				switch m.Kind {
				case RowReasoning:
					if e := h.chat.Thought(m.Text, m.Stat.ThoughtMS); e != nil {
						h.Log.Warn("thought not logged", "origin", "agent", "error", e)
					}
				case "":
					if e := h.chat.Agent(m.Text); e != nil {
						h.Log.Warn("reply not logged", "origin", "agent", "error", e)
					}
				}
			}
			h.tMu.Unlock()
			ag.endThinking()
			if ms := ag.thought.Milliseconds(); strings.TrimSpace(ag.reasonBuf) != "" {
				if e := h.chat.Thought(ag.reasonBuf, ms); e != nil {
					h.Log.Warn("thought not logged", "origin", "agent", "error", e)
				}
			}
		}
		// NOTE: a parallel call that settled before a later call opened was
		// flushed off ag.tools already, and its transcript row holds only the
		// digested input, which cannot rebuild the label - that row's rail is
		// the one draw a cut turn can still lose
		for _, t := range cut {
			if e := h.chat.Tool(string(tools.ToolName(t.Name)), cmp.Or(t.Raw, t.Input), t.ID); e != nil {
				h.Log.Warn("call not logged", "origin", "agent", "error", e)
			} else if e := h.chat.Echo(t.Name, t.Status == CallOK, t.Output, t); e != nil {
				h.Log.Warn("call result not logged", "origin", "agent", "error", e)
			}
		}
		// the reply streamed so far draws live and dies with the turn: log
		// it, or a resumed window loses the line the user read
		if !ag.stepSettled && strings.TrimSpace(ag.textBuf) != "" {
			if e := h.chat.Agent(ag.textBuf); e != nil {
				h.Log.Warn("reply not logged", "origin", "agent", "error", e)
			}
		}
	}
	n := h.flushOutput(stat)
	// a dead turn's flush never reaches the log: the interrupted step's
	// OnStepFinish is gone, so no log row will name these - the pairing
	// cursor consumes them here, or it sits on the orphan and every later
	// row of the window stays unnamed (zoom's target lost) until a reset
	if err != nil && !hold && n > 0 {
		h.tMu.Lock()
		if at := len(h.session.Transcript) - n; at >= h.stampRow {
			// earlier steps' rows are written by the time their step ends,
			// so the cursor rides just behind the tail it must jump past
			h.stampRow = len(h.session.Transcript)
		}
		h.tMu.Unlock()
	}
	thought := ag.turnThought
	ag.mu.Unlock()
	h.refresh()
	if hold {
		return
	}
	// the readout rides the log's last reply row too: a window rebuilt from
	// the log draws it from there, and each stretch's thought from its own row
	if text != "" && h.chat != nil {
		if err := h.chat.setReplyStat(stat, thought); err != nil {
			h.Log.Warn("turn stat not logged", "origin", "agent", "error", err)
		}
	}
	cost := h.turnCost(model, usage)
	switch {
	case errors.Is(err, context.Canceled):
		h.Log.Info("turn cancelled", "origin", "agent", "model", model,
			"in", billedInput(usage), "out", usage.OutputTokens, "ms", stat.ElapsedMS)
	case err != nil:
		h.Log.Warn("turn failed", "origin", "agent", "model", model,
			"in", billedInput(usage), "out", usage.OutputTokens, "ms", stat.ElapsedMS, "error", errString(err))
	default:
		h.Log.Info("turn done", "origin", "agent", "model", model,
			"in", billedInput(usage), "out", usage.OutputTokens, "cost", cost, "ms", stat.ElapsedMS)
	}
	ui := h.hooks()
	if ui != nil {
		stopped := errors.Is(err, context.Canceled) || text == "" && slices.ContainsFunc(live, func(t StreamTool) bool { return t.Status == CallStopped })
		ui.TurnDone(h.friendlyError(err), stopped)
	}
	if err != nil {
		h.EmitAsync(EventAgentError, map[string]any{"error": errString(err)})
		// The failure stays in the transcript: a toast expires, and a turn
		// that ended silently looks like zemi ignored the message.
		row := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: clipRunes(sanitize.String(h.errorRowText(err)), 400)}
		if errors.Is(err, context.Canceled) {
			row.Status = CallCancelled
			// a call the interrupt cut off is logged with its result above,
			// so a resumed window draws the rail it drew live
			for _, t := range live {
				if t.Status != CallStopped {
					continue
				}
				name := string(tools.ToolName(t.Name))
				if t.Label != "" {
					// the label reads as what the call was, not its name:
					// "while the shell call "second" ran", not "shell: second"
					row.Text = clipRunes(row.Text+"\nwhile the "+name+" call \""+t.Label+"\" ran", 400)
					continue
				}
				row.Text = clipRunes(row.Text+"\nwhile the "+name+" call ran", 400)
			}
		}
		if h.chat != nil {
			if err := h.chat.Error(row.Text, row.Status == CallCancelled); err != nil {
				h.logAppendFailed(err)
			}
		}
		h.AppendChat(row)
	} else {
		payload := map[string]any{"session": store.MainSessionID, "model": model, "usage": map[string]any{
			"in": billedInput(usage), "out": usage.OutputTokens, "total": billedInput(usage) + usage.OutputTokens,
			"cost": cost,
		}}
		h.EmitAsync(EventAgentFinish, payload)
		h.consultHooks(EventAgentFinish, nil)
	}
}

// ToolInputDelta labels a pending row as soon as "about" has streamed: a
// long command or heredoc takes seconds to arrive after it.
func (s *streamAdapter) ToolInputDelta(id, delta string) {
	ag := s.h.agents
	ag.mu.Lock()
	i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id })
	if i < 0 || ag.tools[i].Label != "" || len(ag.tools[i].partial) > 1024 {
		ag.mu.Unlock()
		return
	}
	t := &ag.tools[i]
	t.partial += delta
	var label string
	if m := aboutArg.FindStringSubmatch(t.partial); m != nil && json.Unmarshal([]byte(m[1]), &label) == nil {
		t.Label = clipLine(label, 80)
	}
	labelled := t.Label != ""
	ag.mu.Unlock()
	if labelled {
		s.h.agentStream()
	}
}

// StreamSnapshot is the live stream state for widgets.
type StreamSnapshot struct {
	Active    bool
	Text      string
	Reasoning string
	Thinking  bool // a reasoning block is open: the model thinks right now
	Tools     []StreamTool
	Model     string
	Steering  []Queued      // what waits for the turn's next step boundary, oldest first
	Thought   time.Duration // wall time spent in reasoning blocks
	Elapsed   time.Duration // whole-turn wall time (0 outside a turn)
	Retrying  bool          // a provider retry waits or is under way
	RetryIn   time.Duration // time until the next provider retry, 0 once it is under way
	Retries   int           // provider retries this turn
	RetryWhy  string        // what failed, for the retry countdown
	Turn      int           // the turn these counts belong to (agentHost.turns)
	// the goal is being checked after a turn (/goal); the agent stays active
	CheckingGoal bool
	// the running or last chain of turns began with a loop run, which no one
	// waits on: its end arms no recap and sends no alert
	ByLoop bool

	// usage tally (DESIGN §8 statusline): session totals, the last prompt
	// size (the current-context estimate), and the context window used for
	// the percent readout.
	SessionIn   int64
	SessionOut  int64
	LastIn      int64
	CtxWindow   int64
	SessionCost float64
	// prompt cache tokens read, over the input billed alongside them: the
	// session's own, restored with its stored rows so a resume shows the same
	// share
	CacheRead   int64
	CacheBilled int64
}

func (h *Host) AgentStream() StreamSnapshot {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.streamLocked()
}

// liveView is the transcript and the live turn read together, as
// flushOutput changes them.
func (h *Host) liveView() (Session, StreamSnapshot) {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.Session(), h.streamLocked()
}

// streamLocked is AgentStream with ag.mu held.
func (h *Host) streamLocked() StreamSnapshot {
	ag := h.agents
	// the host's clock, never time.Now: a seeded stream, a golden included,
	// must read the same seconds however loaded the machine is
	now := h.now()
	thought := ag.thought
	if ag.thinking {
		thought += now.Sub(ag.thinkStart)
	}
	// milliseconds, as the settled row stores it, so both read alike
	thought = thought.Truncate(time.Millisecond)
	var elapsed time.Duration
	if ag.active && !ag.turnStart.IsZero() {
		elapsed = now.Sub(ag.turnStart)
	}
	out := StreamSnapshot{
		Active: ag.active, Text: ag.textBuf, Reasoning: ag.reasonBuf, Thinking: ag.thinking,
		Thought: thought, Elapsed: elapsed,
		SessionIn: ag.sessIn, SessionOut: ag.sessOut, LastIn: ag.lastIn,
		CacheRead: ag.cacheRead, CacheBilled: ag.cacheBilled,
		CtxWindow: ag.ctxWin, ByLoop: ag.byLoop,
	}
	out.Model = ag.modelSeed
	if out.Model == "" {
		out.Model = h.liveModelID()
	}
	if out.CtxWindow == 0 {
		out.CtxWindow = h.contextWindow(out.Model)
	}
	out.Steering = queuedRows(ag.steering)
	out.Tools = append(out.Tools, ag.tools...)
	out.Retries, out.CheckingGoal, out.SessionCost = ag.retries, ag.checkingGoal, ag.sessCost
	out.Turn = ag.turns
	if !ag.retryAt.IsZero() {
		out.Retrying, out.RetryIn, out.RetryWhy = true, max(ag.retryAt.Sub(now), 0), ag.retryWhy
	}
	return out
}

// SeedStream installs a stream snapshot directly (no turn runs). It backs
// golden tests and UI development: the next AgentStream read returns the
// seeded state until a real turn resets it. A queued message is not seeded
// with it: a test queues one with AgentSend, which a seeded active turn takes.
// TestSeedStreamSeedsEveryField holds this to every field a snapshot has.
func (h *Host) SeedStream(s StreamSnapshot) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.active = s.Active
	ag.byLoop = s.ByLoop
	ag.textBuf = s.Text
	ag.reasonBuf = s.Reasoning
	ag.tools = s.Tools
	ag.modelSeed = s.Model
	ag.thought = s.Thought
	ag.thinking = s.Thinking
	// an open block counts from now, so the stream reads the seeded total
	ag.thinkStart = time.Time{}
	if s.Thinking {
		ag.thinkStart = h.now()
	}
	ag.sessIn = s.SessionIn
	ag.sessOut = s.SessionOut
	ag.sessCost = s.SessionCost
	ag.cacheRead, ag.cacheBilled = s.CacheRead, s.CacheBilled
	ag.lastIn = s.LastIn
	ag.ctxWin = s.CtxWindow
	ag.checkingGoal = s.CheckingGoal
	if s.Elapsed > 0 && s.Active {
		ag.turnStart = h.now().Add(-s.Elapsed)
	} else {
		ag.turnStart = time.Time{}
	}
	ag.retries, ag.retryAt, ag.retryWhy = s.Retries, time.Time{}, ""
	ag.turns = s.Turn
	if s.Retrying {
		ag.retryAt, ag.retryWhy = h.now().Add(s.RetryIn), s.RetryWhy
	}
}
