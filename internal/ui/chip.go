package ui

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/metafates/zemi/internal/hostapi"
)

type chip struct{ key, text string }

// chipLine draws one chip line for the terminal p describes: a chip, the key
// as a keycap on the panel, the rest on the terminal's own ground. It reads as
// plain words off a terminal.
func chipLine(p colorprofile.Profile, chip string, c chip) string {
	pal := hostapi.Whorl
	styled := func(st uv.Style, s string) string { c := uv.ConvertStyle(st, p); return c.Styled(s) }
	line := styled(uv.Style{Fg: pal.Bg, Bg: pal.Accent, Attrs: uv.AttrBold}, chip)
	if c.key != "" {
		line += styled(uv.Style{Fg: pal.Accent, Bg: pal.Panel, Attrs: uv.AttrBold}, " "+strings.TrimRight(c.key, " ")+" ")
	}
	return line + " " + c.text
}

// Chip draws one chip line: a bold label naming the kind of word (busy,
// update) and the text after it, in the terminal's own colors.
func Chip(w io.Writer, name, text string) string {
	return chipLine(colorprofile.Detect(w, os.Environ()), " "+name+" ", chip{text: text})
}

// Busy is the one-instance refusal: a chip names the trouble, the holder's
// pid rides as the keycap when the lock knows it.
func Busy(w io.Writer, pid int) string {
	c := chip{text: "another zemi is already open on this project; close it to open here"}
	if pid > 0 {
		c.key = "pid " + strconv.Itoa(pid)
	}
	return chipLine(colorprofile.Detect(w, os.Environ()), " busy ", c)
}
