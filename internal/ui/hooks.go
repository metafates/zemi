package ui

import (
	"image/color"
	"log/slog"
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

func rgb(v uint32) ansi.RGBColor {
	return ansi.RGBColor{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

// chrome is the host chrome's palette, whorl's own values (TestChromeIsWhorl
// keeps them equal): no theme can restyle a prompt, and it still looks like
// zemi. The canvas is the theme's (hlStyle fills it). The selected row's band
// is the canvas tinted toward the accent (faintly: 256 colors round a bluer
// tint to navy, not grey).
type chrome struct{ fg, dim, rule, accent, panel, add, del, ok, warn, danger, band color.Color }

var (
	darkChrome = chrome{
		rgb(0xd8d8d8), rgb(0x828282), rgb(0x3a3a3a), rgb(0x9aa5f2), rgb(0x1e1e1e),
		rgb(0x183018), rgb(0x361010), rgb(0x82d2a8), rgb(0xf0d680), rgb(0xe97170),
		rgb(0x28292f),
	}
	lightChrome = chrome{
		rgb(0x1b1b1b), rgb(0x737373), rgb(0xcfcfcf), rgb(0x6169c8), rgb(0xf0f0f0),
		rgb(0xe7f2e2), rgb(0xfae7e7), rgb(0x2a7d57), rgb(0xa97500), rgb(0xc72d2e),
		rgb(0xe9eaf6),
	}
	// keyed by "the canvas is light" (hlStyle)
	hostStyles = map[bool]map[string]uv.Style{false: chromeStyles(darkChrome), true: chromeStyles(lightChrome)}
)

// chromeStyles are the fixed styles of the permission prompt and other Go
// chrome: no theme can restyle them, so nothing can dress up a prompt.
func chromeStyles(c chrome) map[string]uv.Style {
	return map[string]uv.Style{
		hostapi.ChromeBold:       {Fg: c.fg, Attrs: uv.AttrBold},
		hostapi.ChromeTitle:      {Fg: c.accent, Attrs: uv.AttrBold},
		hostapi.ChromeRule:       {Fg: c.rule},
		hostapi.ChromeCursor:     {Attrs: uv.AttrReverse},
		hostapi.ChromeValue:      {Fg: c.fg},
		hostapi.ChromeHint:       {Fg: c.dim},
		hostapi.ChromeHintKey:    {Fg: c.accent},
		hostapi.ChromeWarn:       {Fg: c.warn},
		hostapi.ChromeOk:         {Fg: c.ok},
		hostapi.ChromeDanger:     {Fg: c.danger},
		hostapi.ChromePanel:      {Fg: c.fg, Bg: c.panel},
		hostapi.ChromeDiffAdd:    {Fg: c.fg, Bg: c.add},
		hostapi.ChromeDiffAddNum: {Fg: c.ok, Bg: c.add},
		hostapi.ChromeDiffDel:    {Fg: c.fg, Bg: c.del},
		hostapi.ChromeDiffDelNum: {Fg: c.danger, Bg: c.del},
		hostapi.ChromeDiffCtx:    {Fg: c.fg},
		hostapi.ChromeDiffCtxNum: {Fg: c.dim},
		hostapi.ChromeSelection:  {Attrs: uv.AttrReverse},
		hostapi.ChromeHover:      {Underline: uv.UnderlineSingle},
		hostapi.ChromeDim:        {Fg: c.rule},
		hostapi.ChromeBand:       {Bg: c.band},
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
		for _, part := range strings.SplitAfter(perm.TildePath(tp.root), "/") {
			path = append(path, hostapi.Segment{Text: part, HL: hostapi.ChromeBold})
		}
		rows := hostapi.WrapSegs(path, width)
		// the root is resolved and a linked worktree maps to its main checkout
		cwd := tp.cwd
		if real, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = real
		}
		switch rel, in := perm.Under(tp.root, cwd); {
		case !in:
			rows = append(rows, wrapText("Started in "+perm.TildePath(tp.cwd)+", which this answer covers too.", hostapi.ChromeHint, width)...)
		case rel != ".":
			rows = append(rows, wrapText("Covers the whole project, not only "+rel+".", hostapi.ChromeHint, width)...)
		}
		if tp.state != "" {
			rows = append(append(rows, nil), wrapText("Currently "+tp.state+".", hostapi.ChromeHint, width)...)
		}
		rows = append(rows, nil)
		rows = append(rows, wrapText("Trusting lets this "+what+" run code and steer zemi:", "", width)...)
		for _, g := range []string{
			".agents: Lua, skills, agents, commands and workflows",
			"AGENTS.md and CLAUDE.md instructions",
			"language servers, which can build the project",
			"auto mode edits, builds and tests without asking",
		} {
			rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: "  - "}, {Text: g, HL: hostapi.ChromeValue}}, width)...)
		}
		return append(append(rows, nil), wrapText("Without trust, zemi asks before anything but reads. Either answer is "+saved+".", hostapi.ChromeHint, width)...)
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
	act := func(compact bool) [][]hostapi.Segment { return choiceRows(choices, tp.sel, width, compact) }
	body := scrollChoiceBody(h, width, &tp.view, explain, width, act, false, pickHint, hintPair{"esc", "later"})
	return promptBox("Trust this "+what+"?", "", body, w, h, 100, &tp.size)
}

// ---------------------------------------------------------------------------
// hostapi.UIHooks

// Notify shows a toast from any goroutine. Title and body are sanitized
// here.
func (a *App) Notify(level slog.Level, title, body string) {
	title, body = sanitize.String(title), sanitize.String(body)
	a.do(func() { a.pushToast(level, title, body) })
}

func (a *App) OpenOverlay(o hostapi.Overlay) {
	a.do(func() { a.openOverlay(o) })
}

// RefreshOverlay rebuilds the open overlay id with build on the worker and
// replaces it in place, Body and Keys too; a closed one builds nothing.
func (a *App) RefreshOverlay(id string, build func() hostapi.Overlay) {
	a.do(func() {
		if a.overlayIndex(id) >= 0 {
			a.background(func() {
				o := build()
				o.Refresh = true
				a.do(func() { a.openOverlay(o) })
			})
		}
	})
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
		if !mayClose(origin, o) {
			a.log.Debug("overlay close refused", "origin", "ui", "id", id, "from", string(origin))
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
	if !a.dispatch(uiFunc(func() tea.Cmd { a.refreshPending.Store(false); a.dropAnsweredPerm(); return nil })) {
		a.refreshPending.Store(false)
	}
}

// ComposerEdit posts a zemi.composer edit to the UI goroutine.
func (a *App) ComposerEdit(op hostapi.ComposerOp, text string, cursor int) {
	text = sanitize.String(text)
	a.dispatch(uiFunc(func() tea.Cmd {
		var cmd tea.Cmd
		switch op {
		case hostapi.ComposerSet:
			a.composer.text, a.composer.cursor = text, cursor
			if n := len([]rune(text)); cursor < 0 || cursor > n {
				a.composer.cursor = n
			}
		case hostapi.ComposerInsert:
			a.composer.insert(text)
		case hostapi.ComposerSubmit:
			a.submitComposer()
		case hostapi.ComposerHistoryPrev:
			a.seedHistory()
			a.composer.histPrev()
		case hostapi.ComposerHistoryNext:
			a.composer.histNext()
		case hostapi.ComposerPaste:
			cmd = a.pasteClipboard()
		case hostapi.ComposerEditor:
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
func (a *App) Scroll(kind hostapi.ScrollAction, id string) {
	a.do(func() { a.applyScroll(kind, id) })
}
