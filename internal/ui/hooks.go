package ui

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

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

// trustBox draws the Go-owned trust prompt (DESIGN §5, §8) in host
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
	body := scrollChoiceBody(h, width, nil, &tp.view, explain, width, act, false, pickHint, hintPair{"esc", "later"})
	return promptBox("Trust this "+what+"?", "", body, w, h, 100, &tp.size)
}

// bootBox draws the screen a failed boot leaves (DESIGN §8): the shipped Lua
// layer never loaded, so the runtime is hot and no theme, widget or
// config-driven text exists. Everything here is Go's, and it reads on a
// terminal with no theme at all. The two keys are the ones that work in that
// state: copy the error for a report, or quit.
func (a *App) bootBox(w, h int) [][]hostapi.Segment {
	width := min(boxInner(w), textBoxW-4)
	text := func(width int) [][]hostapi.Segment {
		rows := wrapText("zemi's own Lua layer did not load: no theme, widgets, config or model ran, and this is not your config's doing.", hostapi.ChromeHint, width)
		head, rest, _ := strings.Cut(a.bootErr.Error(), "\n")
		rows = append(append(rows, nil), wrapText(head, hostapi.ChromeDanger, width)...)
		if rest != "" {
			rows = append(rows, wrapText(rest, hostapi.ChromeHint, width)...)
		}
		rows = append(append(rows, nil), wrapText("The whole traceback is in the log: "+perm.TildePath(a.host.Dirs.LogDir()), hostapi.ChromeHint, width)...)
		return rows
	}
	var size boxSize
	body := scrollChoiceBody(h, width, nil, &a.bootView, text, width, nil, false, hintPair{"c", "copy the error"}, hintPair{"q", "quit"})
	return promptBox("Boot failed", "", body, w, h, textBoxW, &size)
}

// copyBootError puts the boot failure and its log line on the clipboard, for
// a report: the box's only answer besides quitting.
func (a *App) copyBootError() {
	if a.bootErr == nil {
		return
	}
	text := a.bootErr.Error()
	a.dispatch(uiFunc(func() tea.Cmd { return a.copied(text) }))
}

// BootError is why the boot failed, nil when it did not: main reads it once
// the program returned, when no update runs.
func (a *App) BootError() error { return a.bootErr }

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
// replaces it in place, Body and Keys too; a closed one builds and draws
// nothing, so a job refreshing each second keeps an idle zemi idle.
func (a *App) RefreshOverlay(id string, build func() hostapi.Overlay) {
	a.dispatch(uiProbe(func() {
		if a.overlayIndex(id) >= 0 {
			a.background(func() {
				o := build()
				o.Refresh = true
				a.do(func() { a.openOverlay(o) })
			})
		}
	}))
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
