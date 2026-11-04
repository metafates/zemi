package ui

import (
	"strings"
	"time"
)

// The discovery tips: one is zemi's parting line, picked by the hour of the
// host's clock so it does not change between runs in the same hour.
var _tips = []struct{ key, text string }{
	{"/btw ", "asks a side question the conversation does not keep"},
	{"@", "attaches files, dirs and skills to the prompt"},
	{"/goal ", "keeps working until a condition holds"},
	{"/review", "has several agents review your changes"},
	{"alt+o", "browses the chat's whole memory, however old"},
}

// Tip is this hour's discovery tip as one line, without the "tip:" prefix.
func Tip(now time.Time) string {
	t := _tips[int(now.Unix()/3600)%len(_tips)]
	return strings.TrimRight(t.key, " ") + " " + t.text
}
