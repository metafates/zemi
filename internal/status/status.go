// Package status speaks the Program Status Protocol (OSC 7501): a program's
// reports of what it is doing — working, blocked on the user, done, failed,
// idle — to the terminal that hosts it. zemi reports its own state on the
// root record; a terminal that knows the sequence keeps a record per id.
package status

import (
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/metafates/zemi/internal/sanitize"
)

// OSC is the operating system command number of the protocol; a report is
// OSC 7501 ; pairs ST and a feature-detection query is OSC 7501 ; ? ST.
const OSC = 7501

// State is one of the protocol's states. State(0) is no state and fails to
// encode, so a report whose state was never set sends nothing. Clear removes
// records instead of setting one.
type State uint8

const (
	_ State = iota
	Idle
	Working
	Done
	Blocked
	Error
	Clear
)

// String is the state's name on the wire, "" for one that is no state: an
// unrecognized state makes a terminal ignore the whole report, so a state
// added later never turns into idle on an older terminal.
func (s State) String() string {
	switch s {
	case Idle:
		return "idle"
	case Working:
		return "working"
	case Done:
		return "done"
	case Blocked:
		return "blocked"
	case Error:
		return "error"
	case Clear:
		return "clear"
	default:
		return ""
	}
}

// Kind is what a blocked report waits on. Kind(0) is none and drops the pair,
// the way the protocol treats an unknown kind.
type Kind uint8

const (
	_ Kind = iota
	KindPermission
	KindPlan
	KindQuestion
	KindAuth
)

// String is the kind's name on the wire.
func (k Kind) String() string {
	switch k {
	case KindPermission:
		return "permission"
	case KindPlan:
		return "plan"
	case KindQuestion:
		return "question"
	case KindAuth:
		return "auth"
	default:
		return ""
	}
}

// The protocol's caps: a report breaking one is discarded whole by the
// terminal, so the encoder refuses or truncates to stay inside them.
const (
	_appLimit = 32   // app: bytes
	_idLimit  = 128  // id: bytes in all
	_titleMax = 192  // title: decoded bytes (256 encoded)
	_msgMax   = 2048 // msg: decoded bytes (2732 encoded)
	_seqMax   = 4096 // the whole sequence
)

// Report is one report: a record's complete next state. Msg and Title are
// plain text, encoded here.
type Report struct {
	State State
	ID    string // "" addresses the root record
	Kind  Kind   // with Blocked only
	App   string
	Title string
	Msg   string
}

// Sequence encodes the report as the escape sequence a program writes to the
// terminal, "" when the report breaks the protocol (no known state, or a
// malformed id): a terminal would discard it whole.
func (r Report) Sequence() string {
	state := r.State.String()
	if state == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\x1b]")
	b.WriteString(strconv.Itoa(OSC))
	b.WriteString(";state=")
	b.WriteString(state)
	if r.ID != "" {
		if !validID(r.ID) {
			return ""
		}
		b.WriteString(":id=")
		b.WriteString(r.ID)
	}
	if r.State == Blocked {
		if kind := r.Kind.String(); kind != "" {
			b.WriteString(":kind=")
			b.WriteString(kind)
		}
	}
	if r.App != "" && len(r.App) <= _appLimit && validApp(r.App) {
		b.WriteString(":app=")
		b.WriteString(r.App)
	}
	if r.Title != "" {
		if v, ok := encode(r.Title, _titleMax); ok {
			b.WriteString(":title=")
			b.WriteString(v)
		}
	}
	if r.Msg != "" {
		if v, ok := encode(r.Msg, _msgMax); ok {
			b.WriteString(":msg=")
			b.WriteString(v)
		}
	}
	b.WriteString("\x1b\\")
	if b.Len() > _seqMax {
		return ""
	}
	return b.String()
}

// encode makes text a single-line base64 value within limit decoded bytes:
// control characters and escapes collapse away (a terminal discards a report
// whose decoded text carries one), then the text truncates at a rune edge.
func encode(text string, limit int) (string, bool) {
	text = sanitize.Line(text)
	if text == "" {
		return "", false
	}
	if len(text) > limit {
		text = text[:limit]
		for { // a rune the cut split in half: drop bytes until the tail decodes
			r, size := utf8.DecodeLastRuneInString(text)
			if r != utf8.RuneError || size > 1 {
				break
			}
			if size == len(text) {
				text = ""
				break
			}
			text = text[:len(text)-size]
		}
		text = strings.TrimRight(text, " ")
	}
	return base64.StdEncoding.EncodeToString([]byte(text)), true
}

// validID holds the record-id grammar: segments of 1..32 word bytes,
// at most 8 levels deep.
func validID(id string) bool {
	if len(id) > _idLimit {
		return false
	}
	for seg := range strings.SplitSeq(id, "/") {
		if len(seg) == 0 || len(seg) > 32 || !validApp(seg) {
			return false
		}
	}
	return strings.Count(id, "/") < 8
}

// validApp holds the app and id-segment character set: [A-Za-z0-9_.+-].
func validApp(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '+' || c == '-':
		default:
			return false
		}
	}
	return s != ""
}
