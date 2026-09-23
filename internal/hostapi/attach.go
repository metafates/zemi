// attach.go expands @path references in a user message: each referenced
// project file is appended to what the model receives, behind attachMarker,
// while the transcript keeps the typed text. Images (png, jpeg, gif, webp)
// travel as file parts for vision models. Only files inside the project
// that the permission store allows reading attach; a file that is too big,
// not text or not allowed gets a note saying so in place of its content, so
// the model knows to reach for it with its tools.
package hostapi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

const (
	attachMarker   = "\n\n<attached-files>"
	attachMax      = 64 << 10           // bytes per text file
	attachImageMax = 20 << 20           // bytes per image read; tools.FitImage sizes what is sent
	dropTTL        = 7 * 24 * time.Hour // idle copies age out, like scratchpads
)

// Attached puts text the user did not type (piped stdin, a command file's
// body, a note from zemi) behind attachMarker as one element of the
// trailer, so nothing shows, stores or recalls it as the user's own words.
func Attached(tag, text string) string {
	return attachMarker + "\n<" + tag + ">\n" + text + "\n</" + tag + ">\n</attached-files>"
}

// CutAttached splits a message into what was typed and its trailer.
func CutAttached(s string) (typed, trailer string) {
	if i := strings.Index(s, attachMarker); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// modelNote wraps text zemi itself sends the model (a nag, a plan handoff).
func modelNote(text string) string { return Attached("zemi-note", text) }

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// composeUserMessage is the runner's Compose hook: the typed text with its
// attachments, then any images as file parts. A text-only model gets a
// note in their place instead of a refused request.
func (h *Host) composeUserMessage(text string, images bool) fantasy.Message {
	expanded, imgs := h.expandAttachments(text)
	if !images && len(imgs) > 0 {
		expanded = strings.ReplaceAll(expanded, "attached below</image>", "omitted: the current model does not take images</image>")
		imgs = nil
	}
	parts := []fantasy.MessagePart{fantasy.TextPart{Text: expanded}}
	for _, img := range imgs {
		parts = append(parts, img)
	}
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: parts}
}

var attachRef = regexp.MustCompile(`(?:^|\s)@(\S+)`)

func overLimit(limit int64) string {
	if limit >= 1<<20 {
		return fmt.Sprintf("it is over the %d MB attachment limit", limit>>20)
	}
	return fmt.Sprintf("it is over the %d KB attachment limit; read it with the read tool", limit>>10)
}

// expandAttachments also carries output of the user's !commands since the
// last message, behind the same marker, and returns referenced images.
func (h *Host) expandAttachments(text string) (string, []fantasy.FilePart) {
	var b strings.Builder
	var images []fantasy.FilePart
	seen := map[string]bool{}
	// what follows the marker is host-composed from model output (agent
	// reports), and an @ there must not attach anything
	typed, _, _ := strings.Cut(text, attachMarker)
	for _, m := range attachRef.FindAllStringSubmatch(typed, -1) {
		rel := strings.TrimRight(m[1], ".,;:!?)")
		if seen[rel] {
			continue
		}
		seen[rel] = true
		var abs string
		drop := false
		if name, ok := strings.CutPrefix(rel, "drop:"); ok {
			// only copies of what the user dropped live there (AttachPath)
			if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") || h.dropsDir() == "" {
				continue
			}
			abs, drop = filepath.Join(h.dropsDir(), name), true
		} else {
			var err error
			abs, err = h.resolveJailed(rel, []string{h.Dirs.ProjectDir})
			if err != nil {
				continue
			}
		}
		mt, image := imageTypes[strings.ToLower(filepath.Ext(abs))]
		limit, tag := int64(attachMax), "file"
		if image {
			limit, tag = attachImageMax, "image"
		}
		// say why instead of dropping the reference: the model is told the
		// file was meant to be there and can still read it itself
		note := func(why string) { fmt.Fprintf(&b, "\n<%s path=%q>not attached: %s</%s>", tag, rel, why, tag) }
		// a FIFO blocks the turn, and an @word naming no file is just prose
		fi, err := os.Stat(abs)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		// the model reads the file, so the model's rules apply
		if !drop && h.Perm.Evaluate("fs.read", abs, "model").Decision != perm.Allow {
			note("reading it needs your approval")
			continue
		}
		if fi.Size() > limit { // a huge file would be read whole first
			note(overLimit(limit))
			continue
		}
		data, err := os.ReadFile(abs)
		switch {
		case err != nil:
			note("it could not be read")
		case int64(len(data)) > limit:
			note(overLimit(limit))
		case image:
			fit, fitMT, err := tools.FitImage(data, mt)
			if err != nil {
				note(err.Error())
				continue
			}
			images = append(images, fantasy.FilePart{Filename: filepath.Base(abs), Data: fit, MediaType: fitMT})
			fmt.Fprintf(&b, "\n<image path=%q>attached below</image>", rel)
		case !utf8.Valid(data):
			note("it is not text")
		default:
			fmt.Fprintf(&b, "\n<file path=%q>\n%s\n</file>", rel, data)
		}
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
func (h *Host) dropsDir() string { return joinIf(h.Dirs.DataHome, "drops") }

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
	if dir == "" {
		return "", errors.New("no data directory for drops")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	defer sweepDrops(dir) // after the write, so the name just handed out stays
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
			t := time.Now()
			_ = os.Chtimes(target, t, t) // attaching it again keeps it alive
			return name, nil
		case os.IsNotExist(err):
			return name, os.WriteFile(target, data, 0o600)
		case err != nil:
			return "", err
		}
	}
}

// sweepDrops removes copies idle past dropTTL; without it the directory
// grows for the life of the install. An @drop: reference older than that
// stops resolving, like a scratchpad file.
func sweepDrops(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > dropTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func quotePath(p string) string {
	if strings.ContainsAny(p, " \t'\"$`\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
	}
	return p
}
