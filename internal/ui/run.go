package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"

	"github.com/metafates/zemi/internal/hostapi"
)

// printLines writes lines as SGR text for a shell: foreground only, since
// the shell's ground is not the theme's canvas, and a foreground equal to
// Normal's is left to the terminal, so a dark theme's text reads on a light
// one. The profile drops what the terminal cannot show (NO_COLOR keeps bold).
func printLines(w io.Writer, t themeResolver, p colorprofile.Profile, lines [][]hostapi.Segment) {
	var b strings.Builder
	normal := t.style("Normal").Fg
	for _, line := range lines {
		for _, s := range line {
			st := t.style(s.HL)
			st.Bg = nil
			if sameColor(st.Fg, normal) {
				st.Fg = nil
			}
			if p == colorprofile.ANSI {
				st.Fg = ansi16(st.Fg)
			}
			st = uv.ConvertStyle(st, p)
			b.WriteString(st.Styled(s.Text))
		}
		b.WriteString("\n")
	}
	_, _ = io.WriteString(w, b.String())
}

// RunView prints a zemi run the way the chat draws it: settled rows once,
// and on a terminal a live tail (the pending rows, the live turn and the
// working rule) redrawn in place under them. It is the run's stderr, so a
// diagnostic prints above the tail.
// NOTE: a resize rewraps the tail the terminal already drew, so the first
// redraw after it can leave a stray row in scrollback, and keys typed during
// a run echo into the tail.
type RunView struct {
	w       io.Writer
	tty     bool                       // w is a terminal
	size    func() (width, height int) // nil when no cursor moves on w: no tail
	profile colorprofile.Profile
	quiet   bool // -q or --json: diagnostics only
	now     func() time.Time

	// the run loop's alone: renders wait on the Lua actor, which may be
	// writing a diagnostic, so none runs under mu
	h       *hostapi.Host
	thm     themeResolver
	printed bool // settled rows went out, so the next batch starts with a gap
	dead    bool // a render failed or Close ran: the view draws nothing more

	mu   sync.Mutex // held over each write to w
	tail string     // the tail on screen, "" for none
}

// NewRunView prints to f; quiet prints diagnostics only.
func NewRunView(f *os.File, quiet bool) *RunView {
	v := &RunView{w: f, tty: term.IsTerminal(f.Fd()), profile: colorprofile.Detect(f, os.Environ()), quiet: quiet, now: time.Now}
	if v.tty && os.Getenv("TERM") != "dumb" {
		v.size = func() (int, int) {
			w, h, err := term.GetSize(f.Fd())
			if err != nil || w <= 0 {
				return 100, 30
			}
			return w, h
		}
	}
	return v
}

// Start draws h's run from now on; before it the view only forwards writes.
// Start, On, Show and Close run on the run loop only.
func (v *RunView) Start(h *hostapi.Host) {
	v.h, v.thm = h, themeResolver{host: h, sgr: map[string]uv.Style{}}
}

// On reports whether the view draws (not quiet, started, no render failed
// and not closed) and whether it draws to a terminal.
func (v *RunView) On() (on, terminal bool) {
	return v.on(), v.on() && v.tty
}

func (v *RunView) on() bool { return !v.quiet && !v.dead && v.h != nil }

// Show prints fr's settled rows above the tail and redraws the tail.
func (v *RunView) Show(fr hostapi.RunFrame, cancelling bool) {
	if !v.on() {
		return
	}
	width, height := v.dims()
	var b strings.Builder
	if len(fr.Settled) > 0 {
		lines, err := v.region("chat", width, height, hostapi.UIState{Printed: &hostapi.Printed{Rows: fr.Settled}})
		if err != nil {
			v.fail(err)
			return
		}
		if v.printed && len(lines) > 0 {
			lines = append([][]hostapi.Segment{{}}, lines...)
		}
		v.printed = v.printed || len(lines) > 0
		printLines(&b, v.thm, v.profile, lines)
	}
	var tail string
	if v.size != nil {
		var err error
		if tail, err = v.drawTail(fr, cancelling, width, height); err != nil {
			v.fail(err)
			return
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if b.Len() == 0 && tail == v.tail {
		return
	}
	v.redraw(b.String(), tail)
}

// drawTail is the tail's text: the chat's pending rows and live turn, cut
// to their last rows so a redraw stays off the top of the screen, a blank
// row, and the working rule. No trailing newline, so it fills no extra row,
// and two cells short of the edge, so the ^C a terminal echoes after it does
// not wrap to a row the next erase would miss.
func (v *RunView) drawTail(fr hostapi.RunFrame, cancelling bool, width, height int) (string, error) {
	width -= 2
	state := hostapi.UIState{Printed: &hostapi.Printed{Rows: fr.Pending, Live: &fr.Stream}, Cancelling: cancelling}
	if v.h.Settings.Bool("ui.animations") {
		state.Frame = int(v.now().UnixMilli() / frameMS)
	}
	chat, err := v.region("chat", width, height, state)
	if err != nil {
		return "", err
	}
	chat = chat[max(0, len(chat)-max(4, height/2-2)):]
	rule, err := v.region("working", width, height, state)
	if err != nil {
		return "", err
	}
	if len(chat) > 0 {
		if v.printed { // the gap the chat puts between blocks
			chat = append([][]hostapi.Segment{{}}, chat...)
		}
		chat = append(chat, nil)
	}
	var b strings.Builder
	printLines(&b, v.thm, v.profile, append(chat, rule...))
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// redraw erases the tail, prints above what it covered, and draws tail in
// its place, in one synchronized write (a terminal without mode 2026
// ignores it). The cursor stays hidden while a tail is up. v.mu held.
func (v *RunView) redraw(above, tail string) {
	if v.size == nil { // a log gets the rows alone
		_, _ = io.WriteString(v.w, above)
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	v.erase(&b)
	b.WriteString(above)
	if tail != "" {
		b.WriteString("\x1b[?25l" + tail)
	}
	b.WriteString("\x1b[?2026l")
	_, _ = io.WriteString(v.w, b.String())
	v.tail = tail
}

// erase moves to the tail's first row and clears to the end of the screen.
func (v *RunView) erase(b *strings.Builder) {
	if v.tail == "" {
		return
	}
	b.WriteString("\r")
	if n := strings.Count(v.tail, "\n"); n > 0 {
		fmt.Fprintf(b, "\x1b[%dA", n)
	}
	b.WriteString("\x1b[J")
}

// Write prints a diagnostic above the tail; it is the host's Stderr for the
// whole run.
func (v *RunView) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tail == "" {
		return v.w.Write(p)
	}
	v.redraw(string(p), v.tail)
	return len(p), nil
}

// Close erases the tail, shows the cursor and prints the exit region: how
// to come back to the session.
func (v *RunView) Close() {
	var exit strings.Builder
	if v.on() {
		width, height := v.dims()
		lines, err := v.region(hostapi.RegionExit, width, height, hostapi.UIState{Printed: &hostapi.Printed{}})
		if err != nil {
			v.h.Log.Warn("exit widget failed", "origin", "run", "error", err)
		}
		printLines(&exit, v.thm, v.profile, lines)
	}
	v.dead = true
	v.mu.Lock()
	defer v.mu.Unlock()
	var b strings.Builder
	v.clearTail(&b)
	b.WriteString(exit.String())
	_, _ = io.WriteString(v.w, b.String())
}

// clearTail erases the tail and shows the cursor again. v.mu held.
func (v *RunView) clearTail(b *strings.Builder) {
	if v.tail != "" {
		v.erase(b)
		b.WriteString("\x1b[?25h")
		v.tail = ""
	}
}

// fail stops the view after a render error: the log gets it and stderr one
// line, and the reply goes to stdout since the view did not draw it.
func (v *RunView) fail(err error) {
	v.h.Log.Error("run view failed", "origin", "run", "error", err)
	v.dead = true
	v.mu.Lock()
	defer v.mu.Unlock()
	var b strings.Builder
	v.clearTail(&b)
	fmt.Fprintf(&b, "zemi: run view: %s\n", hostapi.Headline(err.Error()))
	_, _ = io.WriteString(v.w, b.String())
}

// Width is the columns a line gets before it wraps.
func (v *RunView) Width() int {
	w, _ := v.dims()
	return w
}

// dims is the terminal's size, or a plain log's width.
func (v *RunView) dims() (int, int) {
	if v.size == nil {
		return 100, 30
	}
	return v.size()
}

// region renders every widget of region as the layout would: a wrapping
// widget wrapped at width, the rest cut to it.
func (v *RunView) region(region string, width, height int, state hostapi.UIState) ([][]hostapi.Segment, error) {
	// a wedged runtime fails the view instead of holding the run
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out [][]hostapi.Segment
	for _, w := range v.h.Widgets.InRegion(region) {
		lines, _, err := v.h.RenderWidget(ctx, w, width, height, state)
		if err != nil {
			return nil, fmt.Errorf("widget %s: %w", w.ID, err)
		}
		for _, line := range lines {
			if w.Wrap {
				out = append(out, hostapi.WrapSegs(line, width)...)
				continue
			}
			out = append(out, truncateSegs(expandTabs([][]hostapi.Segment{line})[0], width))
		}
	}
	return out, nil
}
