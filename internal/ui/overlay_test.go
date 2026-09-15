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

// TestOverlayFilter: typing narrows the rows (all words, any case), enter
// runs the highlighted match, backspace widens again, and esc first clears
// the filter, then closes.
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

	typeText("t")
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
	typeText("gam")
	a.overlayKey("enter", "")
	if len(a.overlays) != 0 || a.composer.text != "gamma three" {
		t.Fatalf("enter: overlays=%d composer=%q", len(a.overlays), a.composer.text)
	}
}

// TestModelPickerFilter: typing narrows the picker by id or display name
// and enter picks the highlighted match.
func TestModelPickerFilter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	answer := make(chan modelPick, 1)
	a.Update(modelPickMsg{items: []hostapi.ModelOption{
		{ID: "anthropic/claude-sonnet", Display: "Sonnet"},
		{ID: "openai/gpt-5", Display: "GPT-5"},
		{ID: "deepseek/deepseek-chat", Display: "DeepSeek V3"},
	}, answer: answer})
	for _, r := range "v3" {
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
