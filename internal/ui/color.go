package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
)

// hexColor parses a #rgb or #rrggbb string into a color.Color (DESIGN §6.6:
// highlight groups reference palette names or raw hex; Go resolves them).
func hexColor(s string) (color.Color, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if len(s) != 6 || err != nil {
		return nil, fmt.Errorf("invalid hex color %q", s)
	}
	return rgb(uint32(v)), nil
}
