package win

import (
	"fmt"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// ParseKeys reads neovim key notation: plain characters are typed as they
// are, and <...> names a key, with modifiers as in <c-w>, <a-x>, <s-tab>,
// <c-s-left>. <lt> is a literal <. A < that does not open a known name is
// an error rather than silent text, so a typo never lands in the program.
func ParseKeys(s string) ([]uv.KeyPressEvent, error) {
	var out []uv.KeyPressEvent
	for i := 0; i < len(s); {
		if s[i] != '<' {
			r, size := decodeRune(s[i:])
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

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 1
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
	if r, size := decodeRune(name); size == len(name) && size > 0 {
		k := uv.KeyPressEvent{Code: r, Mod: mod}
		if mod == 0 || mod == uv.ModShift {
			k.Text = string(r)
		}
		return k, nil
	}
	return uv.KeyPressEvent{}, fmt.Errorf("keys: unknown key <%s>; names are esc, cr, tab, bs, space, del, up, down, left, right, home, end, pgup, pgdn, f1-f12, lt, with c-, a-, s- modifiers", orig)
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
	if k.Code == uv.KeyTab && k.Mod == uv.ModShift {
		return "" // vt encodes shift+tab itself
	}
	return ""
}
