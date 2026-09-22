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
// yellow stay the terminal's own, so the user's palette decides them. bg
// is the chrome's own canvas: host-drawn text and dims paint it, so the
// terminal's background never shows through even under a modal.
type chrome struct{ fg, dim, rule, accent, panel, add, del, bg color.Color }

var (
	darkChrome  = chrome{rgb(0xd8d8d8), rgb(0x7a7a7a), rgb(0x3a3a3a), rgb(0x9aa5f2), rgb(0x1e1e1e), rgb(0x1b2417), rgb(0x2a1a1a), rgb(0x141414)}
	lightChrome = chrome{rgb(0x1b1b1b), rgb(0x8e8e8e), rgb(0xcfcfcf), rgb(0x6b78d6), rgb(0xf0f0f0), rgb(0xe7f2e2), rgb(0xfae7e7), rgb(0xfafafa)}
	// keyed by "the terminal is light"
	hostStyles = map[bool]map[string]uv.Style{false: chromeStyles(darkChrome), true: chromeStyles(lightChrome)}
)

// chromeStyles are the fixed styles of the permission prompt and other Go
// chrome: no theme can restyle them, so nothing can dress up a prompt.
func chromeStyles(c chrome) map[string]uv.Style {
	return map[string]uv.Style{
		"Host:Bold":        {Fg: c.fg, Bg: c.bg, Attrs: uv.AttrBold},
		"Host:Title":       {Fg: c.accent, Bg: c.bg, Attrs: uv.AttrBold},
		"Host:Rule":        {Fg: c.rule, Bg: c.bg},
		"Host:Cursor":      {Attrs: uv.AttrReverse},
		"Host:Value":       {Fg: c.fg, Bg: c.bg},
		"Host:Hint":        {Fg: c.dim, Bg: c.bg},
		"Host:HintKey":     {Fg: c.accent, Bg: c.bg},
		"Host:Warn":        {Fg: ansi.Yellow, Bg: c.bg},
		"Host:Ok":          {Fg: ansi.Green, Bg: c.bg},
		"Host:Danger":      {Fg: ansi.Red, Bg: c.bg},
		"Host:Panel":       {Fg: c.fg, Bg: c.panel},
		"Host:PanelTitle":  {Fg: c.accent, Bg: c.panel, Attrs: uv.AttrBold},
		"Host:DiffAdd":     {Bg: c.add},
		"Host:DiffAddSign": {Fg: ansi.Green, Bg: c.add},
		"Host:DiffDel":     {Bg: c.del},
		"Host:DiffDelSign": {Fg: ansi.Red, Bg: c.del},
		"Host:BtnSel":      {Attrs: uv.AttrReverse | uv.AttrBold},
		"Host:Selection":   {Attrs: uv.AttrReverse},
		"Host:Hover":       {Underline: uv.UnderlineSingle},
		"Host:Dim":         {Fg: c.rule, Bg: c.bg},
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
	switch rel, in := perm.Under(tp.root, cwd); {
	case !in:
		text("Started in "+hostapi.TildePath(tp.cwd)+", which this answer covers too.", "Host:Hint")
	case rel != ".":
		text("Covers the whole repository, not only "+rel+".", "Host:Hint")
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
	list := choiceRows([]promptChoice{
		{key: "y", label: "Trust", desc: saved},
		{key: "n", label: "Don't trust", desc: saved},
		{key: "esc", label: later, desc: laterDesc},
	}, -1, width)
	for drop := 1; drop <= 8 && len(body)+len(list)+2 > h; drop++ {
		body = slices.DeleteFunc(body, func(r row) bool { return r.drop == drop })
	}
	var rows [][]hostapi.Segment
	for _, r := range body {
		rows = append(rows, r.segs)
	}
	return promptBox("Trust this "+what+"?", "", append(rows, list...), nil, w, 100)
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
