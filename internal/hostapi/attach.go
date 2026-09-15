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
	"time"
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
		var abs string
		if name, ok := strings.CutPrefix(rel, "drop:"); ok {
			// only copies of what the user dropped live there (AttachPath)
			if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
				continue
			}
			abs = filepath.Join(h.dropsDir(), name)
		} else {
			var err error
			abs, err = h.resolveJailed(rel, []string{h.Dirs.ProjectDir})
			if err != nil || h.Perm.Evaluate("fs.read", abs, "user").Decision != perm.Allow {
				continue
			}
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

// dropsDir holds copies of files the user dropped or pasted from outside the
// project. @drop:name reaches only this directory, so a reference typed in
// (or smuggled into) a message cannot attach anything else from outside.
func (h *Host) dropsDir() string { return filepath.Join(h.Dirs.DataHome, "drops") }

// AttachPath turns a path the user dropped or pasted into a composer token:
// @rel for a file in the project, @drop:name for a copy of an image or text
// file from elsewhere, and otherwise the quoted path (a directory, a large,
// binary or secret-shaped file) for the model to reach with its tools.
func (h *Host) AttachPath(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return quotePath(path)
	}
	if rel, err := filepath.Rel(h.Dirs.ProjectDir, path); err == nil && h.inJail(path, h.Dirs.ProjectDir) && !strings.ContainsAny(rel, " \t") {
		if fi.IsDir() {
			return rel + string(filepath.Separator)
		}
		return "@" + rel
	}
	_, image := imageTypes[strings.ToLower(filepath.Ext(path))]
	if fi.IsDir() || perm.PathIsSecret(path) || (image && fi.Size() > attachImageMax) || (!image && fi.Size() > attachMax) {
		return quotePath(path)
	}
	data, err := os.ReadFile(path)
	if err != nil || (!image && !utf8.Valid(data)) {
		return quotePath(path)
	}
	name, err := h.saveDrop(filepath.Base(path), data)
	if err != nil {
		return quotePath(path)
	}
	return "@drop:" + name
}

// AttachImage stores image data (a clipboard paste) and returns its token.
func (h *Host) AttachImage(data []byte, ext string) (string, error) {
	if len(data) > attachImageMax {
		return "", fmt.Errorf("the image is %d MB; the limit is %d MB", len(data)>>20, attachImageMax>>20)
	}
	name, err := h.saveDrop("pasted-"+time.Now().Format("150405")+ext, data)
	if err != nil {
		return "", err
	}
	return "@drop:" + name, nil
}

var dropUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// saveDrop writes data under a token-safe, unused name (the same content
// keeps its name) and returns that name.
func (h *Host) saveDrop(base string, data []byte) (string, error) {
	dir := h.dropsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base = strings.TrimLeft(dropUnsafe.ReplaceAllString(base, "-"), ".-")
	if base == "" {
		base = "file"
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		target := filepath.Join(dir, name)
		old, err := os.ReadFile(target)
		switch {
		case err == nil && string(old) == string(data):
			return name, nil
		case os.IsNotExist(err):
			return name, os.WriteFile(target, data, 0o600)
		case err != nil:
			return "", err
		}
	}
}

func quotePath(p string) string {
	if strings.ContainsAny(p, " \t'\"$`\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
	}
	return p
}
