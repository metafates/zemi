// errlog.go keeps the recent runtime errors behind the statusline badge
// (DESIGN §11): widget renders, keymaps and event handlers record here,
// and /errors lists them newest first and clears the badge.
package hostapi

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

const errLogMax = 50

type errEntry struct {
	at     time.Time
	source string
	msg    string
}

type errLog struct {
	mu      sync.Mutex
	entries []errEntry
	unseen  bool
}

// RecordError keeps one error (sanitized) and raises the badge.
func (h *Host) RecordError(source, msg string) {
	l := &h.errs
	l.mu.Lock()
	l.entries = append(l.entries, errEntry{time.Now(), source, sanitize.String(msg)})
	if len(l.entries) > errLogMax {
		l.entries = l.entries[len(l.entries)-errLogMax:]
	}
	l.unseen = true
	l.mu.Unlock()
	h.refresh()
}

// HasUnseenErrors reports whether errors arrived since /errors last opened.
func (h *Host) HasUnseenErrors() bool {
	h.errs.mu.Lock()
	defer h.errs.mu.Unlock()
	return h.errs.unseen
}

// OpenErrors lists the recent errors newest first and clears the badge.
func (h *Host) OpenErrors() error {
	l := &h.errs
	l.mu.Lock()
	entries := append([]errEntry(nil), l.entries...)
	l.unseen = false
	l.mu.Unlock()
	h.refresh()
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI attached")
	}
	if len(entries) == 0 {
		h.Notify("info", "errors", "no errors recorded")
		return nil
	}
	items := make([]any, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%s  %-16s %s", e.at.Format("15:04:05"), e.source, strings.Join(strings.Fields(e.msg), " ")),
		})
	}
	ui.OpenOverlay(Overlay{ID: "errors", Title: "Errors", Origin: lua.OriginBuiltin, Items: items})
	return nil
}
