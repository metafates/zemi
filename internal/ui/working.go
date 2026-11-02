package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// The composer frame's top rule, all st.Width cells of it: the mascot a cell
// off the left corner in the state of the turn and, at the right end of the
// rule, the interrupt key and the elapsed time. The block keeps a cell of rule
// on either side and ends a cell off the rule's end, so nothing to its right
// ever moves and the cells a part grows into come out of the rule on its left.
// The block stays away for the first 300ms so instant turns do not flash it,
// and the eyes say how the turn ended for a few seconds after. The chat's
// pending row rides the rule's middle once the reader scrolled it out of the
// view; an agent view names the run it shows there instead, and leaves on esc
// where the interrupt key stands.

// reads are the calls whose eyes say zemi is reading.
var reads = map[string]bool{"read": true, "grep": true, "glob": true, "skill": true}

// ruleState remembers between frames how many rows the chat drew below its
// pending row, which says whether that row left the view. Renders may overlap,
// so it takes a lock.
// NOTE: the count is the last chat render's on the package, not the frame's:
// a test reading it must not run parallel to a chat render.
var ruleState = struct {
	sync.Mutex
	after int
}{}

// setRowsAfterPending is the chat's report of the rows it drew below its
// pending row: the rule carries that row only once it left the view.
func setRowsAfterPending(n int) {
	ruleState.Lock()
	ruleState.after = n
	ruleState.Unlock()
}

// eyes is the snail's cycle against the state it is in: an eye cycle starts
// with its state, whatever the clock read. Renders may overlap, so it takes a
// lock.
type eyes struct {
	sync.Mutex
	shown string
	since int
}

// workingWidget is the rule above the composer.
func workingWidget() *widget {
	e := &eyes{}
	return &widget{id: "working", region: regionWorking, priority: 10, render: func(st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
		return workingRule(e, st)
	}}
}

// workingPart is one part of the rule with its worth, the cells it grows to,
// and whether the row centres it: a narrow rule sheds the least worth first,
// sized by what a part grows to, so a ticking clock never sheds one mid-turn.
type workingPart struct {
	line   []hostapi.Segment
	worth  int
	grow   int
	center bool
}

// plain is a part's row: its text in Dim.
func plain(text string) []hostapi.Segment {
	return []hostapi.Segment{{Text: text, HL: "Dim"}}
}

// ruleText is a spacer the rule draws, which copies as nothing.
func ruleText(text string) hostapi.Segment {
	return hostapi.Segment{Text: text, HL: "Rule", NoCopy: true}
}

// clock is the turn's clock, which reads as a clock and never as a countdown:
// the minutes pad, so "0m 48s" beside a bare "3s" window keeps the two apart.
func clock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// workingRule draws the row: the snail, the block, and the part the row
// centres, a cell of rule on either side of it.
func workingRule(e *eyes, st *hostapi.ScreenState) ([][]hostapi.Segment, bool) {
	ag := st.Agent
	ruleState.Lock()
	rowsAfter := ruleState.after
	ruleState.Unlock()
	var parts []workingPart
	state := "idle"
	key, verb := "esc", "interrupt"
	if st.InterruptArmed {
		verb = "again to interrupt"
	}
	// the run the chat shows and the key that leaves it, which is what esc
	// means there: the name takes the row's centre, the key the rule's right
	// end, and the key is worth more than the name so a narrow rule keeps it
	if st.View != nil {
		parts = append(parts, workingPart{line: []hostapi.Segment{{Text: st.View.Agent, HL: "Title"}}, worth: 5, center: true})
		parts = append(parts, workingPart{line: pointAt("esc", "back", ""), worth: 6, grow: 13})
	}
	// the chat's pending row, carried on the rule only once the reader
	// scrolled it out of the view: centred where a view's name stands and
	// drawn as the chat draws it, clickable nowhere
	if st.View == nil && st.RowsBelow > rowsAfter {
		if pending := pendingLine(st); pending != nil {
			parts = append(parts, workingPart{line: pending, worth: 4, center: true})
		}
	}
	var ticks []workingPart
	if !st.AgentActive {
		switch st.Ended {
		case hostapi.TurnOK:
			state = "done"
		case hostapi.TurnError:
			state = "error"
		}
	} else if ag.Elapsed >= 300*time.Millisecond || st.Cancelling {
		state = "working"
		// the eyes read the running call: serial calls run in call order, and a
		// parallel batch starts together
		running := false
		for _, t := range ag.Tools {
			if t.Status == hostapi.CallRun {
				running = true
				if reads[t.Name] {
					state = "reading"
				}
				break
			}
		}
		if !running && ag.Reasoning != "" && ag.Text == "" {
			state = "thinking"
		}
		if ag.CheckingGoal {
			state = "reading"
		}
		if st.Cancelling {
			state = "stopping"
		}
		ticks = []workingPart{{line: plain(clock(ag.Elapsed)), worth: 5}}
		if ag.Retry != nil {
			state = "error"
			// the reason and countdown outlast the key hint and the clock: a
			// narrow rule keeps the state the eyes alone cannot name
			next := "retrying now"
			if ag.Retry.NextIn > 0 {
				next = "next try in " + duration(ag.Retry.NextIn.Seconds(), true)
			}
			// the cause is clipped to the cells the countdown leaves, so a long
			// reason narrows instead of starving the whole rule of any state
			keep := max(st.Width-3-3-segsCells(plain(next))-8, 10)
			ticks = []workingPart{{line: plain(next), worth: 6}}
			if why := clipText(ag.Retry.Why, keep); why != "" {
				ticks = append(ticks, workingPart{line: plain(why), worth: 7})
			}
			ticks = append(ticks, workingPart{line: plain("elapsed " + clock(ag.Elapsed)), worth: 5})
		}
		if st.Editing != "" {
			state, ticks = "idle", nil
			parts = []workingPart{{line: plain("save and close the editor to continue"), worth: 5}}
		}
		// the open prompt names what answers it and esc answers it, so the rule
		// shrinks to the mascot alone rather than repeating a key
		if st.Waiting != "" {
			state, parts, ticks = "asking", nil, nil
		} else if st.Cancelling {
			parts = append(parts, workingPart{line: pointAt("ctrl+c", "quit", ""), worth: 6, grow: 13})
		} else if st.View == nil {
			parts = append(parts, workingPart{line: pointAt(key, verb, ""), worth: 6, grow: 13})
		}
		// last: the block ends on the clock, its digits eating the rule on the
		// left
		parts = append(parts, ticks...)
	}

	// the row holds the snail, the block and the part it centres, a cell of
	// rule on either side of it
	span := st.Width
	fit := func() bool {
		room, w, block := 0, 0, 0
		for _, p := range parts {
			cells := max(segsCells(p.line), p.grow)
			room += 3 + cells
			if p.center {
				w = segsCells(p.line) + 2
			} else {
				if block > 0 {
					block += 3
				}
				block += cells
			}
		}
		// the snail, its cells of rule and the block's own keep eight cells
		if room+8 > span {
			return false
		}
		if w == 0 {
			return true
		}
		// the snail, a cell of rule, the centred part, a cell, the block
		start := (span-w)/2 + 1
		return start >= 6 && start+w <= span-block-2
	}
	for len(parts) > 0 && !fit() {
		least := 0
		for i, p := range parts {
			if p.worth < parts[least].worth {
				least = i
			}
		}
		parts = append(parts[:least], parts[least+1:]...)
	}

	// the parts join as a footer joins its keys, each in the highlight it draws
	// in, a space between the block and the rule; the part the row centres
	// stands in the middle of the rule instead
	var after, mid []hostapi.Segment
	for _, p := range parts {
		if p.center {
			mid = p.line
			continue
		}
		if len(after) > 0 {
			after = append(after, hostapi.Segment{Text: " · ", HL: "Dim"})
		}
		after = append(after, p.line...)
	}
	// the block keeps a cell of rule on either side, as the statusline's
	// clusters do, and ends at the rule's last cell but one: the key and the
	// clock keep their right end whatever the clock reads and whatever a view
	// adds to the left
	block := 0
	if len(after) > 0 {
		block = segsCells(after) + 2
	}
	e.Lock()
	if state != e.shown {
		e.shown, e.since = state, st.Frame
	}
	frame := max(st.Frame-e.since, 0) // a clock gone back starts the cycle over
	e.Unlock()

	// the centred part at the row's middle, in the rule fit left for it
	w := 0
	if mid != nil {
		w = segsCells(mid) + 2
	}
	start := 0
	if w > 0 {
		start = (span-w)/2 + 1
	}
	row := []hostapi.Segment{ruleText(" ")}
	left := span - block - 1
	if mid != nil {
		left = start - 2
	}
	row = append(row, snailRule(state, frame, left)...)
	if mid != nil {
		row = append(row, ruleText(" "))
		row = append(row, mid...)
		row = append(row, ruleText(" "), ruleText(strings.Repeat("─", span-block-(start+w-1))))
	}
	if block > 0 {
		row = append(row, hostapi.Segment{Text: " ", HL: "Dim", NoCopy: true})
		row = append(row, after...)
		row = append(row, hostapi.Segment{Text: " ", HL: "Dim", NoCopy: true})
	}
	return [][]hostapi.Segment{row}, false
}

// The snail: a mascot whose shell is the "@", one cell off the rule's left
// end. Text only, and only the shell takes a color. It stays at the left end
// of the rule: the state shows in its eyes, stepped by the 50ms frame clock.
var snailHeads = map[string]struct {
	eyes  []string
	ticks int
}{
	"idle":     {[]string{"··"}, 1},
	"thinking": {[]string{".?", "?.", "..", "?.", ".?", ".."}, 8},
	"working":  {[]string{"··", ".·", "..", "·."}, 6},
	"stopping": {[]string{"·.", ".·", "..", "··"}, 6},
	"reading":  {[]string{"··", "..", "..", ".."}, 8},
	"asking":   {[]string{"??"}, 1},
	"error":    {[]string{"><"}, 1},
	"done":     {[]string{"^^"}, 1},
}

// snailEyes is the eyes of state, frame frames after it began.
func snailEyes(state string, frame int) string {
	heads, ok := snailHeads[state]
	if !ok {
		heads = snailHeads["idle"]
	}
	return heads.eyes[(frame/heads.ticks)%len(heads.eyes)]
}

// snailRule is the snail set a cell off a plain rule of room cells.
func snailRule(state string, frame, room int) []hostapi.Segment {
	head := snailEyes(state, frame)
	return []hostapi.Segment{
		{Text: "@", HL: "Snail", NoCopy: true},
		{Text: head, HL: "Normal", NoCopy: true},
		ruleText(" "),
		ruleText(strings.Repeat("─", max(room-2-ansiWidth(head), 0))),
	}
}
