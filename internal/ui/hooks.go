package ui

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
)

// fixedStyle returns the style of a "Host:*" highlight name: Go-owned
// security chrome rendered in fixed colors that no theme group can restyle
// (DESIGN §8). Returning false leaves the segment to theme resolution.
func fixedStyle(hl string) (uv.Style, bool) {
	switch hl {
	case "Host:Bold":
		return uv.Style{Attrs: uv.AttrBold}, true
	case "Host:DiffAdd":
		return uv.Style{Fg: ansi.RGBColor{R: 0x9e, G: 0xce, B: 0x6a}}, true
	case "Host:DiffDel":
		return uv.Style{Fg: ansi.RGBColor{R: 0xf7, G: 0x76, B: 0x8e}}, true
	}
	if st, ok := hostStyles[hl]; ok {
		return st, true
	}
	if bg, ok := hostTitleBg[hl]; ok {
		return uv.Style{Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14}, Bg: bg}, true
	}
	return uv.Style{}, false
}

func rgb(v uint32) ansi.RGBColor {
	return ansi.RGBColor{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

// hostStyles are the fixed styles of the permission prompt and other Go
// chrome: no theme can restyle them, so nothing can dress up a prompt.
var hostStyles = map[string]uv.Style{
	"Host:Value":       {Fg: rgb(0xc0caf5)},
	"Host:Hint":        {Fg: rgb(0x6b7394)},
	"Host:HintKey":     {Fg: rgb(0xa9b1d6), Attrs: uv.AttrBold},
	"Host:Warn":        {Fg: rgb(0xe0af68)},
	"Host:Panel":       {Fg: rgb(0xc0caf5), Bg: rgb(0x242838)},
	"Host:DiffAddLine": {Fg: rgb(0xd4e8c8), Bg: rgb(0x243427)},
	"Host:DiffAddNum":  {Fg: rgb(0x7fa36c), Bg: rgb(0x243427)},
	"Host:DiffDelLine": {Fg: rgb(0xeccad0), Bg: rgb(0x3a2629)},
	"Host:DiffDelNum":  {Fg: rgb(0xb06d76), Bg: rgb(0x3a2629)},
	"Host:DiffCtxLine": {Fg: rgb(0x9aa0c0)},
	"Host:DiffCtxNum":  {Fg: rgb(0x4c5372)},
	"Host:Btn":         {Fg: rgb(0xa9b1d6), Bg: rgb(0x2b2f45)},
	"Host:BtnSel":      {Fg: rgb(0x16161e), Bg: rgb(0x7aa2f7), Attrs: uv.AttrBold},
	"Host:BtnNo":       {Fg: rgb(0x16161e), Bg: rgb(0xf7768e), Attrs: uv.AttrBold},
	"Host:Selection":   {Fg: rgb(0xe6e9f5), Bg: rgb(0x3d4f7c)},
	"Host:Hover":       {Fg: rgb(0xe6e9f5), Bg: rgb(0x2d3452)},
	"Host:BtnHover":    {Fg: rgb(0xe6e9f5), Bg: rgb(0x3d4f7c), Attrs: uv.AttrBold},
}

var hostTitleBg = map[string]ansi.RGBColor{
	"Host:TrustTitle": {R: 0xf7, G: 0x76, B: 0x8e},
	"Host:PermTitle":  {R: 0xe0, G: 0xaf, B: 0x68},
	"Host:PickTitle":  {R: 0x7a, G: 0xa2, B: 0xf7},
	"Host:MemTitle":   {R: 0x5a, G: 0xf7, B: 0x8e},
	"Host:QTitle":     {R: 0xf7, G: 0xa2, B: 0x5a},
}

// trustPrompt is the Go-owned project-config trust overlay (DESIGN §5, §8):
// first load of <project>/.zemi, or a changed tree hash. Its chrome is host
// drawn; no Lua participates.
type trustPrompt struct {
	path   string
	hash   string
	answer chan string
}

// AskTrust blocks the caller (a boot step) until the user answers "trust",
// "once" (not now) or "deny" (never for this tree hash). It is safe from
// any goroutine: the prompt travels through the hook channel and the
// answer comes back on a buffered channel.
func (a *App) AskTrust(path, hash string) string {
	ch := make(chan string, 1)
	a.dispatch(trustAskMsg{path: path, hash: hash, answer: ch})
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

// spliceTrust composes the Go-owned trust prompt box over the chat area
// (DESIGN §5, §8): title and answer keys use fixed host styles via the
// "Host:*" highlight names, so no theme group can forge or restyle them,
// and no Lua text is rendered inside this chrome.
func (a *App) spliceTrust(rows [][]hostapi.Segment, tp *trustPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	short := tp.hash
	if len(short) > 12 {
		short = short[:12]
	}
	body := [][]hostapi.Segment{
		{{Text: tp.path}},
		{{Text: "hash: " + short}},
		{{Text: ""}},
		keyHints("y", "trust this config", "n", "not now", "d", "never for this version"),
	}
	box := hostBox(" Trust project config? ", "Host:TrustTitle", "", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
}

// ---------------------------------------------------------------------------
// hostapi.UIHooks

// Notify shows a toast (sanitize already applied by the host API boundary;
// the level is normalized lowercase for the theme mapping).
func (a *App) Notify(level, title, body string) {
	a.dispatch(toastMsg{level: strings.ToLower(level), title: sanitize.String(title), body: sanitize.String(body)})
}

func (a *App) OpenOverlay(o hostapi.Overlay) {
	a.dispatch(overlayOpenMsg{o: o})
}

// CloseOverlay pops the overlay synchronously when no program is attached
// (tests); with a program it posts the close and returns true: the Lua
// side only uses the call, and a synchronous reply here could deadlock the
// queue (interpreter waits for the UI goroutine, which may be blocked in
// rt.Do waiting for that same interpreter).
func (a *App) CloseOverlay(id string) bool {
	if a.program == nil {
		ok := a.closeOverlayByID(id)
		a.frameGen++
		return ok
	}
	a.dispatch(overlayCloseMsg{id: id})
	return true
}

// Refresh invalidates the widget caches (state changed outside Update).
func (a *App) Refresh() {
	a.dispatch(invalidateMsg{})
}

type composerEditMsg struct {
	op, text string
	cursor   int
}

// ComposerEdit posts a zemi.composer edit to the UI goroutine.
func (a *App) ComposerEdit(op, text string, cursor int) {
	a.dispatch(composerEditMsg{op: op, text: sanitize.String(text), cursor: cursor})
}

// applyComposerEdit runs on the UI goroutine.
func (a *App) applyComposerEdit(m composerEditMsg) {
	switch m.op {
	case "set":
		a.composer.text = m.text
		if n := len([]rune(m.text)); m.cursor < 0 || m.cursor > n {
			a.composer.cursor = n
		} else {
			a.composer.cursor = m.cursor
		}
	case "insert":
		a.composer.insert(m.text)
	case "submit":
		a.submitComposer()
	}
	a.publishComposer()
	a.frameGen++
}

// publishComposer hands the draft to the host for zemi.composer.get.
func (a *App) publishComposer() {
	a.host.SetComposerSnapshot(a.composer.text, a.composer.cursor)
}

// ScrollChat moves the transcript viewport (zemi.ui.scroll.* host
// actions); posted to the UI goroutine like every other hook.
func (a *App) ScrollChat(kind string) {
	a.dispatch(chatScrollMsg{kind: kind})
}
