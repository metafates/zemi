package hostapi

import (
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// csvNamespace is zemi.csv, beside zemi.json: encode a table of rows into
// RFC 4180 text and decode text back, the round trip a report or a handoff
// to another tool takes. Both sides are encoding/csv, so the rules are the
// standard ones: quoted fields keep their commas and newlines whole, and a
// quote beside text reads as text. Both calls take an optional options
// table: encode speaks comma and use_crlf, decode speaks comma, comment,
// trim_leading_space and lazy_quotes.
func (h *Host) csvNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		rows, err := csvRows(L.CheckTable(1))
		if err != nil {
			L.RaiseError("zemi.csv.encode: %v", err)
		}
		o := codecOpts(L)
		checkOpts(L, "zemi.csv.encode", o, "comma", "use_crlf")
		L.Push(glua.LString(csvEncodeOpts(rows, optRune(L, o, "zemi.csv.encode", "comma", ","), optBool(L, o, "zemi.csv.encode", "use_crlf", false))))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		o := codecOpts(L)
		checkOpts(L, "zemi.csv.decode", o, "comma", "comment", "trim_leading_space", "lazy_quotes")
		var comment rune
		if s := optString(L, o, "zemi.csv.decode", "comment", ""); s != "" {
			if r := []rune(s); len(r) == 1 {
				comment = r[0]
			} else {
				L.RaiseError("zemi.csv.decode: comment is %q, want one character", s)
			}
		}
		rows, err := csvDecodeOpts(L.CheckString(1),
			optRune(L, o, "zemi.csv.decode", "comma", ","),
			comment,
			optBool(L, o, "zemi.csv.decode", "trim_leading_space", false),
			optBool(L, o, "zemi.csv.decode", "lazy_quotes", true))
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// csvRows flattens one checked Lua table: a row is a table of strings and
// numbers - a csv cell has no shape for a nested table, and nil would
// silently drop a field. A bad cell names its row and field, the only
// address a 500-row report gives.
func csvRows(t *glua.LTable) ([][]string, error) {
	rows := make([][]string, 0, t.Len())
	var rerr error
	t.ForEach(func(k, row glua.LValue) {
		if rerr != nil {
			return
		}
		rt, ok := row.(*glua.LTable)
		if !ok {
			rerr = fmt.Errorf("row %s is a %s, want a table", glua.LVAsString(k), row.Type().String())
			return
		}
		fields := make([]string, 0, rt.Len())
		rt.ForEach(func(fk, v glua.LValue) {
			if rerr != nil {
				return
			}
			switch v := v.(type) {
			case glua.LString:
				fields = append(fields, string(v))
			case glua.LNumber:
				fields = append(fields, luaNumberString(v))
			default:
				rerr = fmt.Errorf("row %s field %s is a %s, want a string or a number", glua.LVAsString(k), glua.LVAsString(fk), v.Type().String())
			}
		})
		rows = append(rows, fields)
	})
	return rows, rerr
}

// luaNumberString prints a whole number without a decimal point, the way a
// spreadsheet reads it; a float keeps its shortest form.
func luaNumberString(n glua.LNumber) string {
	if n == glua.LNumber(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(float64(n), 'g', -1, 64)
}

// csvEncodeOpts writes the rows as encoding/csv writes them: the separator
// defaults to a comma and rows end in a newline, or a carriage return and
// newline on useCRLF.
func csvEncodeOpts(rows [][]string, comma rune, useCRLF bool) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Comma, w.UseCRLF = comma, useCRLF
	_ = w.WriteAll(rows) // an in-memory writer cannot fail the write
	return b.String()
}

// csvDecodeOpts reads the text with the reader's settings: the delimiter,
// the comment rune (0 for none), whether leading spaces are trimmed, and
// whether a quote beside text reads as text. Quoted fields may carry commas
// and newlines, a trailing newline ends the last row, and a blank line
// separates nothing: the reader skips it.
func csvDecodeOpts(text string, comma, comment rune, trimLeadingSpace, lazyQuotes bool) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma, r.Comment = comma, comment
	r.FieldsPerRecord = -1 // rows of one report may vary in shape
	r.TrimLeadingSpace, r.LazyQuotes = trimLeadingSpace, lazyQuotes
	return r.ReadAll()
}
