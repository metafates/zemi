package hostapi

import "strings"

// _tldrOpen and _tldrClose bracket the summary the model appends after a
// turn-ending reply; _tldrParts are the prefixes of _tldrOpen a still-streaming
// reply may end on.
const (
	_tldrOpen  = "<tldr>"
	_tldrClose = "</tldr>"
)

var _tldrParts = []string{"<tldr>", "<tldr", "<tl", "<t", "<"}

// SplitTLDR peels a <tldr>…</tldr> summary a reply ends with: the tag must
// close at the very end, only whitespace after it, or have opened at the very
// end and still be streaming. A tag inside a code fence never counts. body
// keeps the reply as written; tldr is "" when no trailing tag is present.
func SplitTLDR(text string) (body, tldr string) {
	open := -1
	var f fenceScanner
	at := 0
	inside := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if !inside {
			if i := strings.LastIndex(line, _tldrOpen); i >= 0 {
				open = at + i
			}
		}
		inside = f.line(line, 0)
		at += len(line)
	}
	if open < 0 {
		for _, p := range _tldrParts {
			if strings.HasSuffix(text, p) {
				return trimTrail(text[:len(text)-len(p)]), ""
			}
		}
		return text, ""
	}
	rest := text[open+len(_tldrOpen):]
	before, after, found := strings.Cut(rest, _tldrClose)
	if found {
		if strings.TrimSpace(after) != "" {
			return text, ""
		}
		return trimTrail(text[:open]), strings.TrimSpace(before)
	}
	return trimTrail(text[:open]), strings.TrimSpace(rest)
}

func trimTrail(s string) string { return strings.TrimRight(s, " \t\r\n") }
