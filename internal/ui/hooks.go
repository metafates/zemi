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
	"github.com/metafates/zemi/internal/lua"
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

// trustAnswers are the trust prompt's rows, in order: a click on row i
// gives answer i.
var trustAnswers = []string{"trust", "untrust", "later"}

// trustBox draws the Go-owned trust prompt (DESIGN §5, §8) in fixed host
// styles, with no Lua text inside. It has no cursor: trusting runs project
// code, so it takes y or a click on its row. On a short screen rows go in
// drop order, one spacer at a time and the grant that runs code without
// asking last, so the answers stay visible.
func trustBox(tp *trustPrompt, w, h int) [][]hostapi.Segment {
	width := min(boxInner(w), 64)
	type row struct {
		segs []hostapi.Segment
		drop int // 0 always shown, else the order rows go in
	}
	var body []row
	add := func(drop int, segs ...hostapi.Segment) {
		for _, r := range hostapi.WrapSegs(segs, width) {
			body = append(body, row{r, drop})
		}
	}
	text := func(s, hl string) { add(0, hostapi.Segment{Text: s, HL: hl}) }
	// a long path wraps at its slashes: the user trusts every character
	var path []hostapi.Segment
	for _, part := range strings.SplitAfter(hostapi.TildePath(tp.root), "/") {
		path = append(path, hostapi.Segment{Text: part, HL: "Host:Bold"})
	}
	add(0, path...)
	// the root is resolved and a linked worktree maps to its main checkout
	cwd := tp.cwd
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	if rel, err := filepath.Rel(tp.root, cwd); err == nil && rel != "." {
		if strings.HasPrefix(rel, "..") {
			text("Started in "+hostapi.TildePath(tp.cwd)+", which this answer covers too.", "Host:Hint")
		} else {
			text("Covers the whole repository, not only "+rel+".", "Host:Hint")
		}
	}
	saved, what := "saved for this repository", "repository"
	if hostapi.TrustUnsaved(tp.root) {
		saved, what = "this run only", "directory"
	}
	if tp.state != "" {
		text("Currently "+tp.state+".", "Host:Hint")
	}
	add(2)
	add(8, hostapi.Segment{Text: "Trusting lets this " + what + " run code and steer zemi:"})
	for _, g := range []struct {
		drop int
		text string
	}{
		{7, ".zemi config: init.lua, agents, commands and workflows"},
		{6, "AGENTS.md and CLAUDE.md instructions"},
		{5, "language servers, which can build the project"},
		{8, "auto mode edits, builds and tests without asking"},
	} {
		add(g.drop, hostapi.Segment{Text: "  - "}, hostapi.Segment{Text: g.text, HL: "Host:Value"})
	}
	add(1)
	add(3, hostapi.Segment{Text: "Without trust, zemi asks before anything but reads.", HL: "Host:Hint"})
	add(4)
	later, laterDesc := "Not now", "ask again next start"
	if tp.state != "" {
		later, laterDesc = "Keep as is", ""
	}
	list := listRows([]listItem{
		{label: "Trust", desc: saved, key: "y"},
		{label: "Don't trust", desc: saved, key: "n"},
		{label: later, desc: laterDesc, key: "esc"},
	}, -1, nil, 3)
	for drop := 1; drop <= 8 && len(body)+len(list)+2 > h; drop++ {
		body = slices.DeleteFunc(body, func(r row) bool { return r.drop == drop })
	}
	var rows [][]hostapi.Segment
	for _, r := range body {
		rows = append(rows, r.segs)
	}
	rows = append(rows, alignKeys(list, width)...)
	return hostBox(" Trust this "+what+"? ", "Host:Title", "", rows, w)
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
// interpreter could deadlock. An origin that neither opened the overlay nor
// outranks it closes nothing, and the close runs on_close, so a picker's
// callback always answers.
func (a *App) CloseOverlay(id string, origin lua.Origin) {
	a.do(func() {
		i := slices.IndexFunc(a.overlays, func(st overlayState) bool { return st.o.ID == id })
		if i < 0 {
			return
		}
		o := a.overlays[i].o
		if origin != lua.OriginUser && origin != lua.OriginBuiltin && origin != o.Origin {
			a.log.Debug("ui", "overlay close refused", map[string]any{"id": id, "origin": string(origin)})
			return
		}
		a.cancelOverlay(o)
	})
}

// CloseLuaOverlays drops every overlay owned by the interpreter (Gen != 0)
// on reload; Go-built ones (Gen 0) keep working and stay.
func (a *App) CloseLuaOverlays() {
	a.do(func() {
		a.overlays = slices.DeleteFunc(a.overlays, func(st overlayState) bool { return st.o.Gen != 0 })
	})
}

// Refresh invalidates the widget caches (state changed outside Update). At
// most one waits in the hook queue: Lua callbacks may call it by the hundred.
func (a *App) Refresh() {
	if a.refreshPending.Swap(true) {
		return
	}
	if !a.dispatch(uiFunc(func() tea.Cmd { a.refreshPending.Store(false); return nil })) {
		a.refreshPending.Store(false)
	}
}

// ComposerEdit posts a zemi.composer edit to the UI goroutine.
func (a *App) ComposerEdit(op, text string, cursor int) {
	text = sanitize.String(text)
	a.dispatch(uiFunc(func() tea.Cmd {
		var cmd tea.Cmd
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
		case "history_prev":
			a.seedHistory()
			a.composer.histPrev()
		case "history_next":
			a.composer.histNext()
		case "paste":
			cmd = a.pasteClipboard()
		case "edit":
			cmd = a.openEditor()
		}
		a.publishComposer()
		return cmd
	}))
}

// publishComposer hands the draft to the host for zemi.composer.get.
func (a *App) publishComposer() {
	a.host.SetComposerSnapshot(a.composer.text, a.composer.cursor)
}

// Scroll implements hostapi.UIHooks (zemi.ui.scroll.* host actions);
// posted to the UI goroutine like every other hook.
func (a *App) Scroll(kind, id string) {
	a.do(func() { a.applyScroll(kind, id) })
}
