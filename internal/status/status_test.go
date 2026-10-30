package status

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSequence(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		r    Report
		want string
	}{
		{"idle", Report{State: Idle, App: "zemi"}, "\x1b]7501;state=idle:app=zemi\x1b\\"},
		{
			"working",
			Report{State: Working, App: "zemi", Msg: "make check"},
			"\x1b]7501;state=working:app=zemi:msg=bWFrZSBjaGVjaw==\x1b\\",
		},
		{
			"blocked with a kind",
			Report{State: Blocked, Kind: KindPermission, App: "zemi", Msg: "Run this command? make check"},
			"\x1b]7501;state=blocked:kind=permission:app=zemi:msg=UnVuIHRoaXMgY29tbWFuZD8gbWFrZSBjaGVjaw==\x1b\\",
		},
		{
			"done",
			Report{State: Done, App: "zemi", Msg: "migrate the config"},
			"\x1b]7501;state=done:app=zemi:msg=bWlncmF0ZSB0aGUgY29uZmln\x1b\\",
		},
		{"error", Report{State: Error, App: "zemi", Msg: "boom"}, "\x1b]7501;state=error:app=zemi:msg=Ym9vbQ==\x1b\\"},
		{"clear with an id", Report{State: Clear, ID: "build/test"}, "\x1b]7501;state=clear:id=build/test\x1b\\"},
		{
			"a kind outside blocked is absent",
			Report{State: Working, Kind: KindAuth, App: "zemi"},
			"\x1b]7501;state=working:app=zemi\x1b\\",
		},
		{
			"a kind that is no kind is absent",
			Report{State: Blocked, Kind: Kind(9), App: "zemi"},
			"\x1b]7501;state=blocked:app=zemi\x1b\\",
		},
		{
			"a state that is no state sends nothing: a later state never turns idle",
			Report{State: State(9), App: "zemi"},
			"",
		},
		{
			"a malformed id sends nothing: falling back to the root would overwrite it",
			Report{State: Working, ID: "bad space", App: "zemi"},
			"",
		},
		{
			"an id past eight levels sends nothing",
			Report{State: Working, ID: "a/b/c/d/e/f/g/h/i", App: "zemi"},
			"",
		},
		{
			"an app outside the character set is absent",
			Report{State: Idle, App: "my app"},
			"\x1b]7501;state=idle\x1b\\",
		},
		{
			"msg and title collapse to one line, escapes and bidi stripped",
			Report{State: Done, App: "zemi", Msg: "one\ntwo\tthree", Title: "t\u202ee"},
			"\x1b]7501;state=done:app=zemi:title=dGU=:msg=b25lIHR3byB0aHJlZQ==\x1b\\",
		},
		{
			"a control sequence alone in msg empties it and drops the pair",
			Report{State: Done, App: "zemi", Msg: "\x1b]52;c;x\x07"},
			"\x1b]7501;state=done:app=zemi\x1b\\",
		},
		{
			"a msg over the cap truncates at a rune edge",
			Report{State: Done, App: "zemi", Msg: strings.Repeat("é", 2048)},
			"\x1b]7501;state=done:app=zemi:msg=" + base64Of(strings.Repeat("é", 1024)) + "\x1b\\",
		},
	} {
		if got := c.r.Sequence(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// base64Of encodes text as the protocol carries it.
func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
