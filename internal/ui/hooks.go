package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
)

// fixedStyle returns the style of a "Host:*" highlight name: Go-owned
// security chrome rendered in fixed colors that no theme group can restyle
// (DESIGN §8). Returning false leaves the segment to theme resolution.
func fixedStyle(hl string) (uv.Style, bool) {
	switch hl {
	case "Host:TrustTitle":
		return uv.Style{
			Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14},
			Bg: ansi.RGBColor{R: 0xf7, G: 0x76, B: 0x8e},
		}, true
	case "Host:TrustKey":
		return uv.Style{Attrs: uv.AttrBold}, true
	case "Host:PermTitle":
		return uv.Style{
			Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14},
			Bg: ansi.RGBColor{R: 0xf7, G: 0x76, B: 0x8e},
		}, true
	case "Host:PermKey":
		return uv.Style{Attrs: uv.AttrBold}, true
	case "Host:PickTitle":
		return uv.Style{
			Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14},
			Bg: ansi.RGBColor{R: 0x7a, G: 0xa2, B: 0xf7},
		}, true
	case "Host:PickSel":
		return uv.Style{Attrs: uv.AttrBold}, true
	case "Host:MemTitle":
		return uv.Style{
			Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14},
			Bg: ansi.RGBColor{R: 0x5a, G: 0xf7, B: 0x8e},
		}, true
	case "Host:MemMark":
		return uv.Style{Attrs: uv.AttrBold}, true
	case "Host:QTitle":
		return uv.Style{
			Fg: ansi.RGBColor{R: 0x10, G: 0x10, B: 0x14},
			Bg: ansi.RGBColor{R: 0xf7, G: 0xa2, B: 0x5a},
		}, true
	}
	return uv.Style{}, false
}

// trustPrompt is the Go-owned project-config trust overlay (DESIGN §5, §8):
// first load of <project>/.zemi, or a changed tree hash. Its chrome is host
// drawn; no Lua participates.
type trustPrompt struct {
	path   string
	hash   string
	answer chan bool
}

// AskTrust blocks the caller (a boot step) until the user answers. It is
// safe from any goroutine: the prompt travels through the hook channel and
// the answer comes back on a buffered channel.
func (a *App) AskTrust(path, hash string) bool {
	ch := make(chan bool, 1)
	a.dispatch(trustAskMsg{path: path, hash: hash, answer: ch})
	return <-ch
}

func (a *App) answerTrust(ok bool) {
	tp := a.trust
	a.trust = nil
	if tp != nil && tp.answer != nil {
		tp.answer <- ok
	}
	a.frameGen++
}

// spliceTrust composes the Go-owned trust prompt box over the chat area
// (DESIGN §5, §8): title and answer keys use fixed host styles via the
// "Host:*" highlight names, so no theme group can forge or restyle them,
// and no Lua text is rendered inside this chrome.
func (a *App) spliceTrust(rows [][]hostapi.Segment, tp *trustPrompt, w, headerH, chatH int) [][]hostapi.Segment {
	title := " Trust project config? "
	short := tp.hash
	if len(short) > 12 {
		short = short[:12]
	}
	lines := []string{
		tp.path,
		"hash: " + short,
		"",
		"[y] trust this config · [n] refuse once",
	}

	inner := 0
	for _, l := range lines {
		if n := ansi.StringWidth(l); n > inner {
			inner = n
		}
	}
	bw := ansi.StringWidth(title) + 4
	if inner+6 > bw {
		bw = inner + 6
	}
	if bw > w-4 {
		bw = w - 4
	}
	if bw < 12 {
		bw = 12
	}

	var box [][]hostapi.Segment
	box = append(box, []hostapi.Segment{
		{Text: "┌", HL: "Host:TrustTitle"},
		{Text: title, HL: "Host:TrustTitle"},
		{Text: strings.Repeat("─", max0(bw-2-ansi.StringWidth(title))), HL: "Host:TrustTitle"},
		{Text: "┐", HL: "Host:TrustTitle"},
	})
	for _, l := range lines {
		text := ansi.Truncate(l, bw-6, "…")
		hl := ""
		if strings.HasPrefix(l, "[y]") {
			hl = "Host:TrustKey"
		}
		pad := bw - 6 - ansi.StringWidth(text)
		box = append(box, []hostapi.Segment{
			{Text: "│ ", HL: ""},
			{Text: text + strings.Repeat(" ", max0(pad)), HL: hl},
			{Text: " │", HL: ""},
		})
	}
	box = append(box, []hostapi.Segment{
		{Text: "└", HL: ""},
		{Text: strings.Repeat("─", bw-4), HL: ""},
		{Text: "┘", HL: ""},
	})
	return spliceBox(rows, box, w, headerH, chatH)
}

// ---------------------------------------------------------------------------
// hostapi.UIHooks

// Notify shows a toast (sanitize already applied by the host API boundary;
// the level is normalized lowercase for the theme mapping).
func (a *App) Notify(level, title, body string) {
	a.dispatch(toastMsg{level: strings.ToLower(level), title: sanitize.String(title), body: sanitize.String(body)})
}

func (a *App) OpenCmdline(init string) {
	a.dispatch(cmdlineOpenMsg{prefix: init})
}

func (a *App) CloseCmdline() {
	a.dispatch(cmdlineCloseMsg{})
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
