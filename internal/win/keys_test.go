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

func TestKeyEncoding(t *testing.T) {
	for _, c := range []struct {
		name  string
		kitty bool // the program turned on kitty keyboard flag 1
		k     uv.KeyPressEvent
		want  string
	}{
		{"shifted text", false, uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModShift}, "A"},
		{"alt+shift+a", false, uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModAlt | uv.ModShift}, "\x1bA"},
		{"shift+enter", false, uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModShift}, "\r"},
		{"ctrl+alt+enter", false, uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModCtrl | uv.ModAlt}, "\x1b\r"},
		{"ctrl+/", false, uv.KeyPressEvent{Code: '/', Mod: uv.ModCtrl}, "\x1f"},
		{"ctrl+2", false, uv.KeyPressEvent{Code: '2', Mod: uv.ModCtrl}, "\x00"},
		{"ctrl+up", false, uv.KeyPressEvent{Code: uv.KeyUp, Mod: uv.ModCtrl}, "\x1b[1;5A"},
		{"shift+pgdn", false, uv.KeyPressEvent{Code: uv.KeyPgDown, Mod: uv.ModShift}, "\x1b[6;2~"},
		{"bare enter is vt's", false, uv.KeyPressEvent{Code: uv.KeyEnter}, ""},
		{"bare up is vt's", false, uv.KeyPressEvent{Code: uv.KeyUp}, ""},
		{"ctrl+a is vt's", false, uv.KeyPressEvent{Code: 'a', Mod: uv.ModCtrl}, ""},
		{"kitty esc", true, uv.KeyPressEvent{Code: uv.KeyEscape}, "\x1b[27u"},
		{"kitty ctrl+i", true, uv.KeyPressEvent{Code: 'i', Mod: uv.ModCtrl}, "\x1b[105;5u"},
		{"kitty alt+shift+a", true, uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModAlt | uv.ModShift}, "\x1b[97;4u"},
		{"kitty shift+enter", true, uv.KeyPressEvent{Code: uv.KeyEnter, Mod: uv.ModShift}, "\x1b[13;2u"},
		{"kitty shift+tab", true, uv.KeyPressEvent{Code: uv.KeyTab, Mod: uv.ModShift}, "\x1b[9;2u"},
		{"kitty super+s", true, uv.KeyPressEvent{Code: 's', Mod: uv.ModSuper}, "\x1b[115;9u"},
		{"kitty keeps plain enter", true, uv.KeyPressEvent{Code: uv.KeyEnter}, ""},
		{"kitty keeps shifted text", true, uv.KeyPressEvent{Code: 'a', Text: "A", Mod: uv.ModShift | uv.ModCapsLock}, ""},
		{"kitty keeps ctrl+up", true, uv.KeyPressEvent{Code: uv.KeyUp, Mod: uv.ModCtrl}, ""},
	} {
		got := legacyKey(c.k)
		if c.kitty {
			got = kittyKey(c.k)
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
