package sanitize

import "testing"

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in, str, log string
	}{
		{"plain ascii", "a\tb\nc", "a\tb\nc", "a b c"},
		{"csi and osc stripped", "a\x1b[31mb\x1b]0;t\x07c", "abc", "abc"},
		{"short escapes keep the next char", "\x1b7Hi\x1b8 a\x1b(Bb", "Hi ab", "Hi ab"},
		{"c0 and del become spaces", "a\x00b\x7fc", "a b c", "a b c"},
		{"bidi override alone is stripped", "a\u202eb", "ab", "ab"},
		{"encoded c1 csi becomes space", "a\u009bb", "a b", "a b"},
		{"raw c1 byte becomes space", "a\x9bb", "a b", "a b"},
		{"em dash and wide runes kept", "a \u2014 \u4e16", "a \u2014 \u4e16", "a \u2014 \u4e16"},
		{"zero width space kept", "a\u200bb", "a\u200bb", "a\u200bb"},
		{"bidi isolates and marks stripped", "a\u2066b\u2069\u200fc", "abc", "abc"},
		{"tag and annotation runes stripped", "a\U000e0041\ufff9b", "ab", "ab"},
		{"zwj emoji sequence kept", "\U0001f469\u200d\U0001f4bb", "\U0001f469\u200d\U0001f4bb", "\U0001f469\u200d\U0001f4bb"},
		{"zwnj kept, soft hyphen stripped", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645 co\u00adop", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645 coop", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645 coop"},
	}
	for _, c := range cases {
		if got := String(c.in); got != c.str {
			t.Errorf("%s: String = %q, want %q", c.name, got, c.str)
		}
		if got := LogLine(c.in); got != c.log {
			t.Errorf("%s: LogLine = %q, want %q", c.name, got, c.log)
		}
	}
}
