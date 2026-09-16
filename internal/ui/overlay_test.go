package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// TestToggleOverlayStaysOpen: in a Reload overlay (skills) space and enter
// run the row and rebuild the list in place, keeping the selection;
// ctrl+j moves like j.
func TestToggleOverlayStaysOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	on := false
	items := func() []any {
		mark := "○ b"
		if on {
			mark = "● b"
		}
		return []any{map[string]any{"text": "a"}, map[string]any{"text": mark, "fill": "picked"}}
	}
	a.openOverlay(hostapi.Overlay{ID: "t", Origin: lua.OriginBuiltin, Items: items(), Reload: func() []any { on = !on; return items() }})
	a.overlayKey("ctrl+j", "")
	for _, key := range []string{"space", "enter"} {
		wasOn := on
		a.overlayKey(key, map[string]string{"space": " "}[key])
		if len(a.overlays) != 1 || a.overlays[0].selected != 1 || on == wasOn {
			t.Fatalf("%s: overlays=%d selected=%d on=%v", key, len(a.overlays), a.overlays[0].selected, on)
		}
	}
	if got := a.overlays[0].itemText[1]; got != "○ b" || a.composer.text != "picked" {
		t.Fatalf("row=%q composer=%q", got, a.composer.text)
	}
}

// TestReloadClosesLuaOverlays: an overlay the interpreter owns must not
// survive a reload. Its callbacks are dropped as stale, so leaving it on
// screen reads as a frozen picker; Go-built ones keep working and stay.
func TestReloadClosesLuaOverlays(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openOverlay(hostapi.Overlay{ID: "lua", Origin: lua.OriginUser, Gen: 1, Items: []any{"a"}})
	a.openOverlay(hostapi.Overlay{ID: "go", Origin: lua.OriginBuiltin, Items: []any{"b"}})

	a.host.PurgeGeneration()
	if len(a.overlays) != 1 || a.overlays[0].o.ID != "go" {
		t.Fatalf("overlays after purge: %+v", a.overlays)
	}
}

// TestOverlayFilter: "/" starts a filter that narrows the rows (all words,
// any case), backspace widens again, esc ends the filter before closing,
// j moves outside a filter, and enter runs the highlighted match.
func TestOverlayFilter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	var items []any
	for _, s := range []string{"alpha one", "Beta two", "gamma three"} {
		items = append(items, map[string]any{"text": s, "fill": s})
	}
	a.openOverlay(hostapi.Overlay{ID: "f", Origin: lua.OriginBuiltin, Items: items})
	typeText := func(s string) {
		for _, r := range s {
			a.overlayKey(string(r), string(r))
		}
	}
	visible := func() int { return len(a.overlays[0].visible) }

	typeText("/t")
	if visible() != 2 {
		t.Fatalf("t: %d rows", visible())
	}
	typeText("wo b")
	if visible() != 1 {
		t.Fatalf("'two b': %d rows", visible())
	}
	a.overlayKey("backspace", "")
	a.overlayKey("backspace", "")
	if q := a.overlays[0].query; q != "two" || visible() != 1 {
		t.Fatalf("backspace: query %q, %d rows", q, visible())
	}
	a.overlayKey("esc", "")
	if len(a.overlays) != 1 || visible() != 3 {
		t.Fatalf("esc clears the filter first: overlays=%d rows=%d", len(a.overlays), visible())
	}
	a.overlayKey("j", "j")
	if a.overlays[0].selected != 1 || a.overlays[0].query != "" {
		t.Fatalf("j outside a filter: selected=%d query=%q", a.overlays[0].selected, a.overlays[0].query)
	}
	typeText("/gam")
	a.overlayKey("enter", "")
	if len(a.overlays) != 0 || a.composer.text != "gamma three" {
		t.Fatalf("enter: overlays=%d composer=%q", len(a.overlays), a.composer.text)
	}
}

// TestTopChromeTakesInput: keys, ctrl+c and the wheel reach the prompt
// drawn on top, never one hidden under it.
func TestTopChromeTakesInput(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	perm := make(chan permAnswer, 1)
	a.trust = &trustPrompt{answer: make(chan string, 1)}
	a.permAsk = &permAskPrompt{answer: perm}
	a.handleKey(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if a.permAsk != nil || a.trust == nil || (<-perm).ok {
		t.Fatal("n should deny the permission prompt drawn over the trust prompt")
	}

	a.openModelPick(make([]hostapi.ModelOption, 3), "", make(chan modelPick, 1))
	a.memReview = &memReviewPrompt{items: make([]hostapi.MemoryRow, 3), actions: map[string]string{}}
	a.wheelChrome(1)
	if a.memReview.sel != 1 || a.modelPick.selected != 0 {
		t.Fatalf("wheel: memory sel=%d picker sel=%d", a.memReview.sel, a.modelPick.selected)
	}
}

// TestModelPickerFilter: typing narrows the picker by id or display name
// and enter picks the highlighted match.
func TestModelPickerFilter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	answer := make(chan modelPick, 1)
	a.openModelPick([]hostapi.ModelOption{
		{ID: "anthropic/claude-sonnet", Display: "Sonnet"},
		{ID: "openai/gpt-5", Display: "GPT-5"},
		{ID: "deepseek/deepseek-chat", Display: "DeepSeek V3"},
	}, "", answer)
	for _, r := range "/v3" {
		a.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if n := len(a.modelPick.visible); n != 1 {
		t.Fatalf("v3 matches %d models", n)
	}
	a.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := <-answer; !got.ok || got.id != "deepseek/deepseek-chat" {
		t.Fatalf("picked %+v", got)
	}
}
