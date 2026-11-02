// memories_test.go drives /memories in the real UI: l opens a digest's
// children as an overlay of their own and h climbs back, a leaf's enter
// leaves the chat standing at the message it covers, and the browser reads
// the live view.

package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
)

// seedMemoriesApp boots an app whose log and view hold six messages: two
// merged lines, a built leaf and an unbuilt one, the chat drawn stamped.
func seedMemoriesApp(t *testing.T, w, h int) (*App, *hostapi.Host) {
	t.Helper()
	app, host := bootAgentApp(t, w, h)
	testStore(t, host) // the harness boots with no project dir: give it a store
	reloadUI(t, host)  // the builtin commands, /memories among them
	for range 6 {
		if _, err := host.DB.AppendLog("user", strings.Repeat("row ", 200), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if _, err := host.DB.InsertNode(0, int64(i), "msg "+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := host.DB.InsertNode(1, 0, "msgs 0-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.DB.InsertNode(1, 1, "msgs 2-3"); err != nil {
		t.Fatal(err)
	}
	if n, err := host.DB.MergeViewLines([]store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}}); err != nil || n != 2 {
		t.Fatalf("the merges: %d applied, %v", n, err)
	}
	tr := make([]hostapi.ChatMessage, 6)
	for i := range tr {
		tr[i] = hostapi.ChatMessage{Role: "user", Text: strings.Repeat("row ", 200), Seq: int64(i)}
	}
	host.SetTranscript(tr)
	return app, host
}

// pump settles the key path's background hops: a command runs on the worker
// and posts its screen, which lands as another message.
func pump(t *testing.T, app *App) {
	t.Helper()
	for range 3 {
		for app.Pump() {
		}
		app.Frame()
		app.renderSync(t)
	}
	app.Frame()
}

// TestMemoriesZoomKeys: l opens a line's two children as an overlay of their
// own and h climbs back; the rows around them never move.
func TestMemoriesZoomKeys(t *testing.T) {
	t.Parallel()
	app, host := seedMemoriesApp(t, 100, 24)
	if err := host.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	pump(t, app)
	st := &app.overlays[len(app.overlays)-1]
	if st.o.ID != "memories" || !st.o.Follow {
		t.Fatalf("the browser = %s follow=%v", st.o.ID, st.o.Follow)
	}
	if got := st.items[0].label; got != "msgs 0-1" {
		t.Fatalf("the first row reads %q", got)
	}
	st.selected = 0 // the browser opened on the tail's line
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump(t, app)
	st = &app.overlays[len(app.overlays)-1]
	if st.o.ID != "memories zoom 1 0" || st.o.Title != "Memories / 0+2" {
		t.Fatalf("l opened %s %q, want the line's children", st.o.ID, st.o.Title)
	}
	if got := st.items[0].label + "|" + st.items[1].label; got != "msg 0|msg 1" {
		t.Fatalf("l reads %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	pump(t, app)
	st = &app.overlays[len(app.overlays)-1]
	if len(app.overlays) != 1 || st.o.ID != "memories" {
		t.Fatalf("h left %d overlays, top %s", len(app.overlays), st.o.ID)
	}
	if got := st.items[0].label; got != "msgs 0-1" {
		t.Fatalf("h reads %q", got)
	}
}

// TestMemoriesLeafPreview: enter on a leaf reads the messages it covers over
// the browser, and the chat stays where it was - the browser is an overview;
// finding a turn's place on the screen is /messages' job.
func TestMemoriesLeafPreview(t *testing.T) {
	t.Parallel()
	app, host := seedMemoriesApp(t, 100, 24)
	app.renderSync(t)
	app.Frame()
	if err := host.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	pump(t, app)
	ov := &app.overlays[len(app.overlays)-1]
	// the built leaf, message 4
	ov.selected = 2
	app.Frame()
	before := app.scrolls[regionChat].focus
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, app)
	st := &app.overlays[len(app.overlays)-1]
	if st.o.ID != "memories range 0 4" {
		t.Fatalf("enter on a leaf opened %s, want the preview", st.o.ID)
	}
	if len(app.overlays) != 2 {
		t.Fatalf("%d overlays open, want the browser under the preview", len(app.overlays))
	}
	if got := app.scrolls[regionChat].focus; got != before {
		t.Fatalf("enter moved the chat to %d, want it left at %d", got, before)
	}
}

// TestMemoriesPreviewBody: the v-preview of a leaf shows the logged rows,
// each head carrying the row's date, and never the gone-sentence the body's
// own content rules out.
func TestMemoriesPreviewBody(t *testing.T) {
	t.Parallel()
	app, host := seedMemoriesApp(t, 100, 24)
	if err := host.MemoriesCommand("view 0 1"); err != nil {
		t.Fatal(err)
	}
	pump(t, app)
	st := &app.overlays[len(app.overlays)-1]
	if st.o.ID != "memories range 0 1" {
		t.Fatalf("the preview = %s", st.o.ID)
	}
	if st.o.Empty != "" {
		t.Fatalf("a preview with rows carried Empty %q", st.o.Empty)
	}
	if !strings.Contains(st.o.Body, "user 20") || !strings.Contains(st.o.Body, ": row") {
		t.Fatalf("the preview body = %q", st.o.Body)
	}
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "row row") {
		t.Fatalf("the preview drew:\n%s", frame)
	}
	if strings.Contains(frame, "gone from this store") {
		t.Fatalf("the gone-sentence drew under real content:\n%s", frame)
	}
}

// TestMemoriesLeafZoomSaysSo: a leaf advertises no zoom, and l on it opens
// the messages it covers, the screen enter opens.
func TestMemoriesLeafZoomSaysSo(t *testing.T) {
	t.Parallel()
	app, host := seedMemoriesApp(t, 100, 24)
	if err := host.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	pump(t, app)
	ov := &app.overlays[len(app.overlays)-1]
	ov.selected = 2 // the built leaf 4+1
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump(t, app)
	if len(app.overlays) != 2 {
		t.Fatalf("%d overlays after l on a leaf", len(app.overlays))
	}
	if top := app.overlays[len(app.overlays)-1].o; top.ID != "memories range 0 4" {
		t.Fatalf("l on a leaf opened %q", top.ID)
	}
}
