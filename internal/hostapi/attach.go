// attach.go expands @path references in a user message: each referenced
// project file is appended to what the model receives, behind attachMarker,
// while the transcript keeps the typed text. Images (png, jpeg, gif, webp)
// travel as file parts for vision models. Only files inside the project
// that the permission store allows reading attach (secret-shaped paths ask,
// so they are skipped); the model can still read the rest with its tools.
package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

const (
	attachMarker   = "\n\n<attached-files>"
	attachMax      = 64 << 10 // bytes per text file
	attachImageMax = 5 << 20  // bytes per image
)

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// composeUserMessage is the runner's Compose hook: the typed text with its
// attachments, then any images as file parts.
func (h *Host) composeUserMessage(text string) fantasy.Message {
	expanded, images := h.expandAttachments(text)
	parts := []fantasy.MessagePart{fantasy.TextPart{Text: expanded}}
	for _, img := range images {
		parts = append(parts, img)
	}
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: parts}
}

var attachRef = regexp.MustCompile(`(?:^|\s)@(\S+)`)

// expandAttachments also carries output of the user's !commands since the
// last message, behind the same marker, and returns referenced images.
func (h *Host) expandAttachments(text string) (string, []fantasy.FilePart) {
	var b strings.Builder
	var images []fantasy.FilePart
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
		if mt, ok := imageTypes[strings.ToLower(filepath.Ext(abs))]; ok {
			data, err := os.ReadFile(abs)
			if err != nil || len(data) > attachImageMax {
				continue
			}
			images = append(images, fantasy.FilePart{Filename: filepath.Base(abs), Data: data, MediaType: mt})
			fmt.Fprintf(&b, "\n<image path=%q>attached below</image>", rel)
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
		return text, images
	}
	return text + attachMarker + b.String() + "\n</attached-files>", images
}
