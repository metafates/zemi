// compact.go keeps long sessions inside the context window (DESIGN §10.7).
// The context each step used is checked before a turn and after every step;
// past compactThreshold old tool output is cleared, and when that is not
// enough the older history is summarized by one model call while the recent
// messages stay verbatim. A prompt the provider refuses as too long takes
// the same path once. The summary and a copy of the kept messages are stored
// after the rows they replace, so /resume starts from the summary.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

const (
	compactReserve  = 20_000 // reply room; claude-code measured p99.99 summaries at ~17k
	compactBuffer   = 13_000
	compactMaxFails = 3
	summaryMaxOut   = compactReserve // the reply room compaction plans for
	toolKeepTokens  = 40_000         // newest tool output pruning leaves alone
	pruneMinTokens  = 20_000         // a smaller saving is not worth a prompt cache miss

	compactAuto     = "auto"
	compactOverflow = "overflow"
	compactManual   = "manual"

	// compactTailRole marks stored copies of the messages a compaction kept:
	// resume reads them, the transcript, rewind, export and the reflector skip them.
	compactTailRole = "tail"

	clearedOutput = "[old tool output cleared]"
)

const (
	compactPrefix       = "The earlier part of this session was compacted into the summary below. Recent messages follow verbatim. Treat the summary as background: the latest user message is the active request. Continue the current task without recapping or acknowledging the summary.\n\n<summary>\n"
	compactSuffix       = "\n</summary>"
	legacyCompactPrefix = "Summary of the conversation so far:\n\n"
)

const compactSystem = `You write a handoff summary of a coding session so another agent can continue it in a fresh context. You are not in the conversation: do not answer questions in it, do not call tools, output only the summary.

Use exactly these sections, keep all of them, write "(none)" when empty, terse bullets:
## Goal
## User instructions and preferences
(quote the user's own words for anything that constrains the work)
## Done
## In progress
(what was being done in the last messages, precisely; include partial edits)
## Next step
(only what the latest user request calls for; quote the request)
## Files
(path: what changed or why it matters)
## Facts
(exact commands, error messages, identifiers, URLs worth keeping)

If a <previous-summary> is given, merge it: keep everything still relevant, move finished work to Done, and where it conflicts with the conversation, the conversation wins.`

var errNoSummary = errors.New("compaction returned no usable summary")

// compactThreshold is the context size that triggers compaction: room for
// a reply plus slack, but never below half the window, so small windows are
// not compacted every turn. 0 when the window is unknown.
func compactThreshold(window int64) int64 {
	if window <= 0 {
		return 0
	}
	return max(window-compactReserve-compactBuffer, window/2)
}

// compactDue reports whether the last step used enough of the context
// window to compact. An unknown window never is: only a provider refusal
// can tell.
func (h *Host) compactDue() bool {
	s := h.AgentStream()
	h.agents.mu.Lock()
	fails := h.agents.compactFails
	h.agents.mu.Unlock()
	return s.CtxWindow > 0 && fails < compactMaxFails && s.LastIn >= compactThreshold(s.CtxWindow)
}

// contextFull is the runner's Full: it records the context a step used and
// reports whether compaction is due. The first step after a compaction
// judges it: a context still full counts as a failure.
func (h *Host) contextFull(used int64) bool {
	ag := h.agents
	ag.mu.Lock()
	ag.lastIn = used
	judge := ag.compacted
	ag.compacted = false
	ag.mu.Unlock()
	if judge {
		if h.compactDue() {
			h.compactFailed()
		} else {
			ag.mu.Lock()
			ag.compactFails = 0
			ag.mu.Unlock()
		}
	}
	return h.compactDue()
}

func (h *Host) compactFailed() {
	ag := h.agents
	ag.mu.Lock()
	ag.compactFails++
	off := ag.compactFails == compactMaxFails
	ag.mu.Unlock()
	if off {
		h.Notify("warn", "compact", fmt.Sprintf("auto compaction is off for this session after %d failed tries: /compact or /new", compactMaxFails))
	}
}

func contextTooLarge(err error) bool {
	var pe *fantasy.ProviderError
	return errors.As(err, &pe) && pe.IsContextTooLarge()
}

// sendCompacting runs one turn inside the context window: a turn Full cut
// at a step boundary continues after compaction, and a prompt the provider
// refuses as too long is compacted and resent once.
func (h *Host) sendCompacting(ctx context.Context, runner *agent.Runner, text string) {
	ag := h.agents
	retried, capDropped, imagesDropped := false, false, false
	for {
		ag.mu.Lock()
		ag.holdOverflow = !retried && ag.compactFails < compactMaxFails
		ag.holdImages = !imagesDropped && !runner.TextOnly
		hold := ag.holdOverflow
		ag.mu.Unlock()
		err := runner.Send(ctx, text)
		text = "" // history holds the user message from here on
		if err != nil && !errors.Is(err, agent.ErrTurnActive) {
			h.Log.Error("agent", "turn failed", map[string]any{"error": err.Error()})
		}
		// a model whose ceiling is lower than the cap zemi asked for: drop
		// the cap, remember it, and send again rather than failing every turn
		if !capDropped && runner.MaxOutputTokens > 0 && maxTokensRefused(err) {
			capDropped = true
			ag.mu.Lock()
			capRejected.Store(ag.turnModel, true)
			ag.beginTurn()
			ag.mu.Unlock()
			runner.MaxOutputTokens = 0
			continue
		}
		// a model that refuses image parts: replace them with a note that
		// says so, remember it, and send again rather than ending the turn
		if !imagesDropped && !runner.TextOnly && imagesRefused(err) {
			imagesDropped = true
			ag.mu.Lock()
			model := ag.turnModel
			ag.beginTurn()
			ag.mu.Unlock()
			imagesRejected.Store(model, true)
			runner.TextOnly = true
			h.Notify("info", "agent", model+" does not take images; going on without them (set images = false on its model entry)")
			continue
		}
		reason := compactAuto
		switch {
		case hold && contextTooLarge(err):
			retried, reason = true, compactOverflow
		case err == nil && runner.CutShort():
			// Full stopped the turn before the model read its tool results
		default:
			return
		}
		if !h.compactReporting(ctx, runner, reason, "") {
			if err != nil {
				h.streamHooks().TurnDone(err, fantasy.Usage{}) // the held error
			}
			return
		}
		ag.mu.Lock()
		ag.beginTurn()
		ag.mu.Unlock()
	}
}

// CompactNow runs /compact: the conversation is summarized in the
// background, with focus (may be empty) saying what to keep; messages sent
// meanwhile queue and run after it.
func (h *Host) CompactNow(focus string) error {
	ag := h.agents
	ag.mu.Lock()
	runner := ag.runner
	switch {
	case ag.active:
		ag.mu.Unlock()
		return errors.New("a turn is running; compact after it")
	case runner == nil || len(runner.History) < 2:
		ag.mu.Unlock()
		return errors.New("nothing to compact yet")
	}
	ag.active = true
	ag.beginTurn()
	scope := h.beginScope() // esc stops the summary call
	ag.mu.Unlock()
	h.refresh()
	h.GoSafe("compaction", func() {
		defer h.endTurnAfterPanic()
		h.compactReporting(scope.ctx, runner, compactManual, strings.TrimSpace(focus))
		h.handOff(scope)
	})
	return nil
}

// compactReporting compacts with the working row reading "Compacting"; a
// failure is reported, the history stays whole, and it returns false.
func (h *Host) compactReporting(ctx context.Context, runner *agent.Runner, reason, focus string) bool {
	h.setCompacting(true)
	defer h.setCompacting(false)
	err := h.compact(ctx, runner, reason, focus)
	if err == nil {
		return true
	}
	if !errors.Is(err, context.Canceled) {
		h.Log.Warn("agent", "compaction failed", map[string]any{"error": err.Error(), "reason": reason})
		h.Notify("error", "compact", err.Error())
		if reason != compactManual {
			h.compactFailed()
		}
	}
	return false
}

func (h *Host) setCompacting(on bool) {
	h.agents.mu.Lock()
	h.agents.compacting = on
	h.agents.mu.Unlock()
	h.refresh()
}

// compact frees context in an idle runner's history. Auto compaction first
// clears old tool output and stops there when that is enough; otherwise
// everything but a verbatim tail is summarized, merging an earlier summary.
func (h *Host) compact(ctx context.Context, runner *agent.Runner, reason, focus string) error {
	history := runner.History
	window := h.AgentStream().CtxWindow
	threshold := compactThreshold(window)
	if reason != compactManual {
		pruned, freed := pruneToolOutput(history)
		h.agents.mu.Lock()
		used := h.agents.lastIn - freed
		h.agents.mu.Unlock()
		if freed > 0 && reason == compactAuto && used < threshold {
			runner.History = pruned
			h.compactDone(used, false)
			h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "compact", Text: fmt.Sprintf("cleared %dk tokens of old tool output", freed/1000)})
			return nil
		}
		history = pruned
	}

	var prev string
	body := history
	if len(body) > 0 {
		if s, ok := summaryOf(body[0]); ok {
			prev, body = s, body[1:]
		}
	}
	start := tailStart(body, min(max(threshold/4, 2_000), 15_000))
	if start == 0 {
		start = len(body) // it all fits the tail, yet the context is full
	}
	head, tail := body[:start], body[start:]
	if len(head) == 0 {
		return errors.New("nothing to compact yet")
	}
	out, err := h.summarize(ctx, head, prev, focus, window)
	switch {
	case err == nil:
	case reason != compactManual && (errors.Is(err, errNoSummary) || contextTooLarge(err)):
		// NOTE: the earlier history is lost unsummarized; still better than a
		// session that can never send again.
		h.Log.Warn("agent", "summary failed, dropping history", map[string]any{"error": err.Error(), "messages": len(head)})
		out = strings.TrimSpace(fmt.Sprintf("%s\n\n(%d earlier messages were dropped unsummarized to fit the context window)", prev, len(head)))
	default:
		return err
	}
	summary := textMessage(compactPrefix + out + compactSuffix)
	if err := h.storeCompaction(summary, tail); err != nil {
		return err
	}
	runner.History = append([]fantasy.Message{summary}, tail...)
	h.agents.mu.Lock()
	overhead := h.agents.promptTokens // every prompt carries it, the messages are not the whole context
	h.agents.mu.Unlock()
	h.compactDone(estTokens(runner.History...)+overhead, reason == compactManual)
	h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "compact", Text: fmt.Sprintf("compacted %d messages", len(head))})
	return nil
}

// compactDone installs the post-compaction estimate, re-pins the digest and
// has the next step judge the result; a manual success re-enables auto
// compaction.
func (h *Host) compactDone(used int64, manual bool) {
	digest := h.digestFor()
	ag := h.agents
	ag.mu.Lock()
	ag.lastIn, ag.digest, ag.compacted = used, digest, true
	if manual {
		ag.compactFails = 0
	}
	ag.mu.Unlock()
}

// storeCompaction stores the summary, copies of the verbatim tail and the
// session's summary pointer in one transaction, so the rows from the summary
// on always rebuild the whole history: a crash mid-write would otherwise
// leave a resumed conversation short of its recent messages.
func (h *Host) storeCompaction(summary fantasy.Message, tail []fantasy.Message) error {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return nil
	}
	row := func(m fantasy.Message, role string) (*store.Message, error) {
		parts, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		return &store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)}, nil
	}
	head, err := row(summary, "user")
	if err != nil {
		return err
	}
	rows := make([]*store.Message, 0, len(tail))
	for _, m := range tail {
		r, err := row(m, compactTailRole)
		if err != nil {
			return err
		}
		rows = append(rows, r)
	}
	return h.DB.AddCompaction(sid, head, rows)
}

// summarize asks the model for the handoff summary of head. The transcript
// is cut from the front to fit the window, and again by a fifth on each
// context-too-long refusal, up to three times. An empty or cut-off reply is
// errNoSummary.
func (h *Host) summarize(ctx context.Context, head []fantasy.Message, prev, focus string, window int64) (string, error) {
	model, err := h.modelFor("compact.model")
	if err != nil {
		return "", err
	}
	maxOut := int64(summaryMaxOut)
	transcript := transcriptText(head)
	budget := len(transcript)
	if window > 0 {
		maxOut = min(maxOut, window/4)
		budget = min(budget, int(max(window-maxOut-5_000, window/2)*4))
	}
	for tries := 0; ; tries++ {
		text := transcript
		if len(text) > budget {
			text = text[len(text)-budget:]
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			}
			text = "[earlier conversation truncated]\n" + text
		}
		var b strings.Builder
		if prev != "" {
			fmt.Fprintf(&b, "<previous-summary>\n%s\n</previous-summary>\n\n", prev)
		}
		fmt.Fprintf(&b, "<conversation>\n%s</conversation>", text)
		if focus != "" {
			fmt.Fprintf(&b, "\n\nAdditional focus: %s", focus)
		}
		res, err := fantasy.NewAgent(model, fantasy.WithSystemPrompt(compactSystem)).Generate(ctx, fantasy.AgentCall{
			Messages:        []fantasy.Message{textMessage(b.String())},
			MaxOutputTokens: &maxOut,
			StopWhen:        []fantasy.StopCondition{fantasy.StepCountIs(1)},
		})
		switch {
		case contextTooLarge(err) && tries < 3:
			budget = len(text) * 4 / 5
			continue
		case err != nil:
			return "", err
		}
		out := strings.TrimSpace(concatText(res.Response.Content))
		if out == "" {
			return "", errNoSummary
		}
		// A summary cut at the output limit still beats the fallback, which
		// drops the earlier history unsummarized.
		if res.Response.FinishReason == fantasy.FinishReasonLength {
			out += "\n\n(the summary was cut off at the output limit)"
		}
		return out, nil
	}
}

// summaryOf returns the summary a compaction message carries.
func summaryOf(m fantasy.Message) (string, bool) {
	if m.Role != fantasy.MessageRoleUser || len(m.Content) == 0 {
		return "", false
	}
	p, ok := m.Content[0].(fantasy.TextPart)
	switch {
	case ok && strings.HasPrefix(p.Text, compactPrefix):
		return strings.TrimSuffix(strings.TrimPrefix(p.Text, compactPrefix), compactSuffix), true
	case ok && strings.HasPrefix(p.Text, legacyCompactPrefix):
		return strings.TrimPrefix(p.Text, legacyCompactPrefix), true
	}
	return "", false
}

// tailStart is where the verbatim tail begins: the oldest message within
// keep tokens of the end that is not a tool result, so a tool call always
// keeps its result. len(history) when not even the last message fits.
func tailStart(history []fantasy.Message, keep int64) int {
	start := len(history)
	var n int64
	for i, h := range slices.Backward(history) {
		if n += estTokens(h); n > keep {
			break
		}
		if h.Role != fantasy.MessageRoleTool {
			start = i
		}
	}
	return start
}

// pruneToolOutput clears tool results older than the newest toolKeepTokens
// of them when that frees at least pruneMinTokens, and returns what it
// freed. Skill bodies stay: they are instructions, not output. The input
// history is not modified.
func pruneToolOutput(history []fantasy.Message) ([]fantasy.Message, int64) {
	skill := map[string]bool{}
	for _, m := range history {
		for _, part := range m.Content {
			if p, ok := part.(fantasy.ToolCallPart); ok && p.ToolName == "skill" {
				skill[p.ToolCallID] = true
			}
		}
	}
	// Output after the last user message belongs to the turn in flight (a
	// step Full cut continues from it): it counts against the kept budget
	// but is never cleared.
	live := len(history)
	for i, h := range slices.Backward(history) {
		if h.Role == fantasy.MessageRoleUser {
			live = i
			break
		}
	}
	type loc struct{ msg, part int }
	var old []loc
	var seen, freed int64
	for i, h := range slices.Backward(history) {
		for j, part := range h.Content {
			p, ok := part.(fantasy.ToolResultPart)
			text := toolText(p)
			if !ok || skill[p.ToolCallID] || text == "" || text == clearedOutput {
				continue
			}
			n := int64(len(text) / 4)
			if seen += n; seen > toolKeepTokens && i < live {
				old, freed = append(old, loc{i, j}), freed+n
			}
		}
	}
	if freed < pruneMinTokens {
		return history, 0
	}
	out := slices.Clone(history)
	cloned := map[int]bool{}
	for _, l := range old {
		if !cloned[l.msg] {
			out[l.msg].Content, cloned[l.msg] = slices.Clone(out[l.msg].Content), true
		}
		p := out[l.msg].Content[l.part].(fantasy.ToolResultPart)
		p.Output = fantasy.ToolResultOutputContentText{Text: clearedOutput}
		out[l.msg].Content[l.part] = p
	}
	return out, freed
}

// estTokens guesses the size of messages at 4 bytes a token, 1k per file.
func estTokens(msgs ...fantasy.Message) int64 {
	var n int64
	for _, m := range msgs {
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				n += int64(len(p.Text))
			case fantasy.ReasoningPart:
				n += int64(len(p.Text))
			case fantasy.ToolCallPart:
				n += int64(len(p.Input))
			case fantasy.ToolResultPart:
				n += int64(len(toolText(p)))
			case fantasy.FilePart:
				n += 4_000
			}
		}
	}
	return n / 4
}

func toolText(p fantasy.ToolResultPart) string {
	if p.Output == nil {
		return ""
	}
	txt, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](p.Output)
	return txt.Text
}

func textMessage(text string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}}
}

// transcriptText flattens history for the summarizer: text as written,
// tool calls and results abbreviated.
func transcriptText(history []fantasy.Message) string {
	var b strings.Builder
	for _, m := range history {
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				fmt.Fprintf(&b, "%s: %s\n", m.Role, p.Text)
			case fantasy.FilePart:
				// the summarizer only reads this text: name what it cannot see
				fmt.Fprintf(&b, "%s: [attached %s]\n", m.Role, p.MediaType)
			case fantasy.ToolCallPart:
				fmt.Fprintf(&b, "tool call %s: %s\n", p.ToolName, truncRunes(p.Input, 500))
			case fantasy.ToolResultPart:
				if txt := toolText(p); txt != "" {
					fmt.Fprintf(&b, "tool result: %s\n", truncRunes(txt, 2000))
				}
			}
		}
	}
	return b.String()
}
