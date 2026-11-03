package ui

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

var (
	mdCode    = regexp.MustCompile("`([^`\n]+)`")
	mdLink    = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdStrong  = regexp.MustCompile(`\*\*(\S(?:[^*\n]*\S)?)\*\*`)
	mdEm      = regexp.MustCompile(`\*(\S(?:[^*\n]*\S)?)\*`)
	mdHeading = regexp.MustCompile(`^#{1,6}\s+`)
	mdBullet  = regexp.MustCompile(`^\s*[*+-]\s+`)
)

// stripMarkdown turns a reply's markdown into the plain text a notification
// or the statusline can show: emphasis and code spans lose their marks,
// links keep their words, headings, bullets and fence lines go. Emphasis
// rules require a non-space edge so "2 * 3 * 4" keeps its asterisks.
func stripMarkdown(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			continue
		}
		t = mdHeading.ReplaceAllString(t, "")
		t = mdBullet.ReplaceAllString(t, "")
		lines = append(lines, t)
	}
	s = mdCode.ReplaceAllString(strings.Join(lines, "\n"), "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdStrong.ReplaceAllString(s, "$1")
	return mdEm.ReplaceAllString(s, "$1")
}

// count is "3 workflows": the number and its noun, singular or plural.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// lower is text with its first letter lowercased so it reads on after a verb,
// unless that letter starts an acronym ("README").
func lower(s string) string {
	r := []rune(s)
	if len(r) >= 2 && isUpper(r[0]) && isUpper(r[1]) {
		return s
	}
	if len(r) > 0 && isUpper(r[0]) {
		return string(append([]rune{toLower(r[0])}, r[1:]...))
	}
	return s
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func toLower(r rune) rune {
	if isUpper(r) {
		return r + ('a' - 'A')
	}
	return r
}

// segsCells is the cells a line of segments takes.
func segsCells(segs []hostapi.Segment) int { return hostapi.SegsWidth(segs) }

// clipText cuts text to room cells, ending in an ellipsis when it cuts, never
// after a space and never inside a word short enough to drop whole.
func clipText(text string, room int) string {
	return hostapi.SegsText(ellipsizeSegs([]hostapi.Segment{{Text: text}}, room))
}

// tailText cuts a path wider than w cells to its tail, where its name is.
func tailText(path string, w int) string {
	if ansi.StringWidth(path) <= w || w <= 1 {
		return path
	}
	return hostapi.SegsText(tailSegs([]hostapi.Segment{{Text: path}}, w))
}

// running is the work running besides this turn as one string, nil when
// nothing runs.
func running(st *hostapi.ScreenState) string {
	if st.WorkflowsRunning == 0 && st.AgentsRunning == 0 && st.JobsRunning == 0 {
		return ""
	}
	var parts []string
	if st.WorkflowsRunning > 0 {
		parts = append(parts, count(st.WorkflowsRunning, "workflow", "workflows"))
	}
	if st.AgentsRunning > 0 {
		parts = append(parts, count(st.AgentsRunning, "agent", "agents"))
	}
	if st.JobsRunning > 0 {
		parts = append(parts, count(st.JobsRunning, "job", "jobs"))
	}
	return strings.Join(parts, ", ")
}

// pendingLine is what the session still waits on as one row: other work, and
// the jobs the user waits on. Its labels breathe, so the row ticks the frame
// clock while it lives; nil when nothing pends.
func pendingLine(st *hostapi.ScreenState) []hostapi.Segment {
	var parts []string
	// the live rail already names the run the turn waits on: counts are other work
	if !st.AgentActive {
		if runs := running(st); runs != "" {
			parts = append(parts, runs)
		}
	}
	if n := len(st.Busy); n > 0 {
		// one part however many run: the oldest's label, which a new job
		// joining does not move, and the rest of the fleet counted
		label := lower(st.Busy[n-1].Label)
		if n > 1 {
			label = fmt.Sprintf("%s, and %d more", label, n-1)
		}
		parts = append(parts, label)
	}
	if len(parts) == 0 {
		return nil
	}
	bg := hostapi.Marks["bg"]
	row := []hostapi.Segment{{Text: bg.Glyph + " ", HL: bg.HL, NoCopy: true, NoHover: true}}
	for i, part := range parts {
		if i > 0 {
			row = append(row, hostapi.Segment{Text: "  ", HL: "Dim", NoCopy: true, NoHover: true})
		}
		row = append(row, hostapi.Segment{Text: part, HL: "Dim", Effect: "shimmer"})
	}
	return row
}

// emptyChat says whether the splash owns the chat this frame: an empty
// transcript with no turn running and no todo left.
func emptyChat(st *hostapi.ScreenState) bool {
	if st.AgentActive || st.TranscriptCount > 0 {
		return false
	}
	for _, td := range st.Todos() {
		if td.Status != "done" {
			return false
		}
	}
	return true
}

// runClick is the shipped widgets' on_click: a click id is a command and its
// arguments ("model gpt"), or "fill <text>", which puts the text in the
// composer for the user to send. Runs on the UI goroutine.
func (a *App) runClick(ev map[string]any) {
	id, _ := ev["id"].(string)
	if fill, ok := strings.CutPrefix(id, "fill "); ok {
		a.ComposerEdit(hostapi.ComposerSet, fill, -1)
		return
	}
	if name, args := hostapi.SplitCommand(id); name != "" {
		a.runCommand(name, args, false)
	}
}

// tokens is a token count as "812", "24.5k", "1.2M" or "1.5B"; "" for none.
func tokens(n int64) string {
	if n <= 0 {
		return ""
	}
	var s string
	switch {
	case n >= 1_000_000_000:
		s = fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		s = fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		s = fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		s = strconv.FormatInt(n, 10)
	}
	return strings.NewReplacer(".0k", "k", ".0M", "M", ".0B", "B").Replace(s)
}

// costText is a cost in dollars as "$0.42", "<$0.01" under half a cent; ""
// for none (an unpriced model).
func costText(usd float64) string {
	if usd <= 0 {
		return ""
	}
	if usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// duration is a duration as "4.2s" under ten seconds, then "42s", "3m 05s",
// "1h 02m". A clock that ticks passes whole: its seconds only grow in width.
func duration(s float64, whole bool) string {
	if s < 9.95 && !whole {
		return fmt.Sprintf("%.1fs", max(s, 0))
	}
	n := int(math.Floor(max(s, 0) + 0.5))
	if whole {
		n = int(math.Floor(max(s, 0)))
	}
	switch {
	case n < 60:
		return strconv.Itoa(n) + "s"
	case n < 3600:
		return fmt.Sprintf("%dm %02ds", n/60, n%60)
	}
	return fmt.Sprintf("%dh %02dm", n/3600, n/60%60)
}

// clockText is a wall clock, "14:05".
func clockText(t time.Time) string { return t.Format("15:04") }

// pointAt is a pointer to what deals with something: the noun in Title (hl
// overrides it), then the command or key in Dim. Both segments click as
// click, a command's own name when click is left out.
func pointAt(noun, cmd, click string) []hostapi.Segment {
	if click == "" && strings.HasPrefix(cmd, "/") && !strings.Contains(cmd[1:], " ") {
		click = cmd[1:]
	}
	return []hostapi.Segment{{Text: noun, HL: "Title", Click: click}, {Text: " " + cmd, HL: "Dim", Click: click}}
}
