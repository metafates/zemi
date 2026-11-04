package ui

import (
	"io"
	"math/rand/v2"
	"os"
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
	{"/review", "has several agents review your changes"},
	{"alt+o", "browses the chat's whole memory, however old"},
}

// Tip is one discovery tip styled in the whorl chrome: a tip chip, the key as
// a keycap on the panel, the rest on the terminal's own ground. It reads as
// plain words off a terminal.
func Tip(w io.Writer) string {
	return tipStyled(colorprofile.Detect(w, os.Environ()), _tips[rand.N(len(_tips))])
}

// tipStyled draws one tip for the terminal p describes.
func tipStyled(p colorprofile.Profile, t tip) string {
	pal := hostapi.Whorl
	styled := func(st uv.Style, s string) string { c := uv.ConvertStyle(st, p); return c.Styled(s) }
	chip := styled(uv.Style{Fg: pal.Bg, Bg: pal.Accent, Attrs: uv.AttrBold}, " tip ")
	key := styled(uv.Style{Fg: pal.Accent, Bg: pal.Panel, Attrs: uv.AttrBold}, " "+strings.TrimRight(t.key, " ")+" ")
	return chip + key + " " + t.text
}
