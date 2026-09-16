// Package win hosts programs inside zemi: each window is a pseudo-terminal
// running a command, fed to a pure Go terminal emulator (charmbracelet/x/vt)
// whose screen the UI draws in a split or a float. Windows outlive turns
// and config reloads; the UI decides where they go, the Manager only knows
// what they run and what they show. No cgo: the pty is creack/pty.
package win

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/metafates/zemi/internal/proc"
)

// Kinds are where a window is laid out.
const (
	KindSplit  = "split"  // below the chat
	KindVSplit = "vsplit" // beside the chat
	KindFloat  = "float"  // over the chat
)

// Spec is what opens a window.
type Spec struct {
	Cmd   string // run with sh -c, so pipes and quoting work as in bash
	Dir   string
	Env   []string
	Kind  string // KindSplit, KindVSplit or KindFloat
	Title string
	Cols  int // requested size, 0 = the layout's choice
	Rows  int
	Owner string // who opened it: "model", "user" or "lua"
}

// Layout is where and how big a window shows; the UI reads it every frame
// and the user, Lua and the model change it.
type Layout struct {
	Kind   string
	Hidden bool    // keeps running, takes no space
	Frac   float64 // split, vsplit: share of the axis, 0 = an equal share
	Cols   int     // float: requested size, 0 = four fifths of the area
	Rows   int
	X, Y   int // float: top-left cell of the frame, -1 = centered
}

// Window is one running program and its screen.
type Window struct {
	ID    int
	Cmd   string
	Owner string

	lmu    sync.Mutex
	layout Layout

	title atomic.Pointer[string] // the program may rename itself (OSC 0/2)

	mu   sync.Mutex // guards term, w, h
	term *vt.Emulator
	w, h int

	ptmx         *os.File
	cmd          *exec.Cmd
	dirty        atomic.Bool
	cursorHidden atomic.Bool
	mouseOn      atomic.Bool // the program asked for mouse reports
	done         chan struct{}
	exit         atomic.Int32 // exit code, -1 while running
	err          atomic.Pointer[string]
	once         sync.Once

	started time.Time
}

// Span is a run of cells sharing one style; SGR holds the parameters of
// the sequence that reproduces it ("" for plain text).
type Span struct {
	Text string
	SGR  string
}

// Screen is a rendered window: one span row per terminal row, and the
// cursor when the program shows one.
type Screen struct {
	Rows      [][]Span
	CursorX   int
	CursorY   int
	HasCursor bool
}

// Manager owns every open window.
type Manager struct {
	mu     sync.Mutex
	wins   []*Window
	seq    int
	OnExit func(w *Window) // runs on the wait goroutine after the program exits
}

const (
	defaultCols = 80
	defaultRows = 24
	maxWindows  = 8
)

// Open starts spec's command in a new pseudo-terminal. The window starts at
// the requested or default size; the UI resizes it when it lays it out.
func (m *Manager) Open(spec Spec) (*Window, error) {
	if strings.TrimSpace(spec.Cmd) == "" {
		return nil, errors.New("window: command is required")
	}
	switch spec.Kind {
	case KindSplit, KindVSplit, KindFloat:
	case "":
		spec.Kind = KindSplit
	default:
		return nil, fmt.Errorf("window: kind %q is not split, vsplit or float", spec.Kind)
	}
	m.mu.Lock()
	if len(m.wins) >= maxWindows {
		m.mu.Unlock()
		return nil, fmt.Errorf("window: %d windows are open, close one first", maxWindows)
	}
	m.seq++
	id := m.seq
	m.mu.Unlock()

	cols, rows := spec.Cols, spec.Rows
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}
	w := &Window{
		ID: id, Cmd: spec.Cmd, Owner: spec.Owner,
		layout: Layout{Kind: spec.Kind, Cols: spec.Cols, Rows: spec.Rows, X: -1, Y: -1},
		term:   vt.NewEmulator(cols, rows), w: cols, h: rows,
		done: make(chan struct{}), started: time.Now(),
	}
	w.term.SetScrollbackSize(2000)
	w.exit.Store(-1)
	w.SetTitle(spec.Title)
	if spec.Title == "" {
		w.SetTitle(spec.Cmd)
	}
	// callbacks run inside term.Write, under mu: they must not take it
	w.term.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { w.cursorHidden.Store(!visible) },
		EnableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				w.mouseOn.Store(true)
			}
		},
		DisableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				w.mouseOn.Store(false)
			}
		},
		Title: func(t string) {
			if strings.TrimSpace(t) != "" {
				w.SetTitle(t)
			}
		},
	})
	cmd := exec.Command("sh", "-c", spec.Cmd)
	cmd.Dir = spec.Dir
	cmd.Env = append(append([]string{}, spec.Env...), "TERM=xterm-256color", "COLORTERM=truecolor")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("window: %w", err)
	}
	w.ptmx, w.cmd = ptmx, cmd
	proc.Track(cmd)
	m.mu.Lock()
	m.wins = append(m.wins, w)
	m.mu.Unlock()

	go w.readLoop()
	go w.replyLoop()
	go func() {
		err := proc.Wait(cmd)
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			w.exit.Store(int32(ee.ExitCode()))
		case err != nil:
			s := err.Error()
			w.err.Store(&s)
			w.exit.Store(-2)
		default:
			w.exit.Store(0)
		}
		// give the last output a moment to land on the screen
		time.Sleep(30 * time.Millisecond)
		// under mu, like Resize's ioctl: a closed fd number can be reused
		w.mu.Lock()
		_ = ptmx.Close()
		w.mu.Unlock()
		close(w.done)
		w.dirty.Store(true)
		if m.OnExit != nil {
			m.OnExit(w)
		}
	}()
	return w, nil
}

// readLoop feeds the program's output to the emulator.
func (w *Window) readLoop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := w.ptmx.Read(buf)
		if n > 0 {
			w.mu.Lock()
			_, _ = w.term.Write(buf[:n])
			w.mu.Unlock()
			w.dirty.Store(true)
		}
		if err != nil {
			return
		}
	}
}

// replyLoop carries the emulator's answers (cursor reports, device
// attributes) and the keys it encodes back to the program. It drains until
// kill closes the pipe, even once the program is gone: the emulator writes
// to that unbuffered pipe under mu, so a loop that stopped would wedge mu.
func (w *Window) replyLoop() {
	buf := make([]byte, 4<<10)
	for {
		n, err := w.term.Read(buf)
		if n > 0 {
			_, _ = w.ptmx.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// Get returns a window by id.
func (m *Manager) Get(id int) (*Window, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.wins {
		if w.ID == id {
			return w, true
		}
	}
	return nil, false
}

// List returns the open windows in opening order.
func (m *Manager) List() []*Window {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Window(nil), m.wins...)
}

// Close ends a window: the program is killed if still running, and the
// window leaves the list.
func (m *Manager) Close(id int) bool {
	m.mu.Lock()
	var w *Window
	for i, c := range m.wins {
		if c.ID == id {
			w = c
			m.wins = append(m.wins[:i], m.wins[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if w == nil {
		return false
	}
	w.kill()
	return true
}

// CloseAll ends every window (quit).
func (m *Manager) CloseAll() {
	for _, w := range m.List() {
		m.Close(w.ID)
	}
}

// Dirty reports whether any window drew since the last call and clears
// the marks; the UI polls it once per frame.
func (m *Manager) Dirty() bool {
	dirty := false
	for _, w := range m.List() {
		if w.dirty.Swap(false) {
			dirty = true
		}
	}
	return dirty
}

func (w *Window) kill() {
	w.once.Do(func() {
		if !w.Exited() {
			proc.Hangup(w.cmd)
			select {
			case <-w.done:
			case <-time.After(300 * time.Millisecond):
				proc.Kill(w.cmd)
			}
		}
		// end replyLoop by closing the input pipe: Emulator.Close sets a flag
		// that Emulator.Read checks without a lock
		_ = w.term.InputPipe().(io.Closer).Close()
	})
}

// Layout is a snapshot of where the window shows.
func (w *Window) Layout() Layout {
	w.lmu.Lock()
	defer w.lmu.Unlock()
	return w.layout
}

// SetLayout changes the layout under the lock; kind and negative sizes are
// validated so a bad set never breaks the frame.
func (w *Window) SetLayout(fn func(*Layout)) error {
	w.lmu.Lock()
	defer w.lmu.Unlock()
	l := w.layout
	fn(&l)
	switch l.Kind {
	case KindSplit, KindVSplit, KindFloat:
	default:
		return fmt.Errorf("window: kind %q is not split, vsplit or float", l.Kind)
	}
	l.Frac = max(0, min(l.Frac, 0.9))
	l.Cols, l.Rows = max(0, l.Cols), max(0, l.Rows)
	l.X, l.Y = max(-1, l.X), max(-1, l.Y)
	w.layout = l
	return nil
}

// Kind is the layout kind.
func (w *Window) Kind() string { return w.Layout().Kind }

// Hidden reports whether the window is off screen.
func (w *Window) Hidden() bool { return w.Layout().Hidden }

// Focus tells the program it gained or lost focus, when it asked to know.
func (w *Window) Focus(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on {
		w.term.Focus()
	} else {
		w.term.Blur()
	}
}

// ScrollbackText is the last n lines that scrolled off the top.
func (w *Window) ScrollbackText(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	sb := w.term.Scrollback()
	total := sb.Len()
	var lines []string
	for i := max(0, total-n); i < total; i++ {
		var b strings.Builder
		for _, c := range sb.Line(i) {
			b.WriteString(c.Content)
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// Title is the window title: the spec's, or what the program set.
func (w *Window) Title() string {
	if t := w.title.Load(); t != nil {
		return *t
	}
	return ""
}

// SetTitle renames the window.
func (w *Window) SetTitle(t string) { w.title.Store(&t) }

// Exited reports whether the program ended.
func (w *Window) Exited() bool { return w.exit.Load() != -1 }

// Done is closed when the program exits.
func (w *Window) Done() <-chan struct{} { return w.done }

// Wait blocks until the program exits or ctx ends.
func (w *Window) Wait(ctx context.Context) error {
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ExitCode is the exit status: -1 while running, -2 when the process could
// not be waited for (Err says why).
func (w *Window) ExitCode() int { return int(w.exit.Load()) }

// Err is the wait error, "" when there was none.
func (w *Window) Err() string {
	if s := w.err.Load(); s != nil {
		return *s
	}
	return ""
}

// Elapsed is how long the program has run.
func (w *Window) Elapsed() time.Duration { return time.Since(w.started) }

// Resize sets the terminal size (the pty and the emulator); a no-op when
// unchanged. Callers pass the cells the layout gives the window.
func (w *Window) Resize(cols, rows int) {
	if cols < 2 || rows < 1 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if cols == w.w && rows == w.h {
		return
	}
	w.w, w.h = cols, rows
	w.term.Resize(cols, rows)
	_ = pty.Setsize(w.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	w.dirty.Store(true)
}

// Size is the current terminal size.
func (w *Window) Size() (cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w, w.h
}

// SendText types text into the program, as if pasted without bracketing.
func (w *Window) SendText(text string) error {
	if w.Exited() {
		return errors.New("window: the program has exited")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.term.SendText(text)
	return nil
}

// SendKey types one key, encoded the way the program asked for (cursor
// key mode and the like). Printable keys go as their text so shifted and
// composed characters survive; modified special keys go in xterm form.
func (w *Window) SendKey(k uv.KeyPressEvent) {
	if w.Exited() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sendKey(k)
}

func (w *Window) sendKey(k uv.KeyPressEvent) {
	switch seq := modifiedSpecial(k); {
	case k.Text != "" && k.Mod&(uv.ModCtrl|uv.ModAlt|uv.ModMeta) == 0:
		w.term.SendText(k.Text)
	case seq != "":
		w.term.SendText(seq)
	default:
		w.term.SendKey(k)
	}
}

// SendKeys types keys written in neovim notation (ParseKeys).
func (w *Window) SendKeys(notation string) error {
	keys, err := ParseKeys(notation)
	if err != nil {
		return err
	}
	if w.Exited() {
		return errors.New("window: the program has exited")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range keys {
		w.sendKey(k)
	}
	return nil
}

// WaitText polls until the screen contains want or ctx ends; it returns
// the screen text either way and whether want appeared.
func (w *Window) WaitText(ctx context.Context, want string) (string, bool) {
	for {
		text := w.Text()
		if strings.Contains(text, want) {
			return text, true
		}
		select {
		case <-ctx.Done():
			return w.Text(), false
		case <-w.done:
			text = w.Text()
			return text, strings.Contains(text, want)
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func isMouseMode(m ansi.Mode) bool {
	switch m {
	case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		return true
	}
	return false
}

// SendMouse forwards a mouse event at window coordinates. A program that
// never asked for the mouse gets the wheel as arrow keys on the alternate
// screen, the way terminals scroll a pager.
func (w *Window) SendMouse(ev uv.MouseEvent) {
	if w.Exited() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mouseOn.Load() {
		w.term.SendMouse(ev)
		return
	}
	wheel, ok := ev.(uv.MouseWheelEvent)
	if !ok || !w.term.IsAltScreen() {
		return
	}
	key := uv.KeyPressEvent{Code: uv.KeyDown}
	if wheel.Button == uv.MouseWheelUp {
		key.Code = uv.KeyUp
	}
	for range 3 {
		w.term.SendKey(key)
	}
}

// Paste types text with bracketing when the program asked for it.
func (w *Window) Paste(text string) {
	if w.Exited() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.term.Paste(text)
}

// Screen renders the current screen as styled spans.
func (w *Window) Screen() Screen {
	w.mu.Lock()
	defer w.mu.Unlock()
	cols, rows := w.w, w.h
	out := Screen{Rows: make([][]Span, rows)}
	for y := range rows {
		var row []Span
		var cur *Span
		var curStyle uv.Style
		for x := range cols {
			c := w.term.CellAt(x, y)
			if c == nil {
				continue
			}
			text := c.Content
			if text == "" {
				continue // the tail of a wide cell
			}
			if cur == nil || !c.Style.Equal(&curStyle) {
				row = append(row, Span{SGR: sgrParams(c.Style)})
				cur = &row[len(row)-1]
				curStyle = c.Style
			}
			cur.Text += text
		}
		out.Rows[y] = row
	}
	pos := w.term.CursorPosition()
	out.CursorX, out.CursorY = pos.X, pos.Y
	out.HasCursor = !w.cursorHidden.Load()
	return out
}

// Text is the screen as plain text, trailing blanks trimmed.
func (w *Window) Text() string {
	scr := w.Screen()
	lines := make([]string, len(scr.Rows))
	for i, row := range scr.Rows {
		var b strings.Builder
		for _, s := range row {
			b.WriteString(s.Text)
		}
		lines[i] = strings.TrimRight(b.String(), " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// sgrParams renders a cell style as SGR parameters.
func sgrParams(s uv.Style) string {
	var st ansi.Style
	if s.Attrs&uv.AttrBold != 0 {
		st = st.Bold()
	}
	if s.Attrs&uv.AttrFaint != 0 {
		st = st.Faint()
	}
	if s.Attrs&uv.AttrItalic != 0 {
		st = st.Italic(true)
	}
	if s.Attrs&uv.AttrBlink != 0 {
		st = st.Blink(true)
	}
	if s.Attrs&uv.AttrReverse != 0 {
		st = st.Reverse(true)
	}
	if s.Attrs&uv.AttrStrikethrough != 0 {
		st = st.Strikethrough(true)
	}
	if s.Underline != uv.UnderlineNone {
		st = st.UnderlineStyle(s.Underline)
	}
	if s.UnderlineColor != nil {
		st = st.UnderlineColor(s.UnderlineColor)
	}
	if s.Fg != nil {
		st = st.ForegroundColor(s.Fg)
	}
	if s.Bg != nil {
		st = st.BackgroundColor(s.Bg)
	}
	if len(st) == 0 {
		return ""
	}
	return strings.Join(st, ";")
}
