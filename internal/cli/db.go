package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// _dbRowCap is how many rows the text table prints; a clipped one says so.
const _dbRowCap = 500

// _dbCellCap is a text cell's width, past which it cuts and names the cut;
// --json carries every value whole.
const _dbCellCap = 60

func dbCommand(o *options) *cobra.Command {
	var asJSON, global *bool
	cmd := &cobra.Command{
		Use:   "db <sql> [args...]",
		Short: "run one read-only query against zemi's store",
		Long:  "run one read-only query against zemi's store; the args after it bind to ?1, ?2, ...; --global for the global store",
		Args:  minArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dirs := o.dirs
			path := store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot())
			if *global {
				path = store.ProjectDBPath(dirs.StateHome, "")
			}
			db, err := store.OpenRead(path)
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no store at %s; zemi creates it on its first run there", perm.TildePath(path))
			}
			if err != nil {
				return err
			}
			defer db.Close()
			return runDBQuery(c.OutOrStdout(), c.ErrOrStderr(), db, args[0], args[1:], *asJSON)
		},
	}
	asJSON = jsonFlag(cmd)
	global = cmd.Flags().Bool("global", false, "read the global store instead of this project's")
	return cmd
}

// runDBQuery runs one query and prints its rows as an aligned text table or,
// with --json, one JSON object per row. Text caps at dbRowCap rows; JSON
// streams every row, since a script asked for them. The params bind to the
// query's numbered placeholders as text; column affinity compares them
// against typed columns.
func runDBQuery(out, errw io.Writer, db *store.DB, query string, params []string, asJSON bool) error {
	bound := make([]any, len(params))
	for i, p := range params {
		bound[i] = p
	}
	rows, err := db.Query(query, bound...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	into := make([]any, len(cols))
	for i := range vals {
		into[i] = &vals[i]
	}
	if asJSON {
		for rows.Next() {
			if err := rows.Scan(into...); err != nil {
				return err
			}
			row := make(map[string]any, len(cols))
			for i, c := range cols {
				row[c] = vals[i]
			}
			writeJSON(out, row)
		}
		return rows.Err()
	}
	tcols := make([]tableCol, len(cols))
	for i, c := range cols {
		tcols[i] = tableCol{head: c}
	}
	t := table{p: colorprofile.Detect(out, os.Environ()), head: true, cols: tcols}
	for rows.Next() {
		if len(t.rows) >= _dbRowCap {
			fmt.Fprintf(errw, "zemi: showing the first %d rows; add LIMIT or narrow the query\n", _dbRowCap)
			break
		}
		if err := rows.Scan(into...); err != nil {
			return err
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			cells[i] = dbCell(v)
		}
		t.row(cells...)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	t.print(out)
	return nil
}

// dbCell renders one value for the text table: NULL an empty cell, a blob as
// its sanitized bytes, a one-line string capped at dbCellCap, the tail naming
// the cut.
func dbCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		// stored strings are model-influenced: strip them at this boundary
		x = sanitize.String(x)
		x = strings.ReplaceAll(x, "\n", "\\n")
		x = strings.ReplaceAll(x, "\t", "\\t")
		if ansi.StringWidth(x) <= _dbCellCap {
			return x
		}
		// the tail's own width comes out of the cap, so the count it names
		// and the tail's length settle over a pass or two
		tail := "..."
		for range 3 {
			kept := strings.TrimSuffix(ansi.Truncate(x, _dbCellCap, tail), tail)
			next := fmt.Sprintf("... (+%d chars; --json for the whole)",
				utf8.RuneCountInString(x)-utf8.RuneCountInString(kept))
			if next == tail {
				break
			}
			tail = next
		}
		return ansi.Truncate(x, _dbCellCap, tail)
	case []byte:
		return dbCell(string(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
