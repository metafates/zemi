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
}

func TestLegacyKey(t *testing.T) {
	for _, c := range []struct {
		name string
		k    uv.KeyPressEvent
		want string
	}{
		{"shifted text", uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModShift}, "A"},
		{"alt+shift+a", uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModAlt | uv.ModShift}, "\x1bA"},
		{"shift+enter", uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModShift}, "\r"},
		{"ctrl+alt+enter", uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModCtrl | uv.ModAlt}, "\x1b\r"},
		{"ctrl+/", uv.KeyPressEvent{Code: '/', Mod: uv.ModCtrl}, "\x1f"},
		{"ctrl+2", uv.KeyPressEvent{Code: '2', Mod: uv.ModCtrl}, "\x00"},
		{"ctrl+up", uv.KeyPressEvent{Code: uv.KeyUp, Mod: uv.ModCtrl}, "\x1b[1;5A"},
		{"shift+pgdn", uv.KeyPressEvent{Code: uv.KeyPgDown, Mod: uv.ModShift}, "\x1b[6;2~"},
		{"bare enter is vt's", uv.KeyPressEvent{Code: uv.KeyEnter}, ""},
		{"bare up is vt's", uv.KeyPressEvent{Code: uv.KeyUp}, ""},
		{"ctrl+a is vt's", uv.KeyPressEvent{Code: 'a', Mod: uv.ModCtrl}, ""},
	} {
		if got := legacyKey(c.k); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
