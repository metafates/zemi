package ui

import (
	"image/color"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/metafates/zemi/internal/hostapi"
)

// themeResolver turns highlight-group names into ultraviolet styles using
// the active theme's palette (DESIGN §6.6: palettes map names to colors, hl
// groups reference palette names or raw hex; Go resolves them).
type themeResolver struct {
	host *hostapi.Host
}

func (t themeResolver) style(hl string) uv.Style {
	if hl == "" || t.host == nil {
		return uv.Style{}
	}
	theme := t.host.Themes.Get(t.host.ActiveThemeName())
	if theme == nil {
		return uv.Style{}
	}
	spec, ok := theme.HL[hl]
	if !ok {
		return uv.Style{}
	}
	st := uv.Style{}
	if c := t.colorValue(spec.Fg, theme); c != nil {
		st.Fg = c
	}
	if c := t.colorValue(spec.Bg, theme); c != nil {
		st.Bg = c
	}
	if spec.Bold {
		st.Attrs |= uv.AttrBold
	}
	if spec.Italic {
		st.Attrs |= uv.AttrItalic
	}
	if spec.Underline {
		st.Underline = uv.UnderlineSingle
	}
	if spec.Reverse {
		st.Attrs |= uv.AttrReverse
	}
	return st
}

// colorValue resolves a theme color reference: raw hex, or a palette name.
func (t themeResolver) colorValue(ref string, theme *hostapi.Theme) color.Color {
	if ref == "" {
		return nil
	}
	if ref[0] == '#' {
		col, err := hexColor(ref)
		if err != nil {
			return nil
		}
		return col
	}
	if hex, ok := theme.Palette[ref]; ok && hex != "" {
		if hex[0] == '#' {
			col, err := hexColor(hex)
			if err != nil {
				return nil
			}
			return col
		}
	}
	return nil
}
