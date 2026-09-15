package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/hostapi"
)

// TestGoldenConfigReviewOverlay pins the Go-owned config review overlay
// (DESIGN §8, §10.4): pending patches as cards with their rule lines as
// the diff body, provenance stamped by Go, and row ids returned as the
// verdict.
func TestGoldenConfigReviewOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	items := []hostapi.ConfigRow{
		{ID: "c1", Title: "Deny rm -rf in bash", From: "sess-1", Scrubbed: true,
			Lines: []string{
				"+ deny tool.bash rm -rf *",
				"    except rm -f build",
			}},
		{ID: "c2", Title: "Ask before force push", From: "sess-1",
			Lines: []string{"+ ask git.push --force*"}},
	}
	done := make(chan cfgReviewVerdict, 1)
	go func() { done <- verdictOf(app.ReviewConfigs(items)) }()
	waitPrompt(t, app, func() bool { return app.cfgReview != nil })

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}) // accept c1
	if app.cfgReview == nil {
		t.Fatal("overlay should stay open while a card remains")
	}
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}) // reject c2 -> closes
	select {
	case v := <-done:
		if len(v.accepted) != 1 || v.accepted[0] != "c1" {
			t.Fatalf("accepted=%v", v.accepted)
		}
		if len(v.rejected) != 1 || v.rejected[0] != "c2" {
			t.Fatalf("rejected=%v", v.rejected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
	}
	if app.cfgReview != nil {
		t.Fatal("overlay should be closed")
	}
}

func verdictOf(a, b []string) cfgReviewVerdict {
	return cfgReviewVerdict{accepted: a, rejected: b}
}

// TestConfigReviewEscape covers esc: no verdicts and the overlay closes
// (the host leaves the patch queued).
func TestConfigReviewEscape(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan cfgReviewVerdict, 1)
	go func() {
		a, b := app.ReviewConfigs([]hostapi.ConfigRow{
			{ID: "c1", Title: "t", Lines: []string{"+ deny s p"}},
		})
		done <- cfgReviewVerdict{accepted: a, rejected: b}
	}()
	waitPrompt(t, app, func() bool { return app.cfgReview != nil })

	app.renderSync(t)
	frame := app.Frame()
	if !strings.Contains(frame, "Config review") || !strings.Contains(frame, "+ deny s p") {
		t.Fatalf("overlay body missing:\n%s", frame)
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case v := <-done:
		if len(v.accepted) != 0 || len(v.rejected) != 0 {
			t.Fatalf("escape produced verdicts: %+v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("review answer not delivered")
	}
	if app.cfgReview != nil {
		t.Fatal("overlay should be closed")
	}
}
