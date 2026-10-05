package brain

// Reconcile-on-open migration (section 8): the frozen memory table's live
// rows compose into memories.md, idempotently, the ledger's import entries
// the permanent tombstone.

import (
	"cmp"
	"slices"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// Reconcile imports a partition's frozen-table rows into memories.md (§8).
// A live row whose id the ledger holds is done - the import entries are the
// permanent tombstone that keeps a hand-deleted line from re-importing -
// and the missing ones compose into the file: a deterministic merge of the
// existing lines and the new rows, sorted by last-write date desc then id,
// written through writeMemories so the file's malformed lines survive (D6)
// and a crash re-runs to identical bytes. The bytes are written first and
// the entries follow them: a crash between the two re-runs, while the
// reverse order would skip the compose forever. Superseded rows are not
// composed and stay in the frozen table.
func Reconcile(p Partition, rows []*store.Memory, at time.Time) error {
	if p.Zero() {
		return nil
	}
	unlock, err := Lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	lg, err := p.OpenLedger()
	if err != nil {
		return err
	}
	defer func() { _ = lg.Close() }()
	lines, raw, err := readMemories(p)
	if err != nil {
		return err
	}
	ops, err := lg.ops()
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(ops))
	for _, op := range ops {
		seen[op.ID] = true
	}
	var added []Line
	var fresh []*store.Memory
	for _, m := range rows {
		if m.SupersededBy != "" || seen[m.ID] {
			continue
		}
		fresh = append(fresh, m)
		if slices.ContainsFunc(lines, func(l Line) bool { return l.ID == m.ID }) {
			continue
		}
		added = append(added, Line{ID: m.ID, Origin: m.Origin, Date: day(time.UnixMilli(m.UpdatedAt)), Text: m.Text})
	}
	if len(added) > 0 {
		lines = append(lines, added...)
		slices.SortFunc(lines, func(a, b Line) int {
			if c := b.Date.Compare(a.Date); c != 0 {
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
		if err := writeMemories(p, raw, lines); err != nil {
			return err
		}
	}
	for _, m := range fresh {
		op := &Op{
			Op: OpImport, ID: m.ID, Quote: m.Quote, SessionID: m.ProvenanceSessionID,
			Hash: textHash(m.Text), Scope: p.Kind.Scope(), Origin: m.Origin, TS: at.UnixMilli(),
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		}
		if err := lg.Insert(op); err != nil {
			return err
		}
	}
	return nil
}
