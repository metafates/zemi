// attach.go expands @path references in a user message: each referenced
// project file is appended to what the model receives, behind attachMarker,
// while the transcript keeps the typed text. Only files inside the project
// that the permission store allows reading attach (secret-shaped paths ask,
// so they are skipped); the model can still read the rest with its tools.
package hostapi

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/metafates/zemi/internal/perm"
)

const (
	attachMarker = "\n\n<attached-files>"
	attachMax    = 64 << 10 // bytes per file
)

var attachRef = regexp.MustCompile(`(?:^|\s)@(\S+)`)

// expandAttachments also carries output of the user's !commands since the
// last message, behind the same marker.
func (h *Host) expandAttachments(text string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, m := range attachRef.FindAllStringSubmatch(text, -1) {
		rel := strings.TrimRight(m[1], ".,;:!?)")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		abs, err := h.resolveJailed(rel, []string{h.Dirs.ProjectDir})
		if err != nil || h.Perm.Evaluate("fs.read", abs, "user").Decision != perm.Allow {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil || len(data) > attachMax || !utf8.Valid(data) {
			continue
		}
		fmt.Fprintf(&b, "\n<file path=%q>\n%s\n</file>", rel, data)
	}
	b.WriteString(h.takeShellNotes())
	if b.Len() == 0 {
		return text
	}
	return text + attachMarker + b.String() + "\n</attached-files>"
}
