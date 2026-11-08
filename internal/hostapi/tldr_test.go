package hostapi

import "testing"

func TestSplitTLDR(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		text string
		body string
		tldr string
	}{
		{"no tag", "plain reply", "plain reply", ""},
		{"tag at the end", "body text\n<tldr>all green, two files</tldr>", "body text", "all green, two files"},
		{"whitespace after the tag", "body\n<tldr>done</tldr>  \n", "body", "done"},
		{"mid-text tag does not split", "a <tldr>x</tldr> and more words", "a <tldr>x</tldr> and more words", ""},
		{"body after the close does not split", "a <tldr>x</tldr> more", "a <tldr>x</tldr> more", ""},
		{"tag in a fence does not split", "```\n<tldr>x</tldr>\n```", "```\n<tldr>x</tldr>\n```", ""},
		{"streaming open", "body\n<tldr>still writing", "body", "still writing"},
		{"streaming partial opener", "body <tl", "body", ""},
		{"streaming lone bracket", "body 2 < 3 <", "body 2 < 3", ""},
		{"tag on the last line only after a fence", "```\ncode\n```\n<tldr>after</tldr>", "```\ncode\n```", "after"},
		{"misspelled tag", "body text\n<tlr>all green, two files</tlr>", "body text", "all green, two files"},
		{"misspelled tag, correct close", "body text\n<tlr>all green</tldr>", "body text", "all green"},
		{"streaming misspelled opener", "body <tlr", "body", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, tldr := SplitTLDR(c.text)
			if body != c.body || tldr != c.tldr {
				t.Fatalf("SplitTLDR(%q) = %q, %q, want %q, %q", c.text, body, tldr, c.body, c.tldr)
			}
		})
	}
}
