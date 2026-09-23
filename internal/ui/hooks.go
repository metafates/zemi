package ui

import (
	"image/color"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

// fixedStyle returns the style of a "Host:*" highlight name: Go-owned
// security chrome rendered in fixed colors that no theme group can restyle
// (DESIGN §8). light picks the variant for a light terminal. Returning
// false leaves the segment to theme resolution.
func fixedStyle(hl string, light bool) (uv.Style, bool) {
	st, ok := hostStyles[light][hl]
	return st, ok
}

func rgb(v uint32) ansi.RGBColor {
	return ansi.RGBColor{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

// chrome is the host chrome's palette: greys and the one accent
// (periwinkle), plus the faintest tint a diff row may take. Red, green and
// yellow stay the terminal's own, so the user's palette decides them. The
// canvas is the theme's (hlStyle fills it), so chrome and widgets share one.
type chrome struct{ fg, dim, rule, accent, panel, add, del color.Color }

var (
	darkChrome  = chrome{rgb(0xd8d8d8), rgb(0x7a7a7a), rgb(0x3a3a3a), rgb(0x9aa5f2), rgb(0x1e1e1e), rgb(0x183018), rgb(0x361010)}
	lightChrome = chrome{rgb(0x1b1b1b), rgb(0x8e8e8e), rgb(0xcfcfcf), rgb(0x6b78d6), rgb(0xf0f0f0), rgb(0xe7f2e2), rgb(0xfae7e7)}
	// keyed by "the terminal is light"
	hostStyles = map[bool]map[string]uv.Style{false: chromeStyles(darkChrome), true: chromeStyles(lightChrome)}
)

// chromeStyles are the fixed styles of the permission prompt and other Go
// chrome: no theme can restyle them, so nothing can dress up a prompt.
func chromeStyles(c chrome) map[string]uv.Style {
	return map[string]uv.Style{
		"Host:Bold":        {Fg: c.fg, Attrs: uv.AttrBold},
		"Host:Title":       {Fg: c.accent, Attrs: uv.AttrBold},
		"Host:Rule":        {Fg: c.rule},
		"Host:Cursor":      {Attrs: uv.AttrReverse},
		"Host:Value":       {Fg: c.fg},
		"Host:Hint":        {Fg: c.dim},
		"Host:HintKey":     {Fg: c.accent},
		"Host:Warn":        {Fg: ansi.Yellow},
		"Host:Ok":          {Fg: ansi.Green},
		"Host:Danger":      {Fg: ansi.Red},
		"Host:DangerPanel": {Fg: ansi.Red, Bg: c.panel, Attrs: uv.AttrBold},
		"Host:Panel":       {Fg: c.fg, Bg: c.panel},
		"Host:DiffAdd":     {Fg: c.fg, Bg: c.add},
		"Host:DiffAddNum":  {Fg: ansi.Green, Bg: c.add},
		"Host:DiffDel":     {Fg: c.fg, Bg: c.del},
		"Host:DiffDelNum":  {Fg: ansi.Red, Bg: c.del},
		"Host:DiffCtx":     {Fg: c.fg},
		"Host:DiffCtxNum":  {Fg: c.dim},
		"Host:Selection":   {Attrs: uv.AttrReverse},
		"Host:Hover":       {Underline: uv.UnderlineSingle},
		"Host:Dim":         {Fg: c.rule},
	}
}

// trustPrompt is the Go-owned project trust overlay (DESIGN §5, §8): the
// first start in a project with no saved decision, or /trust. Its chrome is
// host drawn; no Lua participates.
type trustPrompt struct {
	root, cwd string
	state     string // the current decision, shown by /trust; "" at boot
	answer    chan string
	opened    time.Time // a reload can open it mid-typing (permGrace)
	sel       int       // the highlighted answer, Later at first
	view      viewport  // what trusting means
	size      boxSize
}

// AskTrust blocks the caller (a boot step or /trust) until the user answers
// "trust", "untrust" or "later" (not now). It is safe from any goroutine:
// the prompt travels through the hook channel and the answer comes back on
// a buffered channel.
func (a *App) AskTrust(root, cwd, state string) string {
	ch := make(chan string, 1)
	a.await(func() tea.Cmd {
		a.trust = &trustPrompt{root: root, cwd: cwd, state: state, answer: ch, opened: a.now(), sel: len(trustAnswers) - 1}
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
	a.bump()
}

// trustAnswers are the trust prompt's rows, in order: a click on row i
// gives answer i.
var trustAnswers = []string{"trust", "untrust", "later"}

// trustBox draws the Go-owned trust prompt (DESIGN §5, §8) in fixed host
// styles, with no Lua text inside. The cursor starts on Later, so an enter
// typed by habit trusts nothing. On a short screen what trusting means
// scrolls, so the answers stay visible.
func trustBox(tp *trustPrompt, w, h int) [][]hostapi.Segment {
	width := min(boxInner(w), 64)
	saved, what := "saved for this project", "project"
	if hostapi.TrustUnsaved(tp.root) {
		saved, what = "kept for this run only", "directory"
	}
	explain := func(width int) [][]hostapi.Segment {
		// a long path wraps at its slashes: the user trusts every character
		var path []hostapi.Segment
		for _, part := range strings.SplitAfter(hostapi.TildePath(tp.root), "/") {
			path = append(path, hostapi.Segment{Text: part, HL: "Host:Bold"})
		}
		rows := hostapi.WrapSegs(path, width)
		// the root is resolved and a linked worktree maps to its main checkout
		cwd := tp.cwd
		if real, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = real
		}
		switch rel, in := perm.Under(tp.root, cwd); {
		case !in:
			rows = append(rows, wrapText("Started in "+hostapi.TildePath(tp.cwd)+", which this answer covers too.", "Host:Hint", width)...)
		case rel != ".":
			rows = append(rows, wrapText("Covers the whole project, not only "+rel+".", "Host:Hint", width)...)
		}
		if tp.state != "" {
			rows = append(append(rows, nil), wrapText("Currently "+tp.state+".", "Host:Hint", width)...)
		}
		rows = append(rows, nil)
		rows = append(rows, wrapText("Trusting lets this "+what+" run code and steer zemi:", "", width)...)
		for _, g := range []string{
			".zemi config: init.lua, agents, commands and workflows",
			"AGENTS.md and CLAUDE.md instructions",
			"language servers, which can build the project",
			"auto mode edits, builds and tests without asking",
		} {
			rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: "  - "}, {Text: g, HL: "Host:Value"}}, width)...)
		}
		return append(append(rows, nil), wrapText("Without trust, zemi asks before anything but reads. Either answer is "+saved+".", "Host:Hint", width)...)
	}
	laterDesc := "ask again next start"
	if tp.state != "" {
		laterDesc = "stays " + tp.state
	}
	choices := []promptChoice{
		{key: "y", label: "Trust"},
		{key: "n", label: "Don't trust"},
		{key: "l", label: "Later", desc: laterDesc},
	}
	body := fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		list := choiceRows(choices, tp.sel, width, level > 1)
		tp.view.viewH = max(h-2-len(stack(tight, oneRow, list, oneRow))+1, 1)
		text := tp.view.window(tp.view.wrap(explain, width), width)
		hintRow := hint(width, slices.Concat([]hintPair{{"enter", "confirm"}}, scrolls(tp.view), []hintPair{{"esc", "later"}})...)
		return stack(tight, text, list, [][]hostapi.Segment{hintRow}), tp.view.viewH < min(tp.view.rows, 3)
	})
	return promptBox("Trust this "+what+"?", "", body, w, h, 100, &tp.size)
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
