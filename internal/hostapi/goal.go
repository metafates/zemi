package hostapi

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

// goalKV is the kv row the live goal persists in: what must hold and the log
// seq of the message that set it, so a reset (the chat it was set in gone)
// drops it.
const goalKV = "goal"

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

// Goal is the chat's goal (/goal): what must hold, when it was set and the
// log seq of the message that set it, and the checks so far. Turns and
// reason are this process' state; the condition, its set-at and the window
// that set it survive. The tags are the kv row's spelling; re-spelling them
// drops a live goal on resume.
type Goal struct {
	Condition string    `json:"Condition"`
	Reason    string    `json:"Reason"` // the last check, "not yet: <why>", "" before the first
	Turns     int       `json:"Turns"`  // the turns it was checked after
	SetAt     int64     `json:"SetAt"`  // the log seq of the message that set it
	Writer    string    `json:"Writer"` // the window that set it; '' predates the field
	Since     time.Time `json:"Since"`
	Tokens    int64     `json:"Tokens"`
	Cost      float64   `json:"Cost"`
}

const goalBrief = `Work until the conversation shows it holds: run the checks that prove it and keep their output in view. After each of your turns zemi checks the goal against the conversation and sends you back while it does not hold. If only the user can unblock it, say what you need and stop.`

const goalCheckPrompt = `Judge whether the session's goal holds now; do not work on it. Call no tools: none will run.
Goal: %s
Decide from what the conversation shows: command output, test results and file contents count; the assistant saying it is done does not.
Answer with one line: MET, NOT YET or BLOCKED, a colon, and one sentence.
MET: the evidence shows the goal holds; name the evidence.
NOT YET: name what is still missing or failing, for the next turn to act on.
BLOCKED: the conversation shows it cannot go on without the user (a decision, access, a tool call nobody could approve, a goal that contradicts itself); name what is needed.`

// Goal is the live goal, false for none.
func (h *Host) Goal() (Goal, bool) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.goal == nil {
		return Goal{}, false
	}
	return *ag.goal, true
}

// loadGoal reads a persisted goal into the live state at boot: a condition
// set before this run, whose message still stands in the log.
func (h *Host) loadGoal() {
	if h.DB == nil {
		return
	}
	raw, err := h.DB.GetKV(goalKV)
	if err != nil {
		return
	}
	var g Goal
	if json.Unmarshal([]byte(raw), &g) != nil || g.Condition == "" {
		return
	}
	if count, err := h.DB.LogCount(); err != nil || g.SetAt >= count {
		return // the log moved under it (a reset cleared it but not the row)
	}
	h.agents.mu.Lock()
	h.agents.goal = &g
	h.agents.mu.Unlock()
}

// storeGoal persists the goal's condition and set-at, or drops the row; a
// missed write costs the goal its persistence, never the chat.
func (h *Host) storeGoal(g *Goal) {
	if h.DB == nil {
		return
	}
	if g == nil {
		_, _ = h.DB.DelKV(goalKV)
		return
	}
	if raw, err := json.Marshal(*g, json.Deterministic(true)); err == nil {
		if err := h.DB.SetKV(goalKV, string(raw)); err != nil {
			h.Log.Warn("goal not kept", "origin", "goal", "error", err)
		}
	}
}

// unstoreGoal drops the kv row only while it still names the goal this
// window ran: a stale copy must not delete a row the writer's replacement
// wrote after this window's copy went old.
func (h *Host) unstoreGoal(g *Goal) {
	if h.DB == nil {
		return
	}
	if raw, err := h.DB.GetKV(goalKV); err == nil {
		var row Goal
		if json.Unmarshal([]byte(raw), &row) == nil && (row.SetAt != g.SetAt || row.Condition != g.Condition) {
			return
		}
	}
	h.storeGoal(nil)
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
	}
	ag := h.agents
	ag.mu.Lock()
	ag.goalPending = cond
	ag.mu.Unlock()
	err := h.sendCommandPrompt("goal", cond, attached, goalAttached(cond))
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
// never one that attached text (a plugin send, a file, a report) quotes, at
// the log seq the message was written at.
func (h *Host) setGoalFrom(seq int64, msg fantasy.Message) {
	ag := h.agents
	ag.mu.Lock()
	cond := ag.goalPending
	if cond == "" || !strings.Contains(concatText(msg.Content), goalAttached(cond)) {
		ag.mu.Unlock()
		return
	}
	ag.goalPending = ""
	old := ag.goal
	g := &Goal{Condition: cond, Since: time.Now(), SetAt: seq, Writer: h.origin, Tokens: ag.sessIn + ag.sessOut, Cost: ag.sessCost}
	ag.goal = g
	ag.mu.Unlock()
	h.storeGoal(g)
	if old != nil {
		h.Notify(slog.LevelInfo, "Replaced the goal", old.Condition)
	}
	h.refresh()
}

// goalMine reports whether this window runs g's chain: the window that set
// it, a row that predates the field, or a fresh boot of the same chat - the
// writer's heartbeat is gone, so one window takes the chain over instead of
// leaving a resumed goal with nobody to run it. Two live windows still never
// run it twice.
func (h *Host) goalMine(g *Goal) bool {
	if g.Writer == "" || g.Writer == h.origin {
		return true
	}
	if h.peersPath == "" {
		return true // presence is off (headless): the store's one window is ours
	}
	raw, err := os.Stat(filepath.Join(filepath.Dir(h.peersPath), g.Writer+".json"))
	return err != nil || time.Since(raw.ModTime()) > peersTTL
}

func (h *Host) stopGoal() error {
	ag := h.agents
	ag.mu.Lock()
	g := ag.goal
	ag.goal = nil
	ag.mu.Unlock()
	if g == nil {
		return Usagef("no goal to stop")
	}
	h.storeGoal(nil)
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "Goal stopped"})
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
		Items:  []OverlayItem{{Text: "Stop the goal", Run: "goal stop", Verb: VerbStop}},
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
// holds, draws the verdict's notice row and returns the note the next turn
// sends: "" ends the chain. An interrupt records nothing, and neither does a
// goal stopped or replaced meanwhile.
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
	if ev == goalMet {
		ag.goal = nil
		ag.mu.Unlock()
		h.unstoreGoal(g)
	} else {
		ag.mu.Unlock()
	}
	if m, ok := goalNotice(ev, reason); ok {
		h.AppendChat(m)
	}
	if ev != goalNotYet {
		return ""
	}
	why := "Not met yet."
	if reason != "" {
		why = "Not met yet: " + reason
	}
	return agent.Attached("goal", why+"\nKeep working until this holds: "+g.Condition)
}

// goalNotice is a goal event's live row; a set row has none.
func goalNotice(ev goalEvent, reason string) (ChatMessage, bool) {
	m := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "Goal " + string(ev)}
	switch ev {
	case goalSet:
		return m, false
	case goalNotYet:
		m.Text = "Goal not met yet"
	case goalBlocked:
		m.Detail = "/goal stop drops it"
	case goalMet, goalPaused, goalStopped:
	}
	if reason != "" {
		m.Text += ": " + sanitize.String(reason)
	}
	return m, true
}

// goalDropped drops the goal: the chat it was set in is gone (/reset).
func (h *Host) goalDropped() {
	ag := h.agents
	ag.mu.Lock()
	ag.goal = nil
	ag.mu.Unlock()
	// the row dies with the chat even when this window held none: a window
	// without the goal may be the one resetting the shared store
	h.storeGoal(nil)
}

// goalReason is a check's verdict as the goal row names it.
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
