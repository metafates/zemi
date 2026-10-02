package hostapi

import (
	"cmp"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
)

const (
	loopMin      = time.Minute      // the shortest interval, and the shortest delay the model may pick
	loopMaxEvery = 24 * time.Hour   // a longer interval belongs to cron and zemi run
	loopPaceMax  = time.Hour        // the longest delay the model may pick
	loopFallback = 20 * time.Minute // after a self-paced run that named no delay
	// NOTE: a busy agent delays a run by up to loopRetry; an idle hook is the
	// upgrade if that matters.
	loopRetry = 30 * time.Second
	// NOTE: 72h; a forgotten 1m loop is ~4k turns before it stops.
	loopExpiry = 72 * time.Hour
)

// loopTag opens a loop run's note; a turn whose text holds it is a loop run.
const loopTag = agent.AttachMarker + "\n<loop>\n"

// Loop is the live session's loop (/loop): a prompt sent again on a timer,
// or when the model says, while this process keeps the session open. It is
// never stored: a resumed session must not start spending on its own.
// NOTE: one loop per session; a list keyed by id is the upgrade.
type Loop struct {
	Prompt  string
	Every   time.Duration // 0 while the model paces it
	Started time.Time
	Next    time.Time // when the next run fires
	Runs    int
	Tokens  int64   // the session's tokens when it started
	Cost    float64 // and its cost
}

// Loop is the live session's loop, false for none.
func (h *Host) Loop() (Loop, bool) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.loop == nil {
		return Loop{}, false
	}
	return *ag.loop, true
}

// SetLoop runs /loop: alone it shows the loop, "stop" stops it, and
// "[interval] <prompt>" starts one, replacing any, with a first run now.
func (h *Host) SetLoop(args string) error {
	if _, err := h.Screen(); err != nil {
		return fmt.Errorf("%w; from a shell, run zemi run in a loop or from cron", err)
	}
	typed, _ := agent.CutAttached(args)
	prompt := strings.TrimSpace(typed)
	first, rest, _ := strings.Cut(prompt, " ")
	if n, ok := strings.CutSuffix(first, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil {
			first = strconv.Itoa(days*24) + "h"
		}
	}
	var every time.Duration
	if d, err := time.ParseDuration(first); err == nil {
		if d > loopMaxEvery {
			return Usagef("a loop runs every 1m to 24h; a longer wait belongs to cron and zemi run")
		}
		if d < loopMin {
			return Usagef("a loop runs every 1m to 24h")
		}
		every, prompt = d, strings.TrimSpace(rest)
	}
	name, _ := SplitCommand(strings.TrimPrefix(prompt, "/"))
	switch {
	case prompt == "" && every == 0:
		return h.openLoop()
	case prompt == "stop" && every == 0:
		return h.stopLoop(nil, "Loop stopped")
	case prompt == "":
		return Usagef("/loop 5m <prompt> runs it every 5m; /loop <prompt> lets the model pace it")
	case strings.HasPrefix(prompt, "/") && (name == "loop" || name == "goal"):
		return Usagef("a loop cannot run /%s", name)
	}
	now := h.RT.Clock().Now()
	ag := h.agents
	ag.mu.Lock()
	old := ag.loop
	l := &Loop{Prompt: prompt, Every: every, Started: now, Next: now, Tokens: ag.sessIn + ag.sessOut, Cost: ag.sessCost}
	if ag.loopStop != nil {
		ag.loopStop()
	}
	ag.loop, ag.loopStop = l, nil
	ag.mu.Unlock()
	if old != nil {
		h.Notify(slog.LevelInfo, "Replaced the loop", old.Prompt)
	}
	h.GoSafe("loop", func() { h.fireLoop(l) })
	return nil
}

// armLoop fires l after d (ag.mu held), in place of its armed timer. A
// retry keeps Next, so the pill keeps saying when the run was due.
func (h *Host) armLoop(l *Loop, d time.Duration, retry bool) {
	ag := h.agents
	if ag.loopStop != nil {
		ag.loopStop()
	}
	if !retry {
		l.Next = h.RT.Clock().Now().Add(d)
	}
	ag.loopStop = h.RT.Clock().AfterFunc(d, func() { h.GoSafe("loop", func() { h.fireLoop(l) }) })
}

// fireLoop sends l's next run, the way the user would: when the agent is
// busy it tries again loopRetry later, and a plain prompt claims the agent
// under the lock that saw it idle, so its run never steers into someone
// else's turn. The fallback timer is armed before the send, so the next run
// is always known; a self-paced run's reply may move it (loopTurnEnded).
// NOTE: runs are not jittered, and a command prompt claims nothing ahead.
func (h *Host) fireLoop(l *Loop) {
	ag := h.agents
	ag.mu.Lock()
	switch {
	case ag.loop != l: // stopped or replaced
		ag.mu.Unlock()
		return
	case h.RT.Clock().Now().Sub(l.Started) >= loopExpiry:
		ag.mu.Unlock()
		_ = h.stopLoop(l, fmt.Sprintf("Loop stopped after %s (%d %s)", fmtEvery(loopExpiry), l.Runs, Plural(l.Runs, "run", "runs")))
		return
	case ag.active:
		h.armLoop(l, loopRetry, true)
		ag.mu.Unlock()
		return
	}
	l.Runs++
	h.armLoop(l, cmp.Or(l.Every, loopFallback), false)
	note := loopNote(l)
	ag.loopRun = l
	cmd := strings.HasPrefix(l.Prompt, "/")
	var scope *turnScope
	if !cmd {
		scope = h.claim()
	}
	ag.mu.Unlock()
	h.refresh()
	var err error
	if cmd {
		// a command gets the note as its attachment, as it would piped stdin
		err = h.SendPrompt(l.Prompt + note)
	} else {
		err = h.sendClaimed(scope, note)
	}
	if err != nil {
		_ = h.stopLoop(l, "Loop stopped: "+h.friendlyError(err))
	}
}

func loopNote(l *Loop) string {
	next := "This runs again every " + fmtEvery(l.Every) + ". Say in a line or two what changed since the last run."
	if l.Every == 0 {
		next = "This runs again on its own. End your reply with a last line `next: <delay>` from 1m to 1h: short while something is about to change, long while nothing moves. End with `next: done` once the task is complete."
	}
	return agent.Attached("loop", fmt.Sprintf("Run %d of: %s\n%s", l.Runs, l.Prompt, next))
}

// loopTurnEnded runs after each turn: an interrupted turn nobody typed stops
// the loop (loopInterrupted), a run of the live loop that failed stops it,
// and a self-paced one's last line schedules the next run or ends the loop.
// Otherwise a run of a loop since replaced moves nothing.
func (h *Host) loopTurnEnded(runner *agent.Runner, text string, err error, cancelled bool) {
	own := strings.Contains(text, loopTag)
	ag := h.agents
	ag.mu.Lock()
	l, run := ag.loop, ag.loopRun
	if own {
		ag.loopRun = nil
	}
	ag.mu.Unlock()
	if typed, _ := agent.CutAttached(text); cancelled && (own || strings.TrimSpace(typed) == "") {
		h.loopInterrupted()
		return
	}
	if !own || cancelled || l == nil || l != run {
		return
	}
	runs := fmt.Sprintf("%d %s", l.Runs, Plural(l.Runs, "run", "runs"))
	switch {
	case err != nil:
		_ = h.stopLoop(l, "Loop stopped: "+h.friendlyError(err)+" ("+runs+")")
	case l.Every == 0:
		d, done, ok := parseNext(plainTextEnd(runner))
		if done {
			_ = h.stopLoop(l, "Loop done after "+runs)
			return
		}
		ag.mu.Lock()
		if ok && ag.loop == l {
			h.armLoop(l, d, false)
		}
		ag.mu.Unlock()
		h.refresh()
	}
}

// loopInterrupted stops the live loop after an interrupt of work nobody
// typed (a loop run, a goal's turn or check, a report's turn): esc means stop
// working on your own.
func (h *Host) loopInterrupted() {
	ag := h.agents
	ag.mu.Lock()
	l, n := ag.loop, 0
	if l != nil {
		n = l.Runs
	}
	ag.mu.Unlock()
	if l != nil {
		_ = h.stopLoop(l, fmt.Sprintf("Loop stopped: interrupted (%d %s)", n, Plural(n, "run", "runs")))
	}
}

// stopLoop stops l (nil: whichever runs) and says why in a notice row.
func (h *Host) stopLoop(l *Loop, why string) error {
	ag := h.agents
	ag.mu.Lock()
	if ag.loop == nil || l != nil && ag.loop != l {
		ag.mu.Unlock()
		return Usagef("no loop to stop")
	}
	if ag.loopStop != nil {
		ag.loopStop()
	}
	ag.loop, ag.loopStop = nil, nil
	ag.mu.Unlock()
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: why})
	return nil
}

func (h *Host) openLoop() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	l, ok := h.Loop()
	if !ok {
		return Usagef("no loop; /loop 5m <prompt> or /loop <prompt> starts one")
	}
	pace := "every " + fmtEvery(l.Every)
	if l.Every == 0 {
		pace = "when the model says"
	}
	stops := l.Started.Add(loopExpiry)
	ui.OpenOverlay(Overlay{
		ID: "loop", Title: "Loop", Origin: lua.OriginBuiltin, Fields: []Field{
			{"prompt", l.Prompt},
			{"next", l.Next.Format(ClockLayout) + ", then " + pace},
			{"ran", fmt.Sprintf("%d %s since %s, stops at %s", l.Runs, Plural(l.Runs, "time", "times"), l.Started.Format(ClockLayout), stops.Format(ClockLayout+" on Jan 2"))},
			{"spent", h.spentSince(l.Tokens, l.Cost)},
		},
		Items: []any{map[string]any{"text": "Stop the loop", "run": "loop stop", "verb": "stop"}},
	})
	return nil
}

// fmtEvery is an interval as /loop takes it: 5m, 1h30m, 90s.
func fmtEvery(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// parseNext reads the delay a self-paced run's reply ends with, clamped to
// loopMin..loopPaceMax; done when it ends the loop, ok false for no line.
func parseNext(reply string) (d time.Duration, done, ok bool) {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	last := strings.ToLower(strings.Trim(lines[len(lines)-1], "`* \t\r"))
	v, ok := strings.CutPrefix(last, "next:")
	if !ok {
		return 0, false, false
	}
	if v = strings.Trim(v, "`* "); v == "done" {
		return 0, true, true
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false, false
	}
	return min(max(d, loopMin), loopPaceMax), false, true
}
