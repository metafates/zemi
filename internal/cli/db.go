package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// dbRowCap is how many rows the text table prints; a clipped one says so.
const dbRowCap = 500

// dbCellCap is a text cell's width, past which it shows an ellipsis; --json
// carries every value whole.
const dbCellCap = 60

func dbCommand(o *options) *cobra.Command {
	var asJSON, global *bool
	cmd := &cobra.Command{
		Use:   "db <sql> [args...]",
		Short: "run one read-only query against zemi's store; the args after it bind to ?1, ?2, ...; --global for the global store",
		Args:  cobra.MinimumNArgs(1),
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
	var table [][]string
	for rows.Next() {
		if len(table) >= dbRowCap {
			fmt.Fprintf(errw, "zemi: showing the first %d rows; add LIMIT or narrow the query\n", dbRowCap)
			break
		}
		if err := rows.Scan(into...); err != nil {
			return err
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			cells[i] = dbCell(v)
		}
		table = append(table, cells)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = ansi.StringWidth(c)
	}
	for _, r := range table {
		for i, cell := range r {
			width[i] = max(width[i], ansi.StringWidth(cell))
		}
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", w-ansi.StringWidth(s)) }
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(out, "  ")
		}
		if i < len(cols)-1 {
			fmt.Fprint(out, pad(c, width[i]))
		} else {
			fmt.Fprint(out, c)
		}
	}
	fmt.Fprintln(out)
	for _, r := range table {
		for i, cell := range r {
			if i > 0 {
				fmt.Fprint(out, "  ")
			}
			if i < len(cols)-1 {
				fmt.Fprint(out, pad(cell, width[i]))
			} else {
				fmt.Fprint(out, cell)
			}
		}
		fmt.Fprintln(out)
	}
	return nil
}

// dbCell renders one value for the text table: NULL an empty cell, a blob as
// its bytes, a one-line string capped at dbCellCap.
func dbCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		x = strings.ReplaceAll(x, "\n", "\\n")
		x = strings.ReplaceAll(x, "\t", "\\t")
		return ansi.Truncate(x, dbCellCap, "...")
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
