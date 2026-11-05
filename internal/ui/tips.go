package ui

import (
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/metafates/zemi/internal/hostapi"
)

type tip struct{ key, text string }

// The discovery tips: one is zemi's parting line, drawn at random on each exit.
var _tips = []tip{
	{"/btw ", "asks a side question the conversation does not keep"},
	{"@", "attaches files, dirs and skills to the prompt"},
	{"/goal ", "keeps working until a condition holds"},
	{"alt+o", "browses the chat's whole memory, however old"},
	{"/config", "you can disable tips with ui.tips set to false"},
}

// Tip is one discovery tip styled in the whorl chrome: a tip chip, the key as
// a keycap on the panel, the rest on the terminal's own ground. It reads as
// plain words off a terminal.
func Tip(w io.Writer) string {
	return tipStyled(colorprofile.Detect(w, os.Environ()), " tip ", _tips[rand.N(len(_tips))])
}

// Busy is the one-instance refusal, dressed like a tip: a chip names the
// trouble, the holder's pid rides as the keycap when the lock knows it.
func Busy(w io.Writer, pid int) string {
	t := tip{text: "another zemi is already open on this project; close it to open here"}
	if pid > 0 {
		t.key = strconv.Itoa(pid)
	}
	return tipStyled(colorprofile.Detect(w, os.Environ()), " busy ", t)
}

// tipStyled draws one tip for the terminal p describes; a tip without a key
// draws its chip and text alone.
func tipStyled(p colorprofile.Profile, chip string, t tip) string {
	pal := hostapi.Whorl
	styled := func(st uv.Style, s string) string { c := uv.ConvertStyle(st, p); return c.Styled(s) }
	line := styled(uv.Style{Fg: pal.Bg, Bg: pal.Accent, Attrs: uv.AttrBold}, chip)
	if t.key != "" {
		line += styled(uv.Style{Fg: pal.Accent, Bg: pal.Panel, Attrs: uv.AttrBold}, " "+strings.TrimRight(t.key, " ")+" ")
	}
	return line + " " + t.text
}
