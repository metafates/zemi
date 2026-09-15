package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
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
	// Title and rendered items are Lua strings: sanitize like widget text.
	o.Title = sanitize.String(o.Title)
	st := overlayState{o: o}
	for i, item := range o.Items {
		text, err := a.renderOverlayItem(o, i, item)
		if err != nil {
			a.log.Debug("ui", "overlay render_item failed", map[string]any{
				"id": o.ID, "error": err.Error(),
			})
			text = fmt.Sprint(item)
		}
		st.itemText = append(st.itemText, sanitize.String(text))
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
			return fmt.Sprintf("%-12s %s", v["keys"], v["desc"])
		case "command":
			return fmt.Sprintf("%-12s %s", v["name"], v["desc"])
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
	if moveSel(&st.selected, len(st.itemText), key) {
		return nil
	}
	switch key {
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
	badge := " " + string(st.o.Origin)
	if st.o.Path != "" {
		badge += ":" + st.o.Path
	}
	const maxItems = 10
	start := max(0, min(st.selected-maxItems/2, len(st.itemText)-maxItems))
	var body [][]hostapi.Segment
	for i := start; i < len(st.itemText) && i < start+maxItems; i++ {
		marker, hl := "  ", "Normal"
		if i == st.selected {
			marker, hl = "> ", "Title"
		}
		body = append(body, []hostapi.Segment{{Text: marker + st.itemText[i], HL: hl}})
	}
	body = append(body, []hostapi.Segment{{Text: ""}}, []hostapi.Segment{{Text: "enter select · esc close", HL: "Comment"}})
	box := hostBox(" "+st.o.Title+" ", "Title", badge+" ", body, w)
	return spliceBox(rows, box, w, headerH, chatH)
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
