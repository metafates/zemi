package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
)

// overlayState is one entry of the overlay stack (DESIGN §8): a Lua (or
// Go-built hostapi.Overlay) plus local list selection.
type overlayState struct {
	o        hostapi.Overlay
	itemText []string // rendered item lines (render_item applied at open)
	selected int
}

// openOverlay pushes an overlay, rendering its items up front (render_item
// runs once per item on the queue; the frame never calls Lua).
func (a *App) openOverlay(o hostapi.Overlay) {
	st := overlayState{o: o}
	for i, item := range o.Items {
		text, err := a.renderOverlayItem(o, i, item)
		if err != nil {
			a.log.Debug("ui", "overlay render_item failed", map[string]any{
				"id": o.ID, "error": err.Error(),
			})
			text = fmt.Sprint(item)
		}
		st.itemText = append(st.itemText, text)
	}
	if len(st.itemText) == 0 {
		st.itemText = []string{"(empty)"}
	}
	a.overlays = append(a.overlays, st)
}

// renderOverlayItem renders one item through the overlay's render_item
// Lua callback; overlays without one fall back to plain formatting.
func (a *App) renderOverlayItem(o hostapi.Overlay, idx int, item any) (string, error) {
	if o.RenderItem == nil {
		return formatOverlayItem(item), nil
	}
	res, err := a.host.RT.Call(a.ctx(), o.Origin, o.RenderItem, item)
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		return "", nil
	}
	if s, ok := res[0].(string); ok {
		return s, nil
	}
	return fmt.Sprint(res[0]), nil
}

// formatOverlayItem renders plain data items (Go-built overlays like help).
func formatOverlayItem(item any) string {
	switch v := item.(type) {
	case string:
		return v
	case map[string]any:
		switch v["kind"] {
		case "key":
			return fmt.Sprintf("%-8s %-10s %s", v["mode"], v["keys"], v["desc"])
		case "command":
			return fmt.Sprintf(":%-14s %s", v["name"], v["desc"])
		}
	}
	return fmt.Sprint(item)
}

func (a *App) closeOverlayByID(id string) bool {
	for i := range a.overlays {
		if a.overlays[i].o.ID == id {
			a.overlays = append(a.overlays[:i], a.overlays[i+1:]...)
			return true
		}
	}
	return false
}

// overlayKey routes keys to the topmost overlay; returns a cmd when Lua
// ran (select/close paths).
func (a *App) overlayKey(key string) tea.Cmd {
	st := &a.overlays[len(a.overlays)-1]
	o := st.o
	switch key {
	case "up", "k":
		if st.selected > 0 {
			st.selected--
		}
	case "down", "j":
		if st.selected < len(st.itemText)-1 {
			st.selected++
		}
	case "enter":
		idx := st.selected
		if o.OnSelect != nil {
			// SelectOverlay runs on_select then closes via the hook.
			a.host.SelectOverlay(a.ctx(), o, idx)
		} else {
			a.closeOverlayByID(o.ID)
			a.frameGen++
		}
	case "esc", "q":
		a.closeOverlayByID(o.ID)
		a.frameGen++
		if o.OnClose != nil {
			a.host.CancelOverlay(a.ctx(), o)
		}
	}
	return nil
}

// spliceOverlay composes the overlay box into the frame rows, centered
// over the chat area, with the Go-stamped origin badge (DESIGN §8).
func (a *App) spliceOverlay(rows [][]hostapi.Segment, st overlayState, w, headerH, chatH int) [][]hostapi.Segment {
	box := a.overlayBox(st, w)
	return spliceBox(rows, box, w, headerH, chatH)
}

func (a *App) overlayBox(st overlayState, w int) [][]hostapi.Segment {
	title := " " + st.o.Title + " "
	badge := " " + string(st.o.Origin)
	if st.o.Path != "" {
		badge += ":" + st.o.Path
	}
	badge += " "

	inner := 0
	for _, it := range st.itemText {
		if n := ansi.StringWidth(it); n > inner {
			inner = n
		}
	}
	bw := ansi.StringWidth(title) + ansi.StringWidth(badge) + 4
	if inner+6 > bw {
		bw = inner + 6
	}
	if bw > w-4 {
		bw = w - 4
	}
	if bw < 12 {
		bw = 12
	}

	var box [][]hostapi.Segment
	box = append(box, []hostapi.Segment{
		{Text: "┌", HL: "Comment"},
		{Text: title, HL: "Title"},
		{Text: strings.Repeat("─", max0(bw-2-ansi.StringWidth(title)-ansi.StringWidth(badge)))},
		{Text: badge, HL: "Comment"},
		{Text: "┐", HL: "Comment"},
	})

	visible := len(st.itemText)
	maxItems := 10
	start := 0
	if visible > maxItems {
		start = st.selected - maxItems/2
		if start < 0 {
			start = 0
		}
		if start+maxItems > visible {
			start = visible - maxItems
		}
		visible = maxItems
	}
	for i := 0; i < visible; i++ {
		idx := start + i
		text := ansi.Truncate(st.itemText[idx], bw-6, "…")
		marker := "  "
		hl := "Normal"
		if idx == st.selected {
			marker = "> "
			hl = "Title"
		}
		pad := bw - 6 - ansi.StringWidth(text)
		box = append(box, []hostapi.Segment{
			{Text: "│ ", HL: "Comment"},
			{Text: marker, HL: hl},
			{Text: text + strings.Repeat(" ", max0(pad)), HL: hl},
			{Text: " │", HL: "Comment"},
		})
	}
	box = append(box, []hostapi.Segment{
		{Text: "└", HL: "Comment"},
		{Text: strings.Repeat("─", bw-4)},
		{Text: "enter select · esc close ", HL: "Comment"},
		{Text: "┘", HL: "Comment"},
	})
	return box
}

// spliceBox writes box rows centered over the chat area, replacing the
// covered rows entirely.
func spliceBox(rows [][]hostapi.Segment, box [][]hostapi.Segment, w, headerH, chatH int) [][]hostapi.Segment {
	if len(rows) == 0 || len(box) == 0 {
		return rows
	}
	chatTop := headerH
	chatBottom := headerH + chatH
	if chatBottom > len(rows) {
		chatBottom = len(rows)
	}
	bh := len(box)
	if bh > chatBottom-chatTop {
		bh = chatBottom - chatTop
	}
	if bh <= 0 {
		return rows
	}
	y0 := chatTop + (chatBottom-chatTop-bh)/2
	for i := 0; i < bh; i++ {
		bw := segsWidth(box[i])
		x0 := (w - bw) / 2
		if x0 < 1 {
			x0 = 1
		}
		row := []hostapi.Segment{}
		if x0 > 0 {
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", x0)})
		}
		row = append(row, box[i]...)
		if pad := w - x0 - bw; pad > 0 {
			row = append(row, hostapi.Segment{Text: strings.Repeat(" ", pad)})
		}
		rows[y0+i] = row
	}
	return rows
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
