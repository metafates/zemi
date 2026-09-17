package win

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// ParseKeys reads neovim key notation: plain characters are typed as they
// are, and <...> names a key, with modifiers as in <c-w>, <a-x>, <s-tab>,
// <c-s-left>. <lt> is a literal <. A < that does not open a known name is
// an error rather than silent text, so a typo never lands in the program.
func ParseKeys(s string) ([]uv.KeyPressEvent, error) {
	var out []uv.KeyPressEvent
	for i := 0; i < len(s); {
		if s[i] != '<' {
			r, size := utf8.DecodeRuneInString(s[i:])
			out = append(out, uv.KeyPressEvent{Code: r, Text: string(r)})
			i += size
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			return nil, fmt.Errorf("keys: unclosed < at %d; write <lt> for a literal <", i)
		}
		k, err := parseKeyName(s[i+1 : i+end])
		if err != nil {
			return nil, err
		}
		out = append(out, k)
		i += end + 1
	}
	return out, nil
}

var keyNames = map[string]rune{
	"esc": uv.KeyEscape, "escape": uv.KeyEscape,
	"cr": uv.KeyEnter, "enter": uv.KeyEnter, "return": uv.KeyEnter,
	"nl":  'j', // ctrl+j, a newline
	"tab": uv.KeyTab, "bs": uv.KeyBackspace, "backspace": uv.KeyBackspace,
	"space": uv.KeySpace, "del": uv.KeyDelete, "delete": uv.KeyDelete,
	"ins": uv.KeyInsert, "insert": uv.KeyInsert,
	"up": uv.KeyUp, "down": uv.KeyDown, "left": uv.KeyLeft, "right": uv.KeyRight,
	"home": uv.KeyHome, "end": uv.KeyEnd,
	"pageup": uv.KeyPgUp, "pgup": uv.KeyPgUp, "pagedown": uv.KeyPgDown, "pgdn": uv.KeyPgDown, "pgdown": uv.KeyPgDown,
	"f1": uv.KeyF1, "f2": uv.KeyF2, "f3": uv.KeyF3, "f4": uv.KeyF4, "f5": uv.KeyF5, "f6": uv.KeyF6,
	"f7": uv.KeyF7, "f8": uv.KeyF8, "f9": uv.KeyF9, "f10": uv.KeyF10, "f11": uv.KeyF11, "f12": uv.KeyF12,
	"lt": '<', "gt": '>', "bar": '|',
}

func parseKeyName(name string) (uv.KeyPressEvent, error) {
	orig := name
	name = strings.ToLower(name)
	var mod uv.KeyMod
	for {
		switch {
		case strings.HasPrefix(name, "c-") || strings.HasPrefix(name, "ctrl-"):
			mod |= uv.ModCtrl
		case strings.HasPrefix(name, "a-") || strings.HasPrefix(name, "m-") || strings.HasPrefix(name, "alt-"):
			mod |= uv.ModAlt
		case strings.HasPrefix(name, "s-") || strings.HasPrefix(name, "shift-"):
			mod |= uv.ModShift
		default:
			goto done
		}
		name = name[strings.IndexByte(name, '-')+1:]
	}
done:
	if name == "nl" {
		return uv.KeyPressEvent{Code: 'j', Mod: uv.ModCtrl | mod}, nil
	}
	if code, ok := keyNames[name]; ok {
		k := uv.KeyPressEvent{Code: code, Mod: mod}
		if mod == 0 && (code == '<' || code == '>' || code == '|') {
			k.Text = string(code)
		}
		return k, nil
	}
	if r, size := utf8.DecodeRuneInString(name); size == len(name) && size > 0 {
		k := uv.KeyPressEvent{Code: r, Mod: mod}
		if mod == 0 || mod == uv.ModShift {
			k.Text = string(r)
		}
		return k, nil
	}
	return uv.KeyPressEvent{}, fmt.Errorf("keys: unknown key <%s>; names are esc, cr, tab, bs, space, del, up, down, left, right, home, end, pgup, pgdn, f1-f12, lt, with c-, a-, s- modifiers", orig)
}

// ctrlBytes are the control bytes xterm sends for ctrl with a digit or /,
// which vt drops.
var ctrlBytes = map[rune]string{'/': "\x1f", '2': "\x00", '3': "\x1b", '4': "\x1c", '5': "\x1d", '6': "\x1e", '7': "\x1f", '8': "\x7f"}

// legacyKey encodes the keys vt gets wrong the xterm way; "" leaves k to vt.
// Printable keys go as their text so shifted and composed characters survive.
func legacyKey(k uv.KeyPressEvent) string {
	esc := ""
	if k.Mod&uv.ModAlt != 0 {
		esc = "\x1b"
	}
	switch rest := k.Mod &^ uv.ModAlt; {
	case k.Text != "" && rest&(uv.ModCtrl|uv.ModMeta) == 0:
		return esc + k.Text
	case k.Code == uv.KeyEnter && rest != 0:
		return esc + "\r" // a legacy terminal cannot tell shift or ctrl+enter from enter
	case rest == uv.ModCtrl && ctrlBytes[k.Code] != "":
		return esc + ctrlBytes[k.Code]
	}
	return modifiedSpecial(k)
}

// modifiedSpecial encodes a special key with ctrl, shift or alt the xterm
// way (vt only knows the bare keys); "" when k is not one.
func modifiedSpecial(k uv.KeyPressEvent) string {
	m := 1
	if k.Mod&uv.ModShift != 0 {
		m++
	}
	if k.Mod&uv.ModAlt != 0 {
		m += 2
	}
	if k.Mod&uv.ModCtrl != 0 {
		m += 4
	}
	if m == 1 {
		return ""
	}
	if final, ok := map[rune]string{uv.KeyUp: "A", uv.KeyDown: "B", uv.KeyRight: "C", uv.KeyLeft: "D",
		uv.KeyHome: "H", uv.KeyEnd: "F", uv.KeyF1: "P", uv.KeyF2: "Q", uv.KeyF3: "R", uv.KeyF4: "S"}[k.Code]; ok {
		return fmt.Sprintf("\x1b[1;%d%s", m, final)
	}
	if n, ok := map[rune]int{uv.KeyInsert: 2, uv.KeyDelete: 3, uv.KeyPgUp: 5, uv.KeyPgDown: 6,
		uv.KeyF5: 15, uv.KeyF6: 17, uv.KeyF7: 18, uv.KeyF8: 19, uv.KeyF9: 20, uv.KeyF10: 21, uv.KeyF11: 23, uv.KeyF12: 24}[k.Code]; ok {
		return fmt.Sprintf("\x1b[%d;%d~", n, m)
	}
	return "" // vt encodes the rest itself, shift+tab included
}

// screen indexes per-screen state: 0 main, 1 alternate.
func (w *Window) screen() int {
	if w.term.IsAltScreen() {
		return 1
	}
	return 0
}

// trackKittyKeys follows the kitty keyboard protocol's flag 1 (disambiguate)
// per screen, the way zellij does; other flags are ignored.
// NOTE: one bool, not the protocol's stack, so a pop after two pushes turns
// it off; a stack per screen is the upgrade.
func (w *Window) trackKittyKeys() {
	on := func(p ansi.Params) bool { n, _, _ := p.Param(0, 0); return n&1 != 0 }
	w.term.RegisterCsiHandler(ansi.Command('>', 0, 'u'), func(p ansi.Params) bool {
		w.kittyKeys[w.screen()] = on(p)
		return true
	})
	w.term.RegisterCsiHandler(ansi.Command('<', 0, 'u'), func(ansi.Params) bool {
		w.kittyKeys[w.screen()] = false
		return true
	})
	w.term.RegisterCsiHandler(ansi.Command('=', 0, 'u'), func(p ansi.Params) bool {
		k := &w.kittyKeys[w.screen()]
		switch mode, _, _ := p.Param(1, 1); mode {
		case 1:
			*k = on(p)
		case 2:
			*k = *k || on(p)
		case 3:
			*k = *k && !on(p)
		}
		return true
	})
	w.term.RegisterCsiHandler(ansi.Command('?', 0, 'u'), func(ansi.Params) bool {
		flags := 0
		if w.kittyKeys[w.screen()] {
			flags = 1
		}
		_, _ = fmt.Fprintf(w.term.InputPipe(), "\x1b[?%du", flags)
		return true
	})
}

// kittyKey encodes k as CSI u for a program that turned on flag 1; "" keeps
// the legacy bytes, which the flag keeps for unmodified text, enter, tab and
// backspace, shifted text and functional keys such as arrows.
func kittyKey(k uv.KeyPressEvent) string {
	mods := k.Mod & (uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModMeta | uv.ModHyper | uv.ModSuper)
	switch {
	case k.Code >= uv.KeyExtended:
		return ""
	case k.Code == uv.KeyEscape:
	case mods == 0, mods == uv.ModShift && k.Text != "":
		return ""
	}
	// kitty numbers super 8 and meta 32, uv the other way round
	m := int(mods &^ (uv.ModMeta | uv.ModSuper))
	if mods&uv.ModSuper != 0 {
		m |= 8
	}
	if mods&uv.ModMeta != 0 {
		m |= 32
	}
	if m == 0 {
		return "\x1b[27u"
	}
	return fmt.Sprintf("\x1b[%d;%du", unicode.ToLower(k.Code), m+1)
}
