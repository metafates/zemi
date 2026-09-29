package ui

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/hostapi"
)

// reviewAnswer waits for the host-side answer of a memory review and
// returns the rows that changed as id -> "action scope".
func reviewAnswer(t *testing.T, done chan []hostapi.MemoryRow) map[string]string {
	t.Helper()
	select {
	case rows := <-done:
		got := map[string]string{}
		for _, r := range rows {
			if r.Action != "" || r.Scope != "project" {
				got[r.ID] = r.Action + " " + string(r.Scope)
			}
		}
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
		return nil
	}
}

// openReview opens the memory screen on rows and returns the channel its
// answer arrives on.
func openReview(t *testing.T, app *App, rows []hostapi.MemoryRow) chan []hostapi.MemoryRow {
	t.Helper()
	done := make(chan []hostapi.MemoryRow, 1)
	go func() { done <- app.ReviewMemories(rows, "") }()
	waitPrompt(t, app, func() bool { return app.memReview != nil })
	return done
}

// TestGoldenMemoryReviewOverlay pins the Go-owned memory screen (DESIGN §8,
// §10.4): a keeps the suggestion, d forgets the stored memory, u undoes,
// and closing hands the decisions back by row id.
func TestGoldenMemoryReviewOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)

	items := []hostapi.MemoryRow{
		{
			ID: "m1", Scope: "project", Origin: "learned",
			Text: "the fts join needs m-qualified columns", Quote: "qualify the fts columns with m", Queued: true,
		},
		{
			ID: "m2", Scope: "user", Origin: "user",
			Text:    "the user prefers short replies",
			Updated: app.now().Add(-12 * 24 * time.Hour).UnixMilli(),
		},
	}
	done := openReview(t, app, items)

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // keep the suggestion
	app.renderSync(t)
	// the kept suggestion reads as remembered at once, without reopening
	if frame := app.Frame(); !strings.Contains(frame, "fts join") || strings.Index(frame, "Remembered") > strings.Index(frame, "fts join") {
		t.Fatalf("a kept suggestion belongs under Remembered:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // no-op on a stored memory
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}) // forget it
	app.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	app.renderSync(t)
	if frame := app.Frame(); !strings.Contains(frame, "short replies") {
		t.Fatalf("undo should bring the forgotten memory back:\n%s", frame)
	}
	app.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 2 || got["m1"] != "accept project" || got["m2"] != "delete user" {
		t.Fatalf("decisions = %v", got)
	}
	if app.memReview != nil {
		t.Fatal("overlay should be closed")
	}
}

// TestGoldenMemoryDetail: enter opens a row with its actions as buttons at
// the list's size; enter runs the selected one and goes back to the list.
func TestGoldenMemoryDetail(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 18)
	done := openReview(t, app, []hostapi.MemoryRow{
		{ID: "m1", Scope: "project", Origin: "learned", Text: "the fts join needs m-qualified columns", Quote: "qualify the fts columns with m", Queued: true},
		{ID: "m2", Scope: "user", Origin: "user", Text: "the user prefers short replies"},
	})
	app.renderSync(t)
	list := strings.Split(ansi.Strip(app.Frame()), "\r\n")
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
	edges := func(row string) string { // the cells of the box's corners
		return fmt.Sprint(strings.Index(row, "┌"), ansi.StringWidth(row[:strings.LastIndex(row, "┐")]))
	}
	if detail := strings.Split(ansi.Strip(app.Frame()), "\r\n"); edges(detail[0]) != edges(list[0]) {
		t.Fatalf("the open row moved the box:\n%s\n%s", list[0], detail[0])
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // drop
	if app.memReview.detail {
		t.Fatal("an action left the row open")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 2 || got["m1"] != "reject project" {
		t.Fatalf("answer = %v", got)
	}
}

// TestMemoryReviewFocus: a typed /memory forget opens its memory with Forget
// armed, as a typed delete opens its detail: the second press forgets it once
// the screen closes, and esc, any other key or a wheel that moves off it
// drops the arm.
func TestMemoryReviewFocus(t *testing.T) {
	for _, c := range []struct {
		name   string
		press  []string
		footer string // after the presses
		want   string // m2's answer
	}{
		{"opens armed", nil, "d again to forget · esc cancel", ""},
		{"the second press forgets", []string{"enter"}, "enter open", "delete project"},
		{"its key forgets too", []string{"d"}, "enter open", "delete project"},
		{"esc drops the arm only", []string{"esc"}, "esc back", ""},
		{"another key drops the arm", []string{"j"}, "esc back", ""},
		{"a wheel off and back drops the arm", []string{"wheel down", "wheel up"}, "esc back", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 16)
			done := make(chan []hostapi.MemoryRow, 1)
			go func() {
				done <- app.ReviewMemories([]hostapi.MemoryRow{{ID: "m1", Scope: "project", Text: "one"}, {ID: "m2", Scope: "project", Text: "two"}}, "m2")
			}()
			waitPrompt(t, app, func() bool { return app.memReview != nil })
			for _, k := range c.press {
				if b, ok := map[string]tea.MouseButton{"wheel down": tea.MouseWheelDown, "wheel up": tea.MouseWheelUp}[k]; ok {
					app.Update(tea.MouseWheelMsg{Button: b})
					continue
				}
				pressKeys(app, k)
			}
			if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, c.footer) {
				t.Fatalf("no %q in:\n%s", c.footer, frame)
			}
			for app.memReview != nil {
				pressKeys(app, "esc")
			}
			if got := reviewAnswer(t, done); strings.TrimSpace(got["m2"]) != c.want || len(got) > 1 {
				t.Fatalf("answer = %v", got)
			}
		})
	}
}

// TestMemoryReviewEdit: o hands the row back for editing and the screen
// the host opens next starts on it; a forget suggestion has no text to edit.
func TestMemoryReviewEdit(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	rows := []hostapi.MemoryRow{
		{ID: "f1", Scope: "project", Origin: "learned", Text: "forget: note", Queued: true, Forget: true},
		{ID: "m1", Scope: "project", Origin: "user", Text: "note"},
		{ID: "m2", Scope: "project", Origin: "user", Text: "other"},
	}
	done := openReview(t, app, rows)
	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if app.memReview == nil {
		t.Fatal("o closed the screen on a forget suggestion")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if got := <-done; !got[1].Edit || got[0].Edit || got[2].Edit {
		t.Fatalf("edit rows: %+v", got)
	}
	done = openReview(t, app, rows)
	if app.memReview.at() != 1 {
		t.Fatalf("reopened on row %d", app.memReview.at())
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	<-done
}

// TestMemoryReviewAnswers: what the screen hands back once the keys ran
// and esc closed it.
func TestMemoryReviewAnswers(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []hostapi.MemoryRow
		keys []string
		want map[string]string
	}{
		{"a suggestion kept, then dropped, was never stored: rejected", []hostapi.MemoryRow{{ID: "m1", Scope: "project", Origin: "learned", Text: "note", Queued: true}}, []string{"a", "d"}, map[string]string{"m1": "reject project"}},
		{"s leaves a forget suggestion in its memory's scope", []hostapi.MemoryRow{
			{ID: "f1", Scope: "project", Origin: "learned", Text: "forget: note", Queued: true, Forget: true},
			{ID: "s1", Scope: "project", Origin: "learned", Text: "idea", Queued: true},
		}, []string{"s", "j", "s", "j"}, map[string]string{"s1": " user"}},
		{"undoing every action decides nothing", []hostapi.MemoryRow{{ID: "m1", Scope: "project", Origin: "user", Text: "note"}}, []string{"d", "u"}, map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 70, 16)
			done := openReview(t, app, c.rows)
			pressKeys(app, append(c.keys, "esc")...)
			if got := reviewAnswer(t, done); !maps.Equal(got, c.want) || app.memReview != nil {
				t.Fatalf("answer = %v, want %v", got, c.want)
			}
		})
	}
}

// TestMemoryReviewForgetAndWrap: a forget suggestion offers "a forget",
// keeping it takes the memory it forgets off the screen, u brings both
// back, and keeping the last suggestion goes back to the first one left.
func TestMemoryReviewForgetAndWrap(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 24)
	done := openReview(t, app, []hostapi.MemoryRow{
		{ID: "s1", Scope: "project", Origin: "learned", Text: "idea", Queued: true},
		{ID: "f1", Scope: "project", Origin: "learned", Text: "forget: note", Queued: true, Forget: true, Supersedes: "m1"},
		{ID: "m1", Scope: "project", Origin: "user", Text: "note"},
	})
	pressKeys(app, "j", "enter")
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "[a] Forget") || !strings.Contains(frame, "[d] Keep") {
		t.Fatalf("forget buttons:\n%s", frame)
	}
	pressKeys(app, "esc")
	for _, key := range []rune{'a', 'u', 'a'} {
		app.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		app.renderSync(t)
		frame := ansi.Strip(app.Frame())
		if kept := key == 'a'; strings.Contains(frame, "forget: note") == kept || strings.Contains(frame, "Remembered (1)") == kept {
			t.Fatalf("after %c the forget and its memory show wrong:\n%s", key, frame)
		}
	}
	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // back on s1
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 2 || got["s1"] != "accept project" || got["f1"] != "accept project" {
		t.Fatalf("decisions = %v", got)
	}
}

// TestMemoryReviewScopeToggle: s moves a row between this project and
// everywhere, u takes it back, and the answer carries the new scope.
func TestMemoryReviewScopeToggle(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := openReview(t, app, []hostapi.MemoryRow{
		{ID: "m2", Scope: "project", Origin: "learned", Text: "idea", Queued: true},
		{ID: "m1", Scope: "project", Origin: "user", Text: "note"},
	})
	app.Update(tea.KeyPressMsg{Code: 's', Text: "s"}) // m2, the suggestion, goes everywhere
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "everywhere · learned") {
		t.Fatalf("toggled scope not shown:\n%s", frame)
	}
	app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	app.Update(tea.KeyPressMsg{Code: 's', Text: "s"}) // m1 goes everywhere
	app.Update(tea.KeyPressMsg{Code: 's', Text: "s"}) // and back
	app.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	app.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // opens the row, esc goes back
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.memReview == nil || app.memReview.detail {
		t.Fatal("enter and esc left the memory list")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 1 || got["m2"] != "accept user" {
		t.Fatalf("answer = %v", got)
	}
}

func TestMemoryReviewEmpty(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)
	done := openReview(t, app, nil)

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	pressKeys(app, "/")
	if app.memReview.active {
		t.Fatal("/ started a filter over no rows")
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := reviewAnswer(t, done); len(got) != 0 {
		t.Fatalf("empty review returned %v", got)
	}
}

// TestMemoryReviewFilter: a typing filter's footer names only enter and
// esc, enter keeps the query and gives the keys back, and backspace on a
// kept query drops nothing.
func TestMemoryReviewFilter(t *testing.T) {
	app, _ := bootAgentApp(t, 90, 16)
	openReview(t, app, []hostapi.MemoryRow{{ID: "m1", Scope: "project", Origin: "user", Text: "the fts join needs qualified columns"}})
	footer := func() string {
		app.renderSync(t)
		return ansi.Strip(app.Frame())
	}
	pressKeys(app, "/fts")
	if frame := footer(); !strings.Contains(frame, "enter done · esc clear") || strings.Contains(frame, "d drop") {
		t.Fatalf("typing filter hints:\n%s", frame)
	}
	pressKeys(app, "enter", "backspace")
	if frame := footer(); app.memReview.detail || len(app.memReview.items) != 1 || !strings.Contains(frame, "enter open") || !strings.Contains(frame, "esc clear") {
		t.Fatalf("kept filter:\n%s", frame)
	}
	pressKeys(app, "enter")
	if !app.memReview.detail {
		t.Fatal("enter on a kept filter opens the row")
	}
}

// TestMemoryReviewLongList: memories read whole and as plain text, so
// markdown cannot hide part of one, the window scrolls to the selection, and the box keeps its place.
func TestMemoryReviewLongList(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 24)
	var items []hostapi.MemoryRow
	for i := range 12 {
		items = append(items, hostapi.MemoryRow{ID: string(rune('a' + i)), Scope: "user", Origin: "user", Text: "short note"})
	}
	items = append(items, hostapi.MemoryRow{
		ID: "z", Scope: "project", Origin: "learned",
		Text: "run **go test** <!-- hidden --> before `make golden`; " + strings.Repeat("word ", 30) + "tail-marker",
	})
	done := openReview(t, app, items)

	app.renderSync(t)
	first := app.Frame()
	for range items {
		app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"tail-marker", "run **go test** <!-- hidden --> before", "`make golden`", "this project · learned", "█"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("frame lacks %q:\n%s", want, frame)
		}
	}
	edges := func(frame string) string {
		var out []string
		for i, l := range strings.Split(frame, "\n") {
			if strings.Contains(l, "┌") || strings.Contains(l, "└") {
				out = append(out, fmt.Sprint(i, ansi.StringWidth(strings.TrimRight(l, " \r"))))
			}
		}
		return strings.Join(out, "|")
	}
	if a, b := edges(ansi.Strip(first)), edges(frame); a != b {
		t.Fatalf("the box moved: %q vs %q", a, b)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	reviewAnswer(t, done)
}
