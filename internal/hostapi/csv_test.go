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
		if got := csvEncodeOpts(c.rows, ',', false /* useCRLF */); got != c.want {
			t.Errorf("%s: encode = %q, want %q", c.name, got, c.want)
		}
		back, err := csvDecodeOpts(c.want, ',', 0, false /* trimLeadingSpace */, true /* lazyQuotes */)
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
