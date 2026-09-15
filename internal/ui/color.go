package ui

import (
	"fmt"
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// hexColor parses a #rgb or #rrggbb string into a color.Color (DESIGN §6.6:
// highlight groups reference palette names or raw hex; Go resolves them).
func hexColor(s string) (color.Color, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	switch len(s) {
	case 3:
		var rgb [3]uint8
		for i := 0; i < 3; i++ {
			n, err := hexNibble(s[i])
			if err != nil {
				return nil, err
			}
			rgb[i] = n*17 + n // expand 4-bit to 8-bit
		}
		return ansi.RGBColor{R: rgb[0], G: rgb[1], B: rgb[2]}, nil
	case 6:
		var rgb [3]uint8
		for i := 0; i < 3; i++ {
			hi, err := hexNibble(s[i*2])
			if err != nil {
				return nil, err
			}
			lo, err := hexNibble(s[i*2+1])
			if err != nil {
				return nil, err
			}
			rgb[i] = hi<<4 | lo
		}
		return ansi.RGBColor{R: rgb[0], G: rgb[1], B: rgb[2]}, nil
	}
	return nil, fmt.Errorf("invalid hex color %q", s)
}

func hexNibble(c byte) (uint8, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("invalid hex digit %q", string(rune(c)))
}
