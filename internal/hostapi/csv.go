package hostapi

import (
	"fmt"
	"strconv"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// csvNamespace is zemi.csv, beside zemi.json: encode a table of rows into
// RFC 4180 text and decode text back, the round trip a report or a handoff
// to another tool takes.
func (h *Host) csvNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("encode", L.NewFunction(func(L *glua.LState) int {
		rows, err := csvRows(L.CheckTable(1))
		if err != nil {
			L.RaiseError("zemi.csv.encode: %v", err)
		}
		L.Push(glua.LString(csvEncode(rows)))
		return 1
	}))
	tb.RawSetString("decode", L.NewFunction(func(L *glua.LState) int {
		rows, err := csvDecode(L.CheckString(1))
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

// csvEncode writes RFC 4180: a field carrying a comma, a quote, a carriage
// return or a newline is quoted, and a quote inside doubles.
func csvEncode(rows [][]string) string {
	var b strings.Builder
	for _, row := range rows {
		for i, field := range row {
			if i > 0 {
				b.WriteByte(',')
			}
			if strings.ContainsAny(field, ",\"\n\r") {
				b.WriteByte('"')
				b.WriteString(strings.ReplaceAll(field, `"`, `""`))
				b.WriteByte('"')
			} else {
				b.WriteString(field)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// csvDecode reads RFC 4180: quoted fields may carry commas, doubled quotes
// and newlines; a trailing newline ends the last row, and a final
// unterminated quoted field is the error it would be for any reader.
func csvDecode(text string) ([][]string, error) {
	var rows [][]string
	var row []string
	var field strings.Builder
	inQuotes, fieldQuoted := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inQuotes:
			if c == '"' {
				if i+1 < len(text) && text[i+1] == '"' {
					field.WriteByte('"')
					i++
				} else {
					inQuotes = false
				}
			} else {
				field.WriteByte(c)
			}
		case c == '"' && field.Len() == 0 && !fieldQuoted:
			inQuotes, fieldQuoted = true, true
		case c == ',':
			row = append(row, field.String())
			field, fieldQuoted = strings.Builder{}, false
		case c == '\n' || c == '\r':
			if c == '\r' && i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			rows = append(rows, append(row, field.String()))
			row, field = nil, strings.Builder{}
			fieldQuoted = false
		default:
			field.WriteByte(c)
		}
	}
	if inQuotes {
		return nil, fmt.Errorf("a quoted field never closes")
	}
	if field.Len() > 0 || fieldQuoted || len(row) > 0 {
		rows = append(rows, append(row, field.String()))
	}
	return rows, nil
}
