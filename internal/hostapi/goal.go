package hostapi

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// goalRole marks a stored goal event: historyFrom draws it and goalIn reads
// the goal back, so a rewind cuts it, a fork copies it and a delete drops it.
const goalRole = "goal"

// goalMaxRuns bounds the turns a goal starts on its own before anyone else
// starts one.
// NOTE: a fixed cap; a goal.max_turns key is the upgrade if long unattended
// goals need more.
const goalMaxRuns = 20

type goalEvent string

const (
	goalSet     goalEvent = "set"
	goalNotYet  goalEvent = "not yet"
	goalMet     goalEvent = "met"
	goalBlocked goalEvent = "blocked"
	goalPaused  goalEvent = "paused"
	goalStopped goalEvent = "stopped"
)

// goalRow is a stored goal event. A set row carries the session's spend
// when it was set; a met row the spend the goal took, as its notice shows.
type goalRow struct {
	Event  goalEvent `json:"event"`
	Text   string    `json:"text,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Tokens int64     `json:"tokens,omitzero"`
	Cost   float64   `json:"cost,omitzero"`
}

// Goal is the session's goal (/goal): what must hold, when it was set and
// the session's spend then, and the checks so far.
type Goal struct {
	Condition string
	Reason    string // the last check, "not yet: <why>", "" before the first
	Turns     int    // the turns it was checked after
	Since     time.Time
	Tokens    int64
	Cost      float64
}

const goalBrief = `Work until the conversation shows it holds: run the checks that prove it and keep their output in view. After each of your turns zemi checks the goal against the conversation and sends you back while it does not hold. If only the user can unblock it, say what you need and stop.`

const goalCheckPrompt = `Judge whether the session's goal holds now; do not work on it. Call no tools: none will run.
Goal: %s
Decide from what the conversation shows: command output, test results and file contents count; the assistant saying it is done does not.
Answer with one line: MET, NOT YET or BLOCKED, a colon, and one sentence.
MET: the evidence shows the goal holds; name the evidence.
NOT YET: name what is still missing or failing, for the next turn to act on.
BLOCKED: the conversation shows it cannot go on without the user (a decision, access, a tool call nobody could approve, a goal that contradicts itself); name what is needed.`

// Goal is the live session's goal, false for none.
func (h *Host) Goal() (Goal, bool) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.goal == nil {
		return Goal{}, false
	}
	return *ag.goal, true
}

// SetGoal runs /goal: alone it shows the goal, "stop" drops it, and a
// condition is sent as a message whose brief sets it once stored.
func (h *Host) SetGoal(args string) error {
	typed, attached := agent.CutAttached(args)
	cond := strings.Join(strings.Fields(typed), " ")
	switch {
	case cond == "":
		return h.openGoal()
	case cond == "stop":
		return h.stopGoal()
	case !strings.Contains(cond, " "):
		return Usagef("a goal names an end state you can check: /goal make check is clean; /goal stop drops the current one")
	case h.readOnly():
		return Usagef("a read-only agent cannot work toward a goal; ctrl+t switches the agent")
	}
	ag := h.agents
	ag.mu.Lock()
	ag.goalPending = cond
	ag.mu.Unlock()
	err := h.AgentSend("/goal " + cond + attached + goalAttached(cond))
	if err != nil {
		ag.mu.Lock()
		ag.goalPending = ""
		ag.mu.Unlock()
	}
	return err
}

func goalAttached(cond string) string {
	return agent.Attached("goal", "The session's goal: "+cond+"\n"+goalBrief)
}

// setGoalFrom sets the goal when msg carries the brief SetGoal last sent,
// never one that attached text (a plugin send, a file, a report) quotes,
// and records it after msg, so a rewind to it cuts the goal too.
func (h *Host) setGoalFrom(sid string, msg fantasy.Message) {
	ag := h.agents
	ag.mu.Lock()
	cond := ag.goalPending
	if cond == "" || !strings.Contains(concatText(msg.Content), goalAttached(cond)) {
		ag.mu.Unlock()
		return
	}
	ag.goalPending = ""
	old := ag.goal
	g := &Goal{Condition: cond, Since: time.Now(), Tokens: ag.sessIn + ag.sessOut, Cost: ag.sessCost}
	ag.goal = g
	ag.mu.Unlock()
	h.storeGoal(sid, goalRow{Event: goalSet, Text: cond, Tokens: g.Tokens, Cost: g.Cost})
	if old != nil {
		h.Notify(slog.LevelInfo, "Replaced the goal", old.Condition)
	}
	h.refresh()
}

func (h *Host) stopGoal() error {
	ag := h.agents
	ag.mu.Lock()
	g, sid := ag.goal, ag.sessionID
	ag.goal = nil
	ag.mu.Unlock()
	if g == nil {
		return Usagef("no goal to stop")
	}
	h.recordGoal(sid, goalRow{Event: goalStopped})
	return nil
}

func (h *Host) openGoal() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	g, ok := h.Goal()
	if !ok {
		return Usagef("no goal; /goal <condition> sets one")
	}
	running := fmtDuration(time.Since(g.Since))
	if g.Turns > 0 {
		running += fmt.Sprintf(", checked after %d %s", g.Turns, Plural(g.Turns, "turn", "turns"))
	}
	ui.OpenOverlay(Overlay{
		ID: "goal", Title: "Goal", Origin: lua.OriginBuiltin,
		Fields: []Field{{"goal", g.Condition}, {"running", running}, {"spent", h.spentSince(g.Tokens, g.Cost)}, {"last", g.Reason}},
		Items:  []any{map[string]any{"text": "Stop the goal", "run": "goal stop", "verb": "stop"}},
	})
	return nil
}

// spentSince is what the session spent since it had spent tokens and cost;
// $ only with a price.
func (h *Host) spentSince(tokens int64, cost float64) string {
	s := h.AgentStream()
	out := fmtTokens(s.SessionIn+s.SessionOut-tokens) + " tokens"
	if spent := s.SessionCost - cost; spent > 0 {
		out += ", " + fmtCost(spent)
	}
	return out
}

// checkGoal asks the model, as an aside on the conversation, whether g
// holds, records the verdict and returns the note the next turn sends: ""
// ends the chain. An interrupt records nothing, and neither does a goal
// stopped or replaced meanwhile.
func (h *Host) checkGoal(ctx context.Context, g *Goal) string {
	reply, err := h.aside(ctx, fmt.Sprintf(goalCheckPrompt, g.Condition))
	if ctx.Err() != nil {
		return ""
	}
	ev, reason, ok := parseVerdict(reply)
	switch {
	case err != nil:
		ev, reason = goalPaused, "the check failed: "+h.friendlyError(err)
	case !ok:
		h.Log.Debug("no verdict", "origin", "goal", "reply", clipRunes(reply, 200))
		ev, reason = goalPaused, "the check gave no verdict"
	}
	ag := h.agents
	ag.mu.Lock()
	if ag.goal != g {
		ag.mu.Unlock()
		return ""
	}
	if ev == goalNotYet && ag.goalRuns >= goalMaxRuns {
		ev, reason = goalPaused, fmt.Sprintf("after %d turns without you; send a message to go on", goalMaxRuns)
	}
	g.Turns++
	g.Reason = goalReason(ev, reason)
	sid := ag.sessionID
	if ev == goalMet {
		ag.goal = nil
	}
	ag.mu.Unlock()
	row := goalRow{Event: ev, Text: reason}
	if ev == goalMet {
		row.Detail = fmt.Sprintf("%d %s, %s, %s", g.Turns, Plural(g.Turns, "turn", "turns"), fmtDuration(time.Since(g.Since)), h.spentSince(g.Tokens, g.Cost))
	}
	h.recordGoal(sid, row)
	if ev != goalNotYet {
		return ""
	}
	why := "Not met yet."
	if reason != "" {
		why = "Not met yet: " + reason
	}
	return agent.Attached("goal", why+"\nKeep working until this holds: "+g.Condition)
}

// recordGoal stores a goal event and draws its notice row.
func (h *Host) recordGoal(sid string, row goalRow) {
	h.storeGoal(sid, row)
	if m, ok := goalNotice(row); ok {
		h.AppendChat(m)
	}
}

func (h *Host) storeGoal(sid string, row goalRow) {
	if h.DB == nil || sid == "" {
		return
	}
	parts, _ := json.Marshal(row, json.Deterministic(true))
	if err := h.DB.AddMessage(&store.Message{SessionID: sid, Role: goalRole, PartsJSON: string(parts)}); err != nil {
		h.Log.Warn("event not stored", "origin", "goal", "error", err)
	}
}

// goalNotice is a goal event's row, live and resumed alike; a set row has none.
func goalNotice(row goalRow) (ChatMessage, bool) {
	m := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "Goal " + string(row.Event), Detail: row.Detail}
	switch row.Event {
	case goalSet:
		return m, false
	case goalNotYet:
		m.Text = "Goal not met yet"
	case goalBlocked:
		m.Detail = "/goal stop drops it"
	case goalMet, goalPaused, goalStopped:
	}
	if row.Text != "" {
		m.Text += ": " + sanitize.String(row.Text)
	}
	return m, true
}

// goalIn is the goal rows leave set, nil for none.
func goalIn(rows []*store.Message) *Goal {
	var g *Goal
	for _, m := range rows {
		var row goalRow
		if m.Role != goalRole || json.Unmarshal([]byte(m.PartsJSON), &row) != nil {
			continue
		}
		switch row.Event {
		case goalSet:
			g = &Goal{Condition: row.Text, Since: time.UnixMilli(m.CreatedAt), Tokens: row.Tokens, Cost: row.Cost}
		case goalMet, goalStopped:
			g = nil
		default:
			if g != nil {
				g.Turns++
				g.Reason = goalReason(row.Event, row.Text)
			}
		}
	}
	return g
}

// goalReason is a check's verdict as /goal and zemi run name it.
func goalReason(ev goalEvent, why string) string {
	if why == "" {
		return string(ev)
	}
	return string(ev) + ": " + why
}

// parseVerdict reads the first line of the judge's reply that opens with
// MET, NOT YET or BLOCKED, markdown and case aside, and the reason after it.
func parseVerdict(reply string) (goalEvent, string, bool) {
	for line := range strings.Lines(reply) {
		head, reason, _ := strings.Cut(line, ":")
		if ev := goalEvent(strings.ToLower(strings.Trim(head, "*`#- \t\r\n"))); ev == goalMet || ev == goalNotYet || ev == goalBlocked {
			return ev, clipRunes(sanitize.String(strings.Trim(reason, "*` \t\r\n")), 200), true
		}
	}
	return "", "", false
}
