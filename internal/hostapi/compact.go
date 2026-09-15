// compact.go keeps long sessions inside the context window (DESIGN §10.7):
// once the last prompt passed compactAt of the window, the history is
// summarized by one model call and replaced by that summary before the
// next turn. The summary persists as a message the session points at, so
// /resume starts from it, and the digest is re-pinned at the same point.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

const compactAt = 0.8

const compactPrefix = "Summary of the conversation so far:\n\n"

const compactSystem = `You compress a coding session so the work can continue in a fresh context. Write a dense summary: the user's goals and constraints, decisions made, files touched and their current state, commands that worked or failed, open problems, and the next steps. Keep exact names, paths and error messages. No preamble.`

// compactDue reports whether the last prompt used enough of the context
// window that the next turn should start from a summary.
func (h *Host) compactDue() bool {
	s := h.AgentStream()
	return s.CtxWindow > 0 && float64(s.LastIn) >= compactAt*float64(s.CtxWindow)
}

// CompactNow runs /compact: the conversation is summarized in the
// background; messages sent meanwhile queue and run after it.
func (h *Host) CompactNow() error {
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
	ag.mu.Unlock()
	h.refresh()
	h.GoSafe("compaction", func() {
		defer h.endTurnAfterPanic()
		h.compactReporting(runner)
		ag.mu.Lock()
		queued := strings.Join(ag.steering, "\n\n")
		ag.steering, ag.active = nil, false
		ag.mu.Unlock()
		h.refresh()
		if queued != "" {
			if err := h.AgentSend(queued); err != nil {
				h.memNotify("error", "agent", err.Error())
			}
		}
	})
	return nil
}

// compactReporting compacts with the working row reading "Compacting";
// a failure is reported and the turn goes on with the full history.
func (h *Host) compactReporting(runner *agent.Runner) {
	h.setCompacting(true)
	defer h.setCompacting(false)
	if err := h.compact(context.Background(), runner); err != nil {
		h.Log.Warn("agent", "compaction failed", map[string]any{"error": err.Error()})
		h.memNotify("error", "compact", err.Error())
	}
}

func (h *Host) setCompacting(on bool) {
	h.agents.mu.Lock()
	h.agents.compacting = on
	h.agents.mu.Unlock()
	h.refresh()
}

// compact replaces an idle runner's history with a model-written summary.
func (h *Host) compact(ctx context.Context, runner *agent.Runner) error {
	history := runner.History
	if len(history) < 2 {
		return nil
	}
	model, err := h.resolveModel(nil)
	if err != nil {
		return err
	}
	out, err := generateText(ctx, model, compactSystem, transcriptText(history))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("compaction returned an empty summary")
	}
	summary := fantasy.Message{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: compactPrefix + out}},
	}
	if sid := h.AgentSessionID(); sid != "" && h.DB != nil {
		parts, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		msg := &store.Message{SessionID: sid, Role: "user", PartsJSON: string(parts)}
		if err := h.DB.AddMessage(msg); err != nil {
			return err
		}
		if err := h.DB.SetSummary(sid, msg.ID); err != nil {
			return err
		}
	}
	runner.History = []fantasy.Message{summary}
	digest := h.digestFor()
	h.agents.mu.Lock()
	h.agents.lastIn, h.agents.digest = 0, digest
	h.agents.mu.Unlock()
	h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "compact", Text: fmt.Sprintf("compacted %d messages", len(history))})
	return nil
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
			case fantasy.ToolCallPart:
				fmt.Fprintf(&b, "tool call %s: %s\n", p.ToolName, truncRunes(p.Input, 500))
			case fantasy.ToolResultPart:
				if p.Output == nil {
					continue
				}
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](p.Output); ok {
					fmt.Fprintf(&b, "tool result: %s\n", truncRunes(txt.Text, 2000))
				}
			}
		}
	}
	return b.String()
}
