package hostapi

import "strings"

// _tldrOpen and _tldrClose bracket the summary the model appends after a
// turn-ending reply; _tldrTypo is the misspelling models sometimes emit,
// accepted silently but never mentioned in the prompt. _tldrParts are the
// prefixes of the open tags a still-streaming reply may end on.
const (
	_tldrOpen  = "<tldr>"
	_tldrClose = "</tldr>"
)

var (
	_tldrOpens  = []string{_tldrOpen, "<tlr>"}
	_tldrCloses = []string{_tldrClose, "</tlr>"}
	_tldrParts  = []string{"<tldr>", "<tldr", "<tlr>", "<tlr", "<tl", "<t", "<"}
)

// _tldrMin: a reply shorter than this reads in one glance and a summary
// would only repeat it.
const _tldrMin = 800

// _keepTldr drops the summary when the body is too small to need one; the
// tag is still peeled, so the reply just ends plain.
func _keepTldr(body, tldr string) (string, string) {
	if tldr != "" && len(body) < _tldrMin {
		return body, ""
	}
	return body, tldr
}

// SplitTLDR peels a <tldr>…</tldr> summary a reply ends with: the tag must
// close at the very end, only whitespace after it, or have opened at the very
// end and still be streaming. A tag inside a code fence never counts. body
// keeps the reply as written; tldr is "" when no trailing tag is present.
func SplitTLDR(text string) (body, tldr string) {
	open, tagLen := -1, 0
	var f fenceScanner
	at := 0
	inside := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if !inside {
			for _, tag := range _tldrOpens {
				if i := strings.LastIndex(line, tag); i >= 0 && (open < 0 || at+i >= open) {
					open, tagLen = at+i, len(tag)
				}
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
	rest := text[open+tagLen:]
	closeAt, closeLen := -1, 0
	for _, c := range _tldrCloses {
		if i := strings.Index(rest, c); i >= 0 && (closeAt < 0 || i < closeAt) {
			closeAt, closeLen = i, len(c)
		}
	}
	if closeAt < 0 {
		return _keepTldr(trimTrail(text[:open]), strings.TrimSpace(rest))
	}
	if strings.TrimSpace(rest[closeAt+closeLen:]) != "" {
		return text, ""
	}
	return _keepTldr(trimTrail(text[:open]), strings.TrimSpace(rest[:closeAt]))
}

func trimTrail(s string) string { return strings.TrimRight(s, " \t\r\n") }
