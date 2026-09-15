package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
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

// formatOverlayItem renders items without render_item: strings, or
// {text, fill} maps (help, revisions).
func formatOverlayItem(item any) string {
	if m, ok := item.(map[string]any); ok {
		if s, ok := m["text"].(string); ok {
			return s
		}
	}
	if s, ok := item.(string); ok {
		return s
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
			// A row's "fill" goes into the composer, like tab in the palette;
			// "run" executes a command, honored only for builtin and user
			// overlays so a plugin row label cannot disguise a command.
			if idx < len(o.Items) {
				if m, ok := o.Items[idx].(map[string]any); ok {
					if fill, _ := m["fill"].(string); fill != "" {
						a.composer.text = sanitize.String(fill)
						a.composer.moveEnd()
					}
					if run, _ := m["run"].(string); run != "" && (o.Origin == lua.OriginBuiltin || o.Origin == lua.OriginUser) {
						name, args := splitCommand(run)
						if err := a.host.RunCommand(a.ctx(), name, args); err != nil {
							a.pushToast("error", "/"+name, err.Error())
						}
					}
				}
			}
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
	items := make([]listItem, 0, len(st.itemText))
	for _, t := range st.itemText {
		items = append(items, listItem{label: t})
	}
	body := listRows(items, st.selected, 10, "Title")
	body = append(body, []hostapi.Segment{{Text: ""}}, keyHints("enter", "select", "esc", "close"))
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
