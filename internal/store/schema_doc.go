package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// schemaPurpose is the one-line purpose of the tables whose role the schema
// cannot say; a table not here renders bare.
var schemaPurpose = map[string]string{
	"sessions":   `the "main" billing and todos anchor, one row per subagent run`,
	"messages":   "a subagent run's persisted rows, read back in rowid order",
	"todos":      "a session's todo list",
	"tool_calls": "one row per tool call a message carried",
	"skills":     "the skill index synced at boot: path, source and enabled flag per name",
	"kv":         "small state: saved config picks, goals, recap hints, MCP state, user storage",
	"log":        "the eternal chat: one append-only row per message, seq its dense 0-based index",
	"nodes":      "the summary tree: node(l, i) covers the 2^l messages from i*2^l on, one line each",
	"view_lines": "the persisted view, oldest first; a line whose node is missing renders as a placeholder",
	"leader":     "the lease: the one window that may write this project's chat",
}

// SchemaDoc renders the schema the embedded migrations build: one bullet per
// table with its columns, types and defaults. It opens a throwaway store, so
// the render is the real schema, renames included.
func SchemaDoc() (string, error) {
	dir, err := os.MkdirTemp("", "zemi-schema")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	db, err := Open(filepath.Join(dir, "zemi.db"))
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version'
		ORDER BY name`)
	tables, err := collect(rows, err, scanString)
	if err != nil {
		return "", fmt.Errorf("store: schema doc: %w", err)
	}
	var b strings.Builder
	for _, t := range tables {
		rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk
			FROM pragma_table_info(?) ORDER BY cid`, t)
		cols, err := collect(rows, err, func(r scanner) (string, error) {
			var name, typ string
			var notnull, pk int
			var dflt any
			if err := r.Scan(&name, &typ, &notnull, &dflt, &pk); err != nil {
				return "", err
			}
			col := name
			if typ != "" {
				col += " " + typ
			}
			if pk != 0 {
				col += " pk"
			}
			if notnull != 0 {
				col += " NOT NULL"
			}
			if dflt != nil {
				col += fmt.Sprintf(" DEFAULT %v", dflt)
			}
			return col, nil
		})
		if err != nil {
			return "", fmt.Errorf("store: schema doc %s: %w", t, err)
		}
		b.WriteString("- `" + t + "`")
		if p, ok := schemaPurpose[t]; ok {
			b.WriteString(" - " + p)
		}
		b.WriteString("\n  " + strings.Join(cols, ", ") + "\n")
	}
	return b.String(), nil
}
