// config.go is the Go-owned config review overlay (DESIGN §8, §10.4):
// pending reflector config patches as cards; y/a accept the selected
// patch, n/x reject it, esc/q close and leave the rest queued. The card
// body is the diff (v1 patches only add rules).
package ui

import (
	"github.com/metafates/zemi/internal/hostapi"
)

// cfgReviewPrompt is one open review overlay; the answer carries the
// accepted and rejected row ids.
type cfgReviewPrompt struct {
	items   []hostapi.ConfigRow
	sel     int
	verdict cfgReviewVerdict
	answer  chan cfgReviewVerdict
}

type cfgReviewVerdict struct {
	accepted []string
	rejected []string
}

// ReviewConfigs implements hostapi.UIHooks: blocks the caller (the host's
// review goroutine) until the overlay closes.
func (a *App) ReviewConfigs(items []hostapi.ConfigRow) ([]string, []string) {
	ch := make(chan cfgReviewVerdict, 1)
	a.do(func() { a.cfgReview = &cfgReviewPrompt{items: items, answer: ch} })
	v := <-ch
	return v.accepted, v.rejected
}

func (a *App) answerCfgReview() {
	if cr := a.cfgReview; cr != nil && cr.answer != nil {
		cr.answer <- cr.verdict
	}
	a.cfgReview = nil
	a.frameGen++
}

func (a *App) cfgDecide(accept bool) {
	cr := a.cfgReview
	if cr == nil || cr.sel >= len(cr.items) {
		return
	}
	i := cr.sel
	id := cr.items[i].ID
	if accept {
		cr.verdict.accepted = append(cr.verdict.accepted, id)
	} else {
		cr.verdict.rejected = append(cr.verdict.rejected, id)
	}
	cr.items = append(cr.items[:i:i], cr.items[i+1:]...)
	cr.sel = max(0, min(cr.sel, len(cr.items)-1))
	if len(cr.items) == 0 {
		a.answerCfgReview()
	}
}

// cfgReviewBox draws the review overlay: title line, provenance, then the
// rule lines (the diff body).
func cfgReviewBox(cr *cfgReviewPrompt, w int) [][]hostapi.Segment {
	var body [][]hostapi.Segment
	if len(cr.items) == 0 {
		body = append(body, []hostapi.Segment{{Text: "no pending config proposals", HL: "Comment"}})
	}
	for i, it := range cr.items {
		cur, hl := "  ", ""
		if i == cr.sel {
			cur, hl = "› ", "Host:Bold"
		}
		meta := it.Title
		if it.From != "" {
			meta += " (from " + it.From + ")"
		}
		if it.Scrubbed {
			meta += " (secrets redacted)"
		}
		body = append(body, []hostapi.Segment{
			{Text: cur, HL: "Host:Bold"},
			{Text: meta, HL: hl},
		})
		for _, ln := range it.Lines {
			body = append(body, []hostapi.Segment{
				{Text: "    "},
				{Text: ln, HL: "Host:Bold"},
			})
		}
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, hintLine("y", "accept", "n", "reject", "esc", "leave queued"))
	return hostBox(" Config review ", "Host:Title", "", body, w)
}
