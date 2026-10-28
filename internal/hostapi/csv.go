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
		L.Push(glua.LString(csvEncodeOpts(rows, optRune(L, o, "comma", ","), optBool(L, o, "use_crlf", false))))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		o := codecOpts(L)
		var comment rune
		if s := optString(L, o, "comment", ""); s != "" {
			if r := []rune(s); len(r) == 1 {
				comment = r[0]
			} else {
				L.RaiseError("comment is %q, want one character", s)
			}
		}
		rows, err := csvDecodeOpts(L.CheckString(1),
			optRune(L, o, "comma", ","),
			comment,
			optBool(L, o, "trim_leading_space", false),
			optBool(L, o, "lazy_quotes", true))
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
// silently drop a field.
func csvRows(t *glua.LTable) ([][]string, error) {
	rows := make([][]string, 0, t.Len())
	var rerr error
	t.ForEach(func(_, row glua.LValue) {
		if rerr != nil {
			return
		}
		rt, ok := row.(*glua.LTable)
		if !ok {
			rerr = fmt.Errorf("row %v is not a table", row)
			return
		}
		fields := make([]string, 0, rt.Len())
		rt.ForEach(func(_, v glua.LValue) {
			if rerr != nil {
				return
			}
			switch v := v.(type) {
			case glua.LString:
				fields = append(fields, string(v))
			case glua.LNumber:
				fields = append(fields, luaNumberString(v))
			default:
				rerr = fmt.Errorf("field %v is a %s, want a string or a number", v, v.Type().String())
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

// csvEncode writes the rows as encoding/csv writes them: the separator
// defaults to a comma and rows end in a newline.
func csvEncode(rows [][]string) string {
	return csvEncodeOpts(rows, ',', false)
}

// csvEncodeOpts is csvEncode with the writer's settings: the delimiter and
// whether rows end in a carriage return and newline.
func csvEncodeOpts(rows [][]string, comma rune, useCRLF bool) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Comma, w.UseCRLF = comma, useCRLF
	_ = w.WriteAll(rows) // an in-memory writer cannot fail the write
	return b.String()
}

// csvDecode reads the text as encoding/csv does: quoted fields may carry
// commas and newlines, a quote beside text reads as text, and a trailing
// newline ends the last row. A blank line separates nothing: the reader
// skips it.
func csvDecode(text string) ([][]string, error) {
	return csvDecodeOpts(text, ',', 0, false, true)
}

// csvDecodeOpts reads the text with the reader's settings: the delimiter,
// the comment rune (0 for none), whether leading spaces are trimmed, and
// whether a quote beside text reads as text.
func csvDecodeOpts(text string, comma, comment rune, trimLeadingSpace, lazyQuotes bool) ([][]string, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma, r.Comment = comma, comment
	r.FieldsPerRecord = -1 // rows of one report may vary in shape
	r.TrimLeadingSpace, r.LazyQuotes = trimLeadingSpace, lazyQuotes
	return r.ReadAll()
}
