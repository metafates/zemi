package hostapi

import (
	"strings"
	"testing"
)

// TestCSVRoundTrip pins the RFC 4180 contract zemi.csv promises: quoting on
// the separators, doubled quotes, whole numbers without a decimal point, and
// the round trip through the reader it shares the rules with.
func TestCSVRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name string
		rows [][]string
		want string
	}{
		{"plain", [][]string{{"name", "age"}, {"ada", "36"}}, "name,age\nada,36\n"},
		{"whole numbers stay whole", [][]string{{"3", "-12", "0"}}, "3,-12,0\n"},
		{"a comma quotes", [][]string{{"a,b", "c"}}, `"a,b",c` + "\n"},
		{"a quote doubles", [][]string{{`say "hi"`}}, "\"say \"\"hi\"\"\"\n"},
		{"a newline quotes", [][]string{{"one", "two\nlines"}}, "one,\"two\nlines\"\n"},
		{"an empty field", [][]string{{"", "x", ""}}, ",x,\n"},
	} {
		if got := csvEncode(c.rows); got != c.want {
			t.Errorf("%s: encode = %q, want %q", c.name, got, c.want)
		}
		back, err := csvDecode(c.want)
		if err != nil {
			t.Errorf("%s: decode: %v", c.name, err)
			continue
		}
		if len(back) != len(c.rows) {
			t.Errorf("%s: decode holds %d rows, want %d", c.name, len(back), len(c.rows))
			continue
		}
		for i, row := range back {
			if strings.Join(row, "|") != strings.Join(c.rows[i], "|") {
				t.Errorf("%s: row %d = %q, want %q", c.name, i, row, c.rows[i])
			}
		}
	}
}

// TestCSVDecodeEdges pins the reader: crlf ends a row, a quoted field keeps
// its commas and newlines whole, a final field without a newline lands, a
// blank line separates nothing, and a quote beside text reads as text - the
// reader's own rules, not zemi's.
func TestCSVDecodeEdges(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     [][]string
	}{
		{"crlf", "a,b\r\nc,d\r\n", [][]string{{"a", "b"}, {"c", "d"}}},
		{"quoted commas and newline", "\"x,y\"\n\"a\nb\"\n", [][]string{{"x,y"}, {"a\nb"}}},
		{"doubled quotes in a quoted field", `"he said ""ok"""` + "\n", [][]string{{"he said \"ok\""}}},
		{"no trailing newline", "a,b", [][]string{{"a", "b"}}},
		{"a blank line separates nothing", "a\n\nb\n", [][]string{{"a"}, {"b"}}},
		{"a quote beside text reads as text", "ab\"c\n", [][]string{{`ab"c`}}},
		{"an unterminated quote reads to the end", "\"never closed\n", [][]string{{"never closed\n"}}},
	} {
		got, err := csvDecode(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %d rows, want %d", c.name, len(got), len(c.want))
			continue
		}
		for i, row := range got {
			if strings.Join(row, "|") != strings.Join(c.want[i], "|") {
				t.Errorf("%s: row %d = %q, want %q", c.name, i, row, c.want[i])
			}
		}
	}
}

// TestCSVOptions: the options table reaches the reader and the writer - a
// semicolon report round-trips, a comment line separates nothing, and a
// crlf writer pairs with a crlf read.
func TestCSVOptions(t *testing.T) {
	text := csvEncodeOpts([][]string{{"name", "age"}, {"ada", "36"}}, ';', true)
	if text != "name;age\r\nada;36\r\n" {
		t.Fatalf("encode = %q", text)
	}
	rows, err := csvDecodeOpts(text, ';', 0, false, true)
	if err != nil || len(rows) != 2 || rows[1][1] != "36" {
		t.Fatalf("decode = %v, %v", rows, err)
	}
	// a comment line separates nothing and leading spaces stay when asked
	rows, err = csvDecodeOpts("# a note\nname; 36\n", ';', '#', false, true)
	if err != nil || len(rows) != 1 || rows[0][1] != " 36" {
		t.Fatalf("commented decode = %v, %v", rows, err)
	}
	rows, err = csvDecodeOpts("name;36\n", ';', 0, true, true)
	if err != nil || rows[0][1] != "36" {
		t.Fatalf("trimmed decode = %v, %v", rows, err)
	}
	// strict quotes turn the lenient read into an error
	if _, err = csvDecodeOpts("ab\"c\n", ',', 0, false, false); err == nil {
		t.Error("a bare quote decoded with lazy quotes off")
	}
}
