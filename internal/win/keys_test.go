package win

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestParseKeys(t *testing.T) {
	keys, err := ParseKeys("iHi<esc>:wq<CR><c-s-left><lt><a-x><S-Tab>é")
	if err != nil {
		t.Fatal(err)
	}
	want := []uv.KeyPressEvent{
		{Code: 'i', Text: "i"}, {Code: 'H', Text: "H"}, {Code: 'i', Text: "i"},
		{Code: uv.KeyEscape}, {Code: ':', Text: ":"}, {Code: 'w', Text: "w"}, {Code: 'q', Text: "q"},
		{Code: uv.KeyEnter}, {Code: uv.KeyLeft, Mod: uv.ModCtrl | uv.ModShift}, {Code: '<', Text: "<"},
		{Code: 'x', Mod: uv.ModAlt}, {Code: uv.KeyTab, Mod: uv.ModShift}, {Code: 'é', Text: "é"},
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys %+v", len(keys), keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key %d: got %+v want %+v", i, keys[i], want[i])
		}
	}
	for _, bad := range []string{"<nope>", "a<b", "<c->"} {
		if _, err := ParseKeys(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if got := modifiedSpecial(uv.KeyPressEvent{Code: uv.KeyUp, Mod: uv.ModCtrl}); got != "\x1b[1;5A" {
		t.Fatalf("ctrl+up: %q", got)
	}
	if got := modifiedSpecial(uv.KeyPressEvent{Code: uv.KeyPgDown, Mod: uv.ModShift}); got != "\x1b[6;2~" {
		t.Fatalf("shift+pgdn: %q", got)
	}
	if got := modifiedSpecial(uv.KeyPressEvent{Code: uv.KeyUp}); got != "" {
		t.Fatalf("bare key must fall to vt: %q", got)
	}
}
