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
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

const (
	compactReserve  = 20_000 // reply room; claude-code measured p99.99 summaries at ~17k
	compactBuffer   = 13_000
	compactMaxFails = 3
	summaryMaxOut   = compactReserve // the reply room compaction plans for
	toolKeepTokens  = 40_000         // ceiling on the newest tool output pruning leaves alone
	pruneMinTokens  = 4_000          // a smaller saving is not worth a prompt cache miss

	compactAuto     = "auto"
	compactOverflow = "overflow"
	compactManual   = "manual"

	// compactTailRole marks stored copies of the messages a compaction kept:
	// resume reads them, the transcript, rewind, export and the reflector skip them.
	compactTailRole = "tail"

	clearedOutput = "[old tool output cleared]"
)

const (
	compactPrefix = "The earlier part of this session was compacted into the summary below; messages after this one are verbatim. The user's latest request is the active one: continue it without recapping or acknowledging the summary.\n\n<summary>\n"
	compactSuffix = "\n</summary>"
)

const compactSystem = `You write a handoff summary of a coding session so another agent can continue it in a fresh context. You are not in the conversation: do not answer questions in it, do not call tools, output only the summary.

` + compactSections

// compactNote asks for the summary inside the conversation itself, so the
// call reads the provider's cache of its prefix.
const compactNote = `Stop the work here. Write a handoff summary of the conversation above so another agent can continue it in a fresh context. Do not call tools, output only the summary.

` + compactSections

const compactSections = `Use exactly these sections, keep all of them, write "(none)" when empty, terse bullets:
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
(exact commands, error messages, identifiers, URLs worth keeping; approaches that failed and why, so they are not retried)

If a <previous-summary> is given, or the conversation opens with a <summary>, merge it: keep everything still relevant, move finished work to Done, and where it conflicts with the conversation, the conversation wins.`

var errNoSummary = errors.New("compaction returned no usable summary")

// errNothingToCompact is a history too short to summarize: a warn, never a failure.
var errNothingToCompact = errors.New("nothing to compact yet")

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
			h.compactFailed("the context was still full after compacting")
		} else {
			ag.mu.Lock()
			ag.compactFails = 0
			ag.mu.Unlock()
		}
	}
	return h.compactDue()
}

// compactFailed counts a failed auto compaction and reports whether it was
// the one that turned auto compaction off, which then says so with the cause.
func (h *Host) compactFailed(cause string) bool {
	ag := h.agents
	ag.mu.Lock()
	ag.compactFails++
	off := ag.compactFails == compactMaxFails
	ag.mu.Unlock()
	if off {
		h.Notify(slog.LevelWarn, "Auto compaction off", cause+"; /compact or /new")
	}
	return off
}

// contextTooLarge also matches Anthropic's "input length and `max_tokens`
// exceed context limit", which fantasy does not flag.
func contextTooLarge(err error) bool {
	pe, ok := errors.AsType[*fantasy.ProviderError](err)
	return ok && (pe.IsContextTooLarge() || strings.Contains(pe.Message, "exceed context limit"))
}

// sendCompacting runs one turn inside the context window: a turn Full cut
// at a step boundary continues after compaction, and a prompt the provider
// refuses as too long is compacted and resent once. It returns the error
// that ended the turn.
func (h *Host) sendCompacting(ctx context.Context, runner *agent.Runner, text string) error {
	ag := h.agents
	retried, capDropped, imagesDropped := false, false, false
	for {
		ag.mu.Lock()
		ag.holdOverflow = !retried && ag.compactFails < compactMaxFails
		ag.holdImages = !imagesDropped && !runner.TextOnly
		ag.holdCap = !capDropped && runner.MaxOutputTokens > 0
		ag.holdCut = true
		hold := ag.holdOverflow
		ag.mu.Unlock()
		err := runner.Send(ctx, text)
		text = "" // history holds the user message from here on
		if err != nil && !errors.Is(err, agent.ErrTurnActive) {
			h.Log.Error("turn failed", "origin", "agent", "error", err)
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
		// says so, remember it, and send again rather than ending the turn;
		// an image too large leaves the history, and later ones still go
		if !imagesDropped && !runner.TextOnly && imagesRefused(err) {
			imagesDropped = true
			ag.mu.Lock()
			model := ag.turnModel
			ag.beginTurn()
			ag.mu.Unlock()
			if imageTooLarge(err) {
				runner.DropImages("too large to send")
				h.Notify(slog.LevelWarn, "Image left out", "too large for "+model+"; the turn goes on without it")
			} else {
				runner.TextOnly = true
				imagesRejected.Store(model, true)
				h.Notify(slog.LevelWarn, "Images left out", model+" takes no images; vision = false on its model entry skips this")
			}
			continue
		}
		reason := compactAuto
		switch {
		case hold && contextTooLarge(err):
			retried, reason = true, compactOverflow
		case err == nil && runner.CutShort():
			// Full stopped the turn before the model read its tool results
		default:
			return err
		}
		// the held TurnDone zeroed the turn clock; the working rule shows
		// "Compacting", the elapsed time and the esc hint only with one
		ag.mu.Lock()
		ag.beginTurn()
		ag.mu.Unlock()
		if !h.compactReporting(ctx, runner, reason, "") {
			// the held finish: the error, or a cut that stays cut; an esc
			// during the summary reads as interrupted like any other
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			h.streamHooks().TurnDone(err, fantasy.Usage{})
			return err
		}
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
		return Usagef("a turn is running; compact after it")
	case runner == nil || len(runner.History) < 2:
		ag.mu.Unlock()
		h.Notify(slog.LevelWarn, "Nothing to compact yet", "")
		return nil
	}
	ag.beginTurn()
	scope := h.claim() // esc stops the summary call
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
		h.Log.Warn("compaction failed", "origin", "agent", "error", err, "reason", reason)
		switch {
		case errors.Is(err, errNothingToCompact):
			h.Notify(slog.LevelWarn, "Nothing to compact yet", "")
		case reason == compactManual:
			h.Notify(slog.LevelError, "Could not compact", h.friendlyError(err))
		default:
			// the turn goes on, or ends on its own error
			if cause := h.friendlyError(err); !h.compactFailed(cause) {
				h.Notify(slog.LevelWarn, "Could not compact", cause)
			}
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
		pruned, freed := pruneToolOutput(history, window, false)
		h.agents.mu.Lock()
		used := h.agents.lastIn - freed
		h.agents.mu.Unlock()
		if freed > 0 && reason == compactAuto && used < threshold {
			runner.History = pruned
			h.compactDone(used, false)
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowCompact, Text: fmt.Sprintf("Cleared %dk tokens of old tool output", freed/1000)})
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
	start := tailStart(body, tailKeep(threshold))
	if start == 0 {
		// It all fits the tail, yet the context is full: the latest request
		// stays verbatim, and a lone turn, with nothing older, goes whole.
		for start = len(body) - 1; start > 0 && body[start].Role != fantasy.MessageRoleUser; start-- {
		}
		if start <= 0 {
			start = len(body)
		}
	}
	head, tail := body[:start], body[start:]
	if len(head) == 0 {
		return errNothingToCompact
	}
	// On the conversation's own model the summary is asked after the runner's
	// whole unpruned history, the exact prefix the last step read, so the
	// provider reads it from a warm cache entry; the flat transcript goes to a
	// separate compact model, when that call gives no summary, or after an
	// overflow, where the whole history was just refused.
	var out string
	err := errNoSummary
	h.agents.mu.Lock()
	model := h.agents.turnModel
	h.agents.mu.Unlock()
	if model != "" && model == h.roleModel("compact") && reason != compactOverflow {
		out, err = h.summarizeAside(ctx, runner, len(tail), model, focus)
	}
	if errors.Is(err, errNoSummary) || contextTooLarge(err) {
		out, err = h.summarize(ctx, head, prev, focus, window)
	}
	switch {
	case err == nil:
	case reason != compactManual && (errors.Is(err, errNoSummary) || contextTooLarge(err)):
		// NOTE: the earlier history is lost unsummarized; still better than a
		// session that can never send again.
		h.Log.Warn("summary failed, dropping history", "origin", "agent", "error", err, "messages", len(head))
		out = strings.TrimSpace(fmt.Sprintf("%s\n\n(%d earlier messages were dropped unsummarized to fit the context window)", prev, len(head)))
	default:
		return err
	}
	// The summarizer keeps what it chooses of the todos calls in head; the
	// stored list goes after the summary, where summaryOf leaves it behind,
	// so the next compaction never merges a stale one forward.
	var todos string
	if h.DB != nil {
		rows, _ := h.DB.Todos(h.AgentSessionID())
		if slices.ContainsFunc(rows, func(td store.Todo) bool { return td.Status != store.TodoDone }) {
			todos = "\n\nThe todo list as of this compaction:\n" + tools.TodoList(rows)
		}
	}
	summary := agent.UserMessage(compactPrefix + out + compactSuffix + todos)
	if err := h.storeCompaction(summary, tail); err != nil {
		return err
	}
	runner.History = append([]fantasy.Message{summary}, tail...)
	h.agents.mu.Lock()
	overhead := h.agents.promptTokens // every prompt carries it, the messages are not the whole context
	h.agents.mu.Unlock()
	h.compactDone(estTokens(runner.History...)+overhead, reason == compactManual)
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowCompact, Text: fmt.Sprintf("Compacted %d messages", len(head))})
	return nil
}

// compactDone installs the post-compaction estimate, re-pins the digest,
// drops the pinned prompt and has the next step judge the result; a manual
// success re-enables auto compaction.
func (h *Host) compactDone(used int64, manual bool) {
	ag := h.agents
	brain, globs := h.brainFor()
	h.forgetInstructions()
	ag.mu.Lock()
	// the prompt cache is gone anyway, so the next send rebuilds the whole
	// prompt with a fresh environment and brain render
	ag.lastIn, ag.brain, ag.brainGlobs, ag.env, ag.compacted = used, brain, globs, "", true
	ag.dropPin()
	if manual {
		ag.compactFails = 0
	}
	ag.mu.Unlock()
}

// storeCompaction stores the summary and copies of the verbatim tail in one
// transaction, so the rows from the summary
// on always rebuild the whole history: a crash mid-write would otherwise
// leave a resumed conversation short of its recent messages.
func (h *Host) storeCompaction(summary fantasy.Message, tail []fantasy.Message) error {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return nil
	}
	row := func(m fantasy.Message, role string) (*store.Message, error) {
		parts, err := json.Marshal(m, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		return &store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)}, nil
	}
	head, err := row(summary, string(fantasy.MessageRoleUser))
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
	// the budget is the smaller window: the call runs on the compact model
	if w := h.contextWindow(h.roleModel("compact")); w > 0 && (window <= 0 || w < window) {
		window = w
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
		res, err := h.generate(ctx, h.AgentSessionID(), compactSystem, b.String(), &maxOut, h.streamHooks().Retrying, "compact")
		switch {
		case contextTooLarge(err) && tries < 3:
			budget = len(text) * 4 / 5
			continue
		case err != nil:
			return "", err
		}
		return summaryText(res)
	}
}

// summarizeAside asks for the summary of the runner's history but its last
// tail messages, the verbatim tail, one step on the runner's system prompt,
// tools and breakpoints (Runner.Aside), billed like /btw.
func (h *Host) summarizeAside(ctx context.Context, runner *agent.Runner, tail int, model, focus string) (string, error) {
	note := compactNote
	if tail > 0 {
		note += fmt.Sprintf("\n\nYour summary replaces the conversation above except its last %d %s, kept verbatim after it: summarize only what comes before.",
			tail, Plural(tail, "message", "messages"))
	}
	if focus != "" {
		note += "\n\nAdditional focus: " + focus
	}
	res, err := runner.Aside(ctx, runner.History, note)
	if err != nil {
		return "", err
	}
	h.bill(h.AgentSessionID(), model, res.TotalUsage)
	return summaryText(res)
}

// summaryText is a summary call's reply: errNoSummary when it is empty or a
// tool call.
func summaryText(res *fantasy.AgentResult) (string, error) {
	out := strings.TrimSpace(concatText(res.Response.Content))
	switch {
	case out == "" || res.Response.FinishReason == fantasy.FinishReasonToolCalls:
		return "", errNoSummary
	case res.Response.FinishReason == fantasy.FinishReasonLength:
		// A summary cut at the output limit still beats the fallback, which
		// drops the earlier history unsummarized.
		out += "\n\n(the summary was cut off at the output limit)"
	}
	return out, nil
}

// summaryOf returns the summary a compaction message carries.
func summaryOf(m fantasy.Message) (string, bool) {
	if m.Role != fantasy.MessageRoleUser || len(m.Content) == 0 {
		return "", false
	}
	p, _ := m.Content[0].(fantasy.TextPart)
	rest, ok := strings.CutPrefix(p.Text, compactPrefix)
	if !ok {
		return "", false
	}
	s, _, _ := strings.Cut(rest, compactSuffix)
	return s, true
}

// tailKeep is the verbatim tail's token budget under a compaction threshold.
func tailKeep(threshold int64) int64 { return min(max(threshold/4, 2_000), 15_000) }

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

// keptTools name results that are not output: a skill body is
// instructions, and question and plan carry the user's own words, which
// nothing else in the history holds.
var keptTools = []string{tools.ToolSkill, tools.ToolQuestion, tools.ToolPlan}

// pruneToolOutput clears tool results older than the newest kept tokens of
// them when that frees at least max(kept/2, pruneMinTokens), and returns
// what it freed. kept is a quarter of the compaction threshold, at most
// toolKeepTokens, which an unknown window gets. inTurn also clears what the
// running turn produced, which only a subagent asks for: its whole run is
// one turn. The input history is not modified.
func pruneToolOutput(history []fantasy.Message, window int64, inTurn bool) ([]fantasy.Message, int64) {
	kept := int64(toolKeepTokens)
	if window > 0 {
		kept = min(kept, compactThreshold(window)/4)
	}
	keep := map[string]bool{}
	for _, m := range history {
		for _, part := range m.Content {
			if p, ok := part.(fantasy.ToolCallPart); ok && slices.Contains(keptTools, p.ToolName) {
				keep[p.ToolCallID] = true
			}
		}
	}
	// Output after the last user message belongs to the turn in flight (a
	// step Full cut continues from it): it counts against the kept budget
	// but is never cleared.
	live := len(history)
	if !inTurn {
		for i, h := range slices.Backward(history) {
			if h.Role == fantasy.MessageRoleUser {
				live = i
				break
			}
		}
	}
	type loc struct{ msg, part int }
	var old []loc
	var seen, freed int64
	for i, h := range slices.Backward(history) {
		for j, part := range h.Content {
			p, ok := part.(fantasy.ToolResultPart)
			text := toolText(p)
			if !ok || keep[p.ToolCallID] || text == "" || text == clearedOutput {
				continue
			}
			n := int64(len(text) / 4)
			if seen += n; seen > kept && i < live {
				old, freed = append(old, loc{i, j}), freed+n
			}
		}
	}
	if freed < max(kept/2, pruneMinTokens) {
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

// transcriptText flattens history for the summarizer: text as written,
// tool calls and results abbreviated.
func transcriptText(history []fantasy.Message) string {
	var b strings.Builder
	for _, m := range history {
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				if m.Role != fantasy.MessageRoleUser {
					fmt.Fprintf(&b, "%s: %s\n", m.Role, p.Text)
					break
				}
				// attachments, notes and reports are not the user's words,
				// and the summarizer is told to quote those
				typed, extra, _ := strings.Cut(p.Text, agent.AttachMarker)
				if typed != "" {
					fmt.Fprintf(&b, "user: %s\n", typed)
				}
				if extra != "" {
					fmt.Fprintf(&b, "attached, not typed by the user: %s\n", truncRunes(strings.TrimSpace(strings.TrimSuffix(extra, "</attached-files>")), 2000))
				}
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
