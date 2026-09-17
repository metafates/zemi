// Package win hosts programs inside zemi: each window is a pseudo-terminal
// running a command, fed to a pure Go terminal emulator (charmbracelet/x/vt)
// whose screen the UI draws in a split or a float. Windows outlive turns
// and config reloads; the UI decides where they go, the Manager only knows
// what they run and what they show. No cgo: the pty is creack/pty.
// A text window (OpenText) runs nothing: it holds text the UI renders and
// re-wraps itself, replaced in place while it shows.
package win

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/sanitize"
)

// Kinds are where a window is laid out.
const (
	KindSplit  = "split"  // below the chat
	KindVSplit = "vsplit" // beside the chat
	KindFloat  = "float"  // over the chat
)

// Spec is what opens a window.
type Spec struct {
	Cmd    string   // run with sh -c, so pipes and quoting work as in bash
	Args   []string // $1... for Cmd
	Dir    string
	Env    []string
	Kind   string // KindSplit, KindVSplit or KindFloat
	Title  string
	Cols   int // requested size, 0 = the layout's choice
	Rows   int
	Owner  string // who opened it: "model", "agent:<name>", "user", "lua" or "editor"
	Origin string // the Lua origin that opened a "lua" window
	// Clipboard lets the program set the host clipboard with OSC 52; never for
	// a window the model drives, which could plant a command the user pastes
	Clipboard bool
	// Wrap confines the command before it starts; nil = unconfined
	Wrap func(*exec.Cmd) error
	// Text and Markdown are the body of a text window (Manager.OpenText),
	// which runs no program; Cmd, Args, Dir, Env and Wrap are unused there.
	Text     string
	Markdown bool
}

// Layout is where and how big a window shows; the UI reads it every frame
// and the user, Lua and the model change it.
type Layout struct {
	Kind   string
	Hidden bool    // keeps running, takes no space
	Frac   float64 // split, vsplit: share of its layout box's cells, 0 = a fill share
	Cols   int     // float: requested size, 0 = four fifths of the area
	Rows   int
	X, Y   int // float: top-left cell of the frame, -1 = centered
}

// body is a text window's content: no process, no emulator. The UI renders
// and wraps it every frame, so a replacement redraws in place and a resize
// re-wraps.
type body struct {
	mu     sync.Mutex
	text   string
	md     bool
	top    int  // first rendered line shown
	maxTop int  // the last top the UI could draw, from Top
	tail   bool // follow the end as the text grows
}

// Window is one running program and its screen.
type Window struct {
	ID     int
	Cmd    string
	Owner  string
	Origin string
	// Sandboxed says the program started under Spec.Wrap
	Sandboxed bool

	lmu    sync.Mutex
	layout Layout

	title atomic.Pointer[string] // the program may rename itself (OSC 0/2)

	m     *Manager
	body  *body      // text window: what the UI draws; nil for a program
	mu    sync.Mutex // guards term, w, h, place, up
	term  *vt.Emulator
	w, h  int
	place func()  // draws image placeholders after the graphics command
	up    *upload // a chunked image in progress
	gone  bool    // closed, so it may queue no images; guarded by m.gmu

	ptmx         *os.File
	inMu         sync.Mutex // guards in: input waiting for the program to read
	in           []byte
	inReady      chan struct{}
	cmd          *exec.Cmd
	cursorHidden atomic.Bool
	cursorStyle  int         // the last DECSCUSR parameter, 0 to 6; guarded by mu
	kittyKeys    [2]bool     // kitty keyboard flag 1 on the main and alt screen; guarded by mu
	syncSince    time.Time   // when the program began a synchronized update (mode 2026); guarded by mu
	last         Screen      // the last screen rendered, shown during that update; guarded by mu
	mouseOn      atomic.Bool // the program asked for mouse reports
	done         chan struct{}
	exit         atomic.Int32 // exit code, -1 while running
	once         sync.Once
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
	// CursorShape is 0 block, 1 underline, 2 bar: the order of tea.CursorShape
	CursorShape int
	CursorBlink bool
}

// Manager owns every open window.
type Manager struct {
	mu     sync.Mutex
	wins   []*Window
	seq    int
	OnExit func(w *Window) // runs on the wait goroutine after the program exits
	// colors answer a program's OSC 10, 11 and 12 queries: the host terminal's
	// foreground, background and cursor, nil until it reports them
	colors [3]color.Color

	kitty atomic.Bool // the host terminal draws kitty placeholders (kitty.go)
	clip  atomic.Pointer[string]
	// gmu guards the image state; lock order is w.mu, then gmu
	gmu   sync.Mutex
	gfx   strings.Builder
	slots [256]imgSlot // by host image id
	next  int          // the seq of the newest id taken
}

// hostTermEnv names the host terminal's identity. A window is its own
// terminal: with the host's identity image tools emit protocols the emulator
// drops (chafa: kitty, sixel, OSC 1337, tmux passthrough) and show nothing.
// NOTE: a prefix list; a terminal detected by a variable not listed slips through.
var hostTermEnv = []string{"TERM_PROGRAM", "LC_TERMINAL", "GHOSTTY_", "KITTY_", "WEZTERM_", "KONSOLE_", "ITERM_", "VTE_VERSION", "TERMINAL_NAME", "TMUX"}

const (
	defaultCols = 80
	defaultRows = 24
	maxWindows  = 8
	// maxClipboard caps an OSC 52 write, decoded
	maxClipboard = 1 << 20
)

// take checks the layout kind and reserves an id for a new window.
func (m *Manager) take(spec *Spec) (int, error) {
	switch spec.Kind {
	case KindSplit, KindVSplit, KindFloat:
	case "":
		spec.Kind = KindSplit
	default:
		return 0, fmt.Errorf("window: kind %q is not split, vsplit or float", spec.Kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// the editor is exempt: the user's ctrl+g must not fail because the model opened eight
	if spec.Owner != "editor" && len(slices.DeleteFunc(slices.Clone(m.wins), func(w *Window) bool { return w.Owner == "editor" })) >= maxWindows {
		return 0, fmt.Errorf("window: %d windows are open, close one first", maxWindows)
	}
	m.seq++
	return m.seq, nil
}

// OpenText opens a window whose body is text the UI renders (markdown when
// spec.Markdown), not a program: no pty, no emulator, nothing to exit.
// SetText replaces it in place.
func (m *Manager) OpenText(spec Spec) (*Window, error) {
	id, err := m.take(&spec)
	if err != nil {
		return nil, err
	}
	w := &Window{
		m: m, ID: id, Owner: spec.Owner, Origin: spec.Origin,
		layout: Layout{Kind: spec.Kind, Cols: spec.Cols, Rows: spec.Rows, X: -1, Y: -1},
		body:   &body{text: capBody(sanitize.String(spec.Text)), md: spec.Markdown},
		w:      cmp.Or(spec.Cols, defaultCols), h: cmp.Or(spec.Rows, defaultRows),
		done: make(chan struct{}),
	}
	w.exit.Store(-1)
	w.SetTitle(cmp.Or(spec.Title, "text"))
	m.mu.Lock()
	m.wins = append(m.wins, w)
	m.mu.Unlock()
	return w, nil
}

// IsText reports a window whose body is text, not a program.
func (w *Window) IsText() bool { return w.body != nil }

// errText refuses what only a program can take.
var errText = errors.New("window: this window shows text, not a program")

// Content is a text window's text and whether it renders as markdown.
func (w *Window) Content() (string, bool) {
	if w.body == nil {
		return "", false
	}
	w.body.mu.Lock()
	defer w.body.mu.Unlock()
	return w.body.text, w.body.md
}

// SetText replaces a text window's body, or adds to it when add is set. A
// replacement shows from the top; an append follows the end when the view
// was already there, so a growing log stays on its newest lines.
func (w *Window) SetText(text string, add bool) error {
	if w.body == nil {
		return errText
	}
	text = sanitize.String(text)
	w.body.mu.Lock()
	defer w.body.mu.Unlock()
	if add {
		text, w.body.tail = w.body.text+text, w.body.top >= w.body.maxTop
	} else {
		w.body.top, w.body.tail = 0, false
	}
	w.body.text = capBody(text)
	return nil
}

// capBody keeps the last maxBody bytes of a body: a trail a turn keeps
// appending to would grow without end, and the UI re-wraps the whole body
// when it changes. It drops the partial first line, or, when the tail holds
// no line break at all, the partial first rune.
func capBody(text string) string {
	if len(text) <= maxBody {
		return text
	}
	text = text[len(text)-maxBody:]
	if _, rest, ok := strings.Cut(text, "\n"); ok {
		text = rest
	}
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}

// maxBody caps a text window's body; appending past it drops the oldest
// lines, which is what a tail panel wants anyway.
const maxBody = 256 << 10

// maxScroll caps a text window's scroll offset, so end and a long run of
// page keys stay far from an integer overflow.
const maxScroll = 1 << 30

// Scroll moves a text window's view by lines; the UI clamps it to the
// rendered text when it draws. Scrolling to the end follows it again.
func (w *Window) Scroll(lines int) {
	if w.body == nil {
		return
	}
	w.body.mu.Lock()
	defer w.body.mu.Unlock()
	w.body.top = max(0, min(w.body.top+lines, maxScroll))
	w.body.tail = w.body.top >= w.body.maxTop
}

// Top is the first rendered line to draw, clamped to maxTop and kept
// there, so text that shrank scrolls back into view.
func (w *Window) Top(maxTop int) int {
	if w.body == nil {
		return 0
	}
	w.body.mu.Lock()
	defer w.body.mu.Unlock()
	w.body.maxTop = maxTop
	if w.body.tail {
		w.body.top = maxTop
	}
	w.body.top = max(0, min(w.body.top, maxTop))
	return w.body.top
}

// Open starts spec's command in a new pseudo-terminal. The window starts at
// the requested or default size; the UI resizes it when it lays it out.
func (m *Manager) Open(spec Spec) (*Window, error) {
	if strings.TrimSpace(spec.Cmd) == "" {
		return nil, errors.New("window: command is required")
	}
	id, err := m.take(&spec)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	colors := m.colors
	m.mu.Unlock()

	cols, rows := spec.Cols, spec.Rows
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}
	w := &Window{
		m: m, ID: id, Cmd: spec.Cmd, Owner: spec.Owner, Origin: spec.Origin, Sandboxed: spec.Wrap != nil,
		layout: Layout{Kind: spec.Kind, Cols: spec.Cols, Rows: spec.Rows, X: -1, Y: -1},
		term:   vt.NewEmulator(cols, rows), w: cols, h: rows,
		done: make(chan struct{}), inReady: make(chan struct{}, 1),
	}
	w.term.SetScrollbackSize(2000)
	// known modes report reset instead of unrecognized, so nvim wraps its redraws in 2026
	_, _ = w.term.Write([]byte(ansi.ResetModeSynchronizedOutput))
	w.setColors(colors)
	w.term.RegisterApcHandler(w.graphics)
	// vt keeps the cursor style per screen and has no getter; terminals keep one
	w.term.RegisterCsiHandler(ansi.Command(0, ' ', 'q'), func(p ansi.Params) bool {
		if n, _, _ := p.Param(0, 0); n <= 6 {
			w.cursorStyle = n
		}
		return false
	})
	w.term.RegisterEscHandler('c', func() bool { w.cursorStyle, w.kittyKeys = 0, [2]bool{}; return false })
	w.trackKittyKeys()
	if spec.Clipboard {
		w.term.RegisterOscHandler(52, m.setClipboard)
		// vt's DA1 plus 52: nvim uses OSC 52 only when a terminal lists it
		w.term.RegisterCsiHandler('c', func(p ansi.Params) bool {
			if n, _, _ := p.Param(0, 0); n != 0 {
				return false
			}
			_, _ = io.WriteString(w.term.InputPipe(), ansi.PrimaryDeviceAttributes(62, 1, 6, 22, 52))
			return true
		})
	}
	w.exit.Store(-1)
	w.SetTitle(spec.Title)
	if spec.Title == "" {
		w.SetTitle(spec.Cmd)
	}
	// callbacks run inside term.Write, under mu: they must not take it
	w.term.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { w.cursorHidden.Store(!visible) },
		AltScreen: func(on bool) {
			if on {
				w.kittyKeys[1] = false
			}
		},
		EnableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				w.mouseOn.Store(true)
			}
			if m == ansi.ModeSynchronizedOutput && w.syncSince.IsZero() {
				w.syncSince = time.Now()
			}
		},
		DisableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				w.mouseOn.Store(false)
			}
			if m == ansi.ModeSynchronizedOutput {
				w.syncSince = time.Time{}
			}
		},
		Title: func(t string) {
			if strings.TrimSpace(t) != "" && spec.Owner != "editor" {
				w.SetTitle(t)
			}
		},
	})
	cmd := exec.Command("sh", append([]string{"-c", spec.Cmd, "sh"}, spec.Args...)...)
	cmd.Dir = spec.Dir
	env := slices.DeleteFunc(slices.Clone(spec.Env), func(kv string) bool {
		return slices.ContainsFunc(hostTermEnv, func(p string) bool { return strings.HasPrefix(kv, p) })
	})
	if m.kitty.Load() {
		env = append(env, "TERM_PROGRAM=ghostty") // what image tools read as kitty graphics with placeholders
	}
	cmd.Env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	if spec.Wrap != nil {
		if err := spec.Wrap(cmd); err != nil {
			return nil, fmt.Errorf("window: the sandbox failed, so the command did not run: %w", err)
		}
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols), X: uint16(cols * cellW), Y: uint16(rows * cellH)})
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
	go w.writeLoop()
	go func() {
		err := proc.Wait(cmd)
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			w.exit.Store(int32(exitCode(ee)))
		case err != nil:
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
			w.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// write feeds output in pieces ending at a backslash, so image placeholders
// land right after the ST of their graphics command and before what follows.
// Writing them earlier, between ST's ESC and backslash, would garble both.
func (w *Window) write(p []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\\') + 1
		if i == 0 {
			i = len(p)
		}
		_, _ = w.term.Write(p[:i])
		if w.place != nil && p[i-1] == '\\' {
			w.place()
			w.place = nil
		}
		p = p[i:]
	}
}

// replyLoop carries the emulator's answers (cursor reports, device
// attributes) and the keys it encodes back to the program. The emulator
// writes to that unbuffered pipe under mu, so the loop only queues: it
// drains until kill closes the pipe, and writeLoop waits on the pty instead.
func (w *Window) replyLoop() {
	defer close(w.inReady)
	buf := make([]byte, 4<<10)
	for {
		n, err := w.term.Read(buf)
		if n > 0 {
			w.inMu.Lock()
			w.in = append(w.in, buf[:n]...)
			w.inMu.Unlock()
			select {
			case w.inReady <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

// writeLoop feeds queued input to the program; a write blocks while the
// program does not read stdin.
// NOTE: the queue is unbounded, so a program that never reads holds every
// paste sent to it in memory until the window closes.
func (w *Window) writeLoop() {
	for range w.inReady {
		w.inMu.Lock()
		b := w.in
		w.in = nil
		w.inMu.Unlock()
		_, _ = w.ptmx.Write(b)
	}
}

// SetHostColor records a color the host terminal reported, osc naming it
// (10 foreground, 11 background, 12 cursor), for open and later windows.
func (m *Manager) SetHostColor(osc int, c color.Color) {
	m.mu.Lock()
	m.colors[osc-10] = c
	colors, wins := m.colors, slices.Clone(m.wins)
	m.mu.Unlock()
	for _, w := range wins {
		w.mu.Lock()
		w.setColors(colors)
		w.mu.Unlock()
	}
}

// setColors sets the emulator's defaults; nil keeps vt's own.
func (w *Window) setColors(c [3]color.Color) {
	w.term.SetDefaultForegroundColor(c[0])
	w.term.SetDefaultBackgroundColor(c[1])
	w.term.SetDefaultCursorColor(c[2])
}

// setClipboard takes an OSC 52 write for the host clipboard. A read ("?") gets
// no answer: the clipboard may hold what a program's screen must not show.
func (m *Manager) setClipboard(data []byte) bool {
	_, rest, _ := bytes.Cut(data, []byte{';'})
	_, payload, _ := bytes.Cut(rest, []byte{';'})
	if base64.StdEncoding.DecodedLen(len(payload)) > maxClipboard {
		return true
	}
	if text, err := base64.StdEncoding.DecodeString(string(payload)); err == nil && len(text) > 0 {
		m.clip.Store(new(string(text)))
	}
	return true
}

// TakeClipboard returns the newest text a window set with OSC 52 since the
// last call, "" when none.
func (m *Manager) TakeClipboard() string {
	if p := m.clip.Swap(nil); p != nil {
		return *p
	}
	return ""
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
	m.dropImages(w)
	w.kill()
	return true
}

// CloseAll ends every window.
func (m *Manager) CloseAll() {
	for _, w := range m.List() {
		m.Close(w.ID)
	}
}

func (w *Window) kill() {
	w.once.Do(func() {
		if w.body != nil {
			w.exit.Store(0) // nothing ran, but a waiter is done waiting
			close(w.done)
			return
		}
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
	if w.body != nil {
		return
	}
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
	if w.body != nil {
		return ""
	}
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
		lines = append(lines, strings.TrimRight(StripPlaceholders(b.String()), " "))
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
// not be waited for, 128+signal when a signal ended it.
func (w *Window) ExitCode() int { return int(w.exit.Load()) }

// exitCode reads the status a signalled process ended with as the shell
// does, since ExitError.ExitCode is then -1, which marks a running window.
func exitCode(ee *exec.ExitError) int {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

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
	if w.body != nil {
		return // the UI re-wraps the text at the new width
	}
	w.term.Resize(cols, rows)
	_ = pty.Setsize(w.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols), X: uint16(cols * cellW), Y: uint16(rows * cellH)})
}

// Size is the current terminal size.
func (w *Window) Size() (cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w, w.h
}

// SendText types text into the program. Text with a line break in it goes
// in as a paste, bracketed when the program asked for that, or an editor's
// auto-indent mangles every line after the first; a trailing newline stays
// outside the brackets so it still submits.
func (w *Window) SendText(text string) error {
	if w.body != nil {
		return errText
	}
	if w.Exited() {
		return errors.New("window: the program has exited")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	body := strings.TrimRight(text, "\r\n")
	if !strings.ContainsAny(body, "\r\n") {
		w.term.SendText(text)
		return nil
	}
	w.term.Paste(body)
	w.term.SendText(text[len(body):])
	return nil
}

// SendKey types one key, encoded the way the program asked for (cursor
// key mode and the like). A text window scrolls instead.
func (w *Window) SendKey(k uv.KeyPressEvent) {
	if w.body != nil {
		w.scrollKey(k)
		return
	}
	if w.Exited() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sendKey(k)
}

// scrollKey moves a text window's view: arrows a line, page keys a screen,
// home and end the ends.
func (w *Window) scrollKey(k uv.KeyPressEvent) {
	_, rows := w.Size()
	page := max(rows-1, 1)
	switch k.Code {
	case uv.KeyUp:
		w.Scroll(-1)
	case uv.KeyDown:
		w.Scroll(1)
	case uv.KeyPgUp:
		w.Scroll(-page)
	case uv.KeyPgDown, uv.KeySpace:
		w.Scroll(page)
	case uv.KeyHome:
		w.Scroll(-maxScroll)
	case uv.KeyEnd:
		w.Scroll(maxScroll)
	}
}

func (w *Window) sendKey(k uv.KeyPressEvent) {
	seq := legacyKey(k)
	if w.kittyKeys[w.screen()] {
		seq = cmp.Or(kittyKey(k), seq)
	}
	if seq != "" {
		w.term.SendText(seq)
	} else {
		w.term.SendKey(k)
	}
}

// SendKeys types keys written in neovim notation (ParseKeys).
func (w *Window) SendKeys(notation string) error {
	keys, err := ParseKeys(notation)
	if err != nil {
		return err
	}
	if w.body != nil {
		for _, k := range keys {
			w.scrollKey(k)
		}
		return nil
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
// screen, the way terminals scroll a pager; a text window scrolls.
func (w *Window) SendMouse(ev uv.MouseEvent) {
	if w.body != nil {
		if wheel, ok := ev.(uv.MouseWheelEvent); ok {
			w.Scroll(map[uv.MouseButton]int{uv.MouseWheelUp: -3, uv.MouseWheelDown: 3}[wheel.Button])
		}
		return
	}
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
	if w.body != nil || w.Exited() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.term.Paste(text)
}

// Screen renders the current screen as styled spans. During a synchronized
// update it returns the screen from before, for at most a second like Ghostty,
// so a program that never ends one does not freeze its window. A text
// window has none: the UI renders its body itself.
func (w *Window) Screen() Screen {
	if w.body != nil {
		return Screen{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.syncSince.IsZero() && time.Since(w.syncSince) < time.Second && w.last.Rows != nil {
		return w.last
	}
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
			if c.Width == 0 {
				text = " " // a combining mark vt did not join: keep the columns aligned
			}
			style := c.Style
			if strings.ContainsRune(text, kitty.Placeholder) {
				text, style = w.placeholderCell(text, style)
			}
			if cur == nil || !style.Equal(&curStyle) {
				row = append(row, Span{SGR: sgrParams(style)})
				cur = &row[len(row)-1]
				curStyle = style
			}
			cur.Text += text
		}
		out.Rows[y] = row
	}
	pos := w.term.CursorPosition()
	out.CursorX, out.CursorY = pos.X, pos.Y
	out.HasCursor = !w.cursorHidden.Load()
	out.CursorShape, out.CursorBlink = (max(w.cursorStyle, 1)-1)/2, w.cursorStyle%2 == 1 || w.cursorStyle == 0
	w.last = out
	return out
}

// Text is the screen as plain text, trailing blanks trimmed, or a text
// window's body.
func (w *Window) Text() string {
	if w.body != nil {
		text, _ := w.Content()
		return text
	}
	scr := w.Screen()
	lines := make([]string, len(scr.Rows))
	for i, row := range scr.Rows {
		var b strings.Builder
		for _, s := range row {
			b.WriteString(s.Text)
		}
		lines[i] = strings.TrimRight(StripPlaceholders(b.String()), " ")
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
