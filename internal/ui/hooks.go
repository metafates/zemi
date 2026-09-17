package ui

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
)

// fixedStyle returns the style of a "Host:*" highlight name: Go-owned
// security chrome rendered in fixed colors that no theme group can restyle
// (DESIGN §8). Returning false leaves the segment to theme resolution.
func fixedStyle(hl string) (uv.Style, bool) {
	st, ok := hostStyles[hl]
	return st, ok
}

func rgb(v uint32) ansi.RGBColor {
	return ansi.RGBColor{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

// hostStyles are the fixed styles of the permission prompt and other Go
// chrome: no theme can restyle them, so nothing can dress up a prompt.
var hostStyles = map[string]uv.Style{
	"Host:Bold":        {Attrs: uv.AttrBold},
	"Host:Title":       {Fg: rgb(0x7aa2f7), Attrs: uv.AttrBold},
	"Host:Cursor":      {Fg: rgb(0x1a1b26), Bg: rgb(0xc0caf5)},
	"Host:Value":       {Fg: rgb(0xc0caf5)},
	"Host:Hint":        {Fg: rgb(0x6b7394)},
	"Host:HintKey":     {Fg: rgb(0xa9b1d6), Attrs: uv.AttrBold},
	"Host:Warn":        {Fg: rgb(0xe0af68)},
	"Host:Ok":          {Fg: rgb(0x9ece6a)},
	"Host:Panel":       {Fg: rgb(0xc0caf5), Bg: rgb(0x242838)},
	"Host:PanelTitle":  {Fg: rgb(0x7aa2f7), Bg: rgb(0x242838), Attrs: uv.AttrBold},
	"Host:DiffAddLine": {Fg: rgb(0xd4e8c8), Bg: rgb(0x243427)},
	"Host:DiffAddNum":  {Fg: rgb(0x7fa36c), Bg: rgb(0x243427)},
	"Host:DiffDelLine": {Fg: rgb(0xeccad0), Bg: rgb(0x3a2629)},
	"Host:DiffDelNum":  {Fg: rgb(0xb06d76), Bg: rgb(0x3a2629)},
	"Host:DiffCtxLine": {Fg: rgb(0x9aa0c0)},
	"Host:DiffCtxNum":  {Fg: rgb(0x4c5372)},
	"Host:BtnSel":      {Fg: rgb(0x16161e), Bg: rgb(0x7aa2f7), Attrs: uv.AttrBold},
	"Host:Selection":   {Fg: rgb(0xe6e9f5), Bg: rgb(0x3d4f7c)},
	"Host:Hover":       {Fg: rgb(0xe6e9f5), Bg: rgb(0x2d3452)},
	"Host:Dim":         {Fg: rgb(0x3b4261)},
}

// trustPrompt is the Go-owned project trust overlay (DESIGN §5, §8): the
// first start in a project with no saved decision, or /trust. Its chrome is
// host drawn; no Lua participates.
type trustPrompt struct {
	root, cwd string
	state     string // the current decision, shown by /trust; "" at boot
	answer    chan string
	opened    time.Time // a reload can open it mid-typing (permGrace)
}

// AskTrust blocks the caller (a boot step or /trust) until the user answers
// "trust", "untrust" or "later" (not now). It is safe from any goroutine:
// the prompt travels through the hook channel and the answer comes back on
// a buffered channel.
func (a *App) AskTrust(root, cwd, state string) string {
	ch := make(chan string, 1)
	a.await(func() tea.Cmd {
		a.trust = &trustPrompt{root: root, cwd: cwd, state: state, answer: ch, opened: a.now()}
		return nil
	})
	return <-ch
}

func (a *App) answerTrust(answer string) {
	tp := a.trust
	a.trust = nil
	if tp != nil && tp.answer != nil {
		tp.answer <- answer
	}
	a.frameGen++
}

// trustAnswers are the trust prompt's rows, in order: a click on row i or
// its key gives answer i.
var trustAnswers = []string{"trust", "untrust", "later"}

// trustBox draws the Go-owned trust prompt (DESIGN §5, §8) in fixed host
// styles, with no Lua text inside. It has no cursor: trusting runs project
// code, so it takes y or a click on its row.
func trustBox(tp *trustPrompt, w int) [][]hostapi.Segment {
	width := min(boxInner(w), 64)
	var body [][]hostapi.Segment
	text := func(s, hl string) {
		body = append(body, wrapSegs([]hostapi.Segment{{Text: s, HL: hl}}, width)...)
	}
	// a long path wraps at its slashes: the user trusts every character
	var path []hostapi.Segment
	for _, part := range strings.SplitAfter(hostapi.TildePath(tp.root), "/") {
		path = append(path, hostapi.Segment{Text: part, HL: "Host:Bold"})
	}
	body = append(body, wrapSegs(path, width)...)
	if rel, err := filepath.Rel(tp.root, tp.cwd); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		text("Covers the whole repository, not only "+rel+".", "Host:Hint")
	}
	saved, what := "saved for this repository", "repository"
	if hostapi.TrustUnsaved(tp.root) {
		text("Trust for your home directory or / lasts this run only.", "Host:Hint")
		saved, what = "this run only", "directory"
	}
	if tp.state != "" {
		text("Currently "+tp.state+".", "Host:Hint")
	}
	text("", "")
	text("Trusting lets this "+what+" run code and steer zemi:", "")
	for _, s := range []string{
		".zemi config: init.lua, agents, commands and workflows",
		"AGENTS.md and CLAUDE.md instructions",
		"language servers, which can build the project",
		"auto mode edits, builds and tests without asking",
	} {
		text("  "+s, "Host:Value")
	}
	text("", "")
	text("Without trust, zemi asks before anything but reads.", "Host:Hint")
	text("", "")
	later, laterDesc := "Not now", "ask again next start"
	if tp.state != "" {
		later, laterDesc = "Keep as is", ""
	}
	list := listRows([]listItem{
		{label: "Trust", desc: saved, key: "y"},
		{label: "Don't trust", desc: saved, key: "n"},
		{label: later, desc: laterDesc, key: "esc"},
	}, -1, nil, 3)
	for _, r := range body {
		width = max(width, segsWidth(r))
	}
	body = append(body, alignKeys(list, width)...)
	return hostBox(" Trust this project? ", "Host:Title", "", body, w)
}

// ---------------------------------------------------------------------------
// hostapi.UIHooks

// Notify shows a toast from any goroutine. Title and body are sanitized
// here; the level is lowercased for the theme mapping.
func (a *App) Notify(level, title, body string) {
	level, title, body = strings.ToLower(level), sanitize.String(title), sanitize.String(body)
	a.do(func() { a.pushToast(level, title, body) })
}

func (a *App) OpenOverlay(o hostapi.Overlay) {
	a.do(func() { a.openOverlay(o) })
}

// CloseOverlay posts the close: waiting on the UI goroutine from the
// interpreter could deadlock.
func (a *App) CloseOverlay(id string) {
	a.do(func() { a.closeOverlayByID(id) })
}

// CloseLuaOverlays drops every overlay owned by the interpreter (Gen != 0)
// on reload; Go-built ones (Gen 0) keep working and stay.
func (a *App) CloseLuaOverlays() {
	a.do(func() {
		a.overlays = slices.DeleteFunc(a.overlays, func(st overlayState) bool { return st.o.Gen != 0 })
	})
}

// Refresh invalidates the widget caches (state changed outside Update).
func (a *App) Refresh() { a.do(func() {}) }

// ComposerEdit posts a zemi.composer edit to the UI goroutine.
func (a *App) ComposerEdit(op, text string, cursor int) {
	text = sanitize.String(text)
	a.do(func() {
		switch op {
		case "set":
			a.composer.text, a.composer.cursor = text, cursor
			if n := len([]rune(text)); cursor < 0 || cursor > n {
				a.composer.cursor = n
			}
		case "insert":
			a.composer.insert(text)
		case "submit":
			a.submitComposer()
		}
		a.publishComposer()
	})
}

// publishComposer hands the draft to the host for zemi.composer.get.
func (a *App) publishComposer() {
	a.host.SetComposerSnapshot(a.composer.text, a.composer.cursor)
}

// ScrollChat moves the transcript viewport (zemi.ui.scroll.* host
// actions); posted to the UI goroutine like every other hook.
func (a *App) ScrollChat(kind string) {
	a.do(func() { a.applyChatScroll(kind) })
}
