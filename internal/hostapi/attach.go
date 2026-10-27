// attach.go expands @path references in a user message: each referenced
// project file is appended to what the model receives, behind agent.AttachMarker,
// while the transcript keeps the typed text. Images (png, jpeg, gif, webp)
// travel as file parts for vision models. Only files inside the project
// that the permission store allows reading attach; a file that is too big,
// not text or not allowed gets a note saying so in place of its content, so
// the model knows to reach for it with its tools.

package hostapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
)

const (
	attachMax      = 64 << 10           // bytes per text file
	attachImageMax = 20 << 20           // bytes per image read; tools.FitImage sizes what is sent
	dropTTL        = 7 * 24 * time.Hour // idle copies age out, like scratchpads
	// dropNameMax is how long a copied file's name may read in a token
	dropNameMax = 60
)

var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

// composeUserMessage is the runner's Compose hook: the typed text with its
// attachments, then any images as file parts. A text-only model gets a
// note in their place instead of a refused request. What the model reads
// beyond the user's words - attached files, the !commands' output - lands in
// the log as echo rows, so the tree can summarize what it saw; a report's
// body is already a work row of its own.
func (h *Host) composeUserMessage(text string, images bool) fantasy.Message {
	expanded, imgs := h.expandAttachments(text, images)
	if h.chat != nil {
		if _, attached, ok := strings.Cut(expanded, agent.AttachMarker); ok && strings.TrimSpace(attached) != "" &&
			!strings.Contains(attached, "<agent-report ") && !strings.Contains(attached, "<agent-update ") {
			if err := h.chat.Echo("attach", true, attached); err != nil {
				h.Log.Warn("chat log append failed", "origin", "agent", "error", err)
			}
		}
	}
	parts := []fantasy.MessagePart{fantasy.TextPart{Text: expanded}}
	for _, img := range imgs {
		parts = append(parts, img)
	}
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: parts}
}

// attachRef finds what attaches: a typed @name, a +skill mention, and the
// [Image name] or [File name] chip zemi writes for a file it copied in, or
// for a project path an @ word cannot carry whole.
var attachRef = regexp.MustCompile(`(?:^|\s)(?:@(\S+)|\+([a-z][a-z0-9-]*))|\[(Image|File) ([^\]\n]+)\]`)

func overLimit(limit int64) string {
	if limit >= 1<<20 {
		return fmt.Sprintf("it is over the %d MB attachment limit", limit>>20)
	}
	return fmt.Sprintf("it is over the %d KB attachment limit; read it with the read tool", limit>>10)
}

// expandAttachments also carries instruction files edited since the prompt
// was pinned and output of the user's !commands since the last message,
// behind the same marker, and returns referenced images. images is whether
// the model takes them at all: it gets a note in their place when not.
func (h *Host) expandAttachments(text string, images bool) (string, []fantasy.FilePart) {
	var b strings.Builder
	var imgs []fantasy.FilePart
	var refused []string // "path: why", which the user hears too
	seen := set.Set[string]{}
	// what follows the marker is host-composed from model output (agent
	// reports), and an @ there must not attach anything
	typed, _, _ := strings.Cut(text, agent.AttachMarker)
	for _, m := range attachRef.FindAllStringSubmatch(typed, -1) {
		if m[2] != "" { // a +skill attaches once, like a repeated @file; a
			// name and an abs path cannot collide (ValidName allows no /)
			if !seen.Has(m[2]) {
				seen.Add(m[2])
				h.attachSkill(&b, m[2])
			}
			continue
		}
		// a chip names a file zemi itself copied, or one of the project's: the
		// copy wins, since a name saveDrop handed out is not the project's
		chip, name := m[3] != "", ""
		if chip {
			name = m[4]
		} else {
			name = strings.TrimRight(m[1], ".,;:!?)")
		}
		var path perm.Path
		drop := false
		if chip && dropName(name) && h.dropsDir() != "" {
			if p, err := perm.Resolve(filepath.Join(h.dropsDir(), name)); err == nil {
				path, drop = p, true
			}
		}
		if !drop {
			// an @ names a project file: one outside the project, by ".." or
			// a symlink, attaches nothing
			p, err := perm.Resolve(h.projectAbs(name))
			if err != nil || !h.inRoots(p.String(), h.Dirs.ProjectDir) {
				continue
			}
			path = p
		}
		abs := path.String()
		if seen.Has(abs) {
			continue
		}
		seen.Add(abs)
		mt, image := imageTypes[strings.ToLower(filepath.Ext(abs))]
		limit, tag := int64(attachMax), "file"
		if image {
			limit, tag = attachImageMax, "image"
		}
		// say why instead of dropping the reference: the model is told the
		// file was meant to be there and can still read it itself
		note := func(why string) {
			b.WriteString(agent.AttachElement(fmt.Sprintf("%s path=%q", tag, name), "not attached: "+why))
			refused = append(refused, name+": "+why)
		}
		// a FIFO blocks the read, and an @word naming no file is just prose;
		// a path that is there but not a file says why, like every other refusal
		if fi, err := os.Stat(abs); err == nil && !fi.Mode().IsRegular() {
			if fi.IsDir() {
				note("it is a directory")
			} else {
				note("it is not a regular file")
			}
			continue
		}
		// the model reads the file, so opening a device has effects
		f, fi, err := tools.OpenRegular(path)
		if err != nil {
			continue
		}
		if fi.Size() > limit { // a huge file would be read whole first
			f.Close()
			note(overLimit(limit))
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		switch {
		case err != nil:
			note("it could not be read")
		case int64(len(data)) > limit:
			note(overLimit(limit))
		case image && !images:
			b.WriteString(agent.AttachElement(fmt.Sprintf("image path=%q", name), "omitted: the current model does not take images"))
		case image:
			fit, fitMT, err := tools.FitImage(data, mt)
			if err != nil {
				note(err.Error())
				continue
			}
			imgs = append(imgs, fantasy.FilePart{Filename: filepath.Base(abs), Data: fit, MediaType: fitMT})
			b.WriteString(agent.AttachElement(fmt.Sprintf("image path=%q", name), "attached below"))
		case !utf8.Valid(data):
			note("it is not text")
		default:
			// the body shows as written; a file carrying its own close tag blurs
			// where it ends, model-side only (NOTE: the ceiling - real escaping
			// would change the bytes of every attached file)
			b.WriteString(agent.AttachElement(fmt.Sprintf("file path=%q", name), string(data)))
		}
	}
	switch len(refused) {
	case 0:
	case 1:
		name, why, _ := strings.Cut(refused[0], ": ")
		h.Notify(slog.LevelWarn, "Could not attach "+name, why)
	default:
		h.Notify(slog.LevelWarn, fmt.Sprintf("Could not attach %d files", len(refused)), strings.Join(refused, "\n"))
	}
	b.WriteString(h.instructionEdits())
	b.WriteString(h.takeShellNotes())
	if b.Len() == 0 {
		return text, imgs
	}
	return text + agent.AttachMarker + b.String() + "\n</attached-files>", imgs
}

// dropsDir holds copies of files the user dropped or pasted from outside the
// project. A chip naming a bare file reaches only this directory, so a
// reference typed in (or smuggled into) a message cannot attach anything else
// from outside.
func (h *Host) dropsDir() string { return joinIf(h.Dirs.DataHome, "drops") }

// dropName reports whether name is one zemi's copies can have: a bare name,
// no separator and no leading dot. A chip with one resolves in the drops
// directory; any other chip names a project file.
func dropName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

// canCarry reports whether a name travels in a token unchanged.
func canCarry(name string) bool { return !strings.ContainsAny(name, "]\n\t") }

// canRef reports whether rel reads back unchanged as an @ word: it ends at
// whitespace, and its trailing punctuation is trimmed as a sentence's is.
func canRef(rel string) bool {
	return rel != "" && !strings.ContainsAny(rel, " \t\n\f\r") && strings.TrimRight(rel, ".,;:!?)") == rel
}

// projectRef is a project file's name in a token. A bare root file would read
// as one of the drops directory's copies, which another project's file of the
// same name could answer, so it carries the ./ that tells them apart.
func projectRef(rel string) string {
	if dropName(rel) {
		return "./" + rel
	}
	return rel
}

// attachToken is the composer's chip for an attachment: the name the user
// knows, whole, for a file from outside the project or one no @ word carries.
func attachToken(name string, image bool) string {
	if image {
		return "[Image " + name + "]"
	}
	return "[File " + name + "]"
}

// attachRefToken is a project file as the composer writes it: an @reference
// the user can go on editing, and the completion picks up again, where what
// zemi reads back is the same path. Else the chip, which carries the name
// whole.
func attachRefToken(rel string, image bool) string {
	if canRef(rel) {
		return "@" + rel
	}
	return attachToken(projectRef(rel), image)
}

// AttachLabel is what the completion popup inserts for a project file the
// user picked. A path no token carries, where the @ word and the chip both
// refuse it, is its quoted absolute path instead, for the model to reach with
// its tools.
func (h *Host) AttachLabel(rel string) string {
	if !canRef(rel) && !canCarry(rel) {
		return tools.ShellQuote(h.projectAbs(rel))
	}
	_, image := imageTypes[strings.ToLower(filepath.Ext(rel))]
	return attachRefToken(rel, image)
}

// AttachPath turns a path the user dropped or pasted into a composer token:
// the form AttachLabel gives a project file, where a token names it, [Image
// name] or [File name] for a file zemi copies into the drops directory under
// its own name, and otherwise the quoted path (a directory, a large or
// binary file) for the model to reach with its tools.
func (h *Host) AttachPath(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return tools.ShellQuote(path)
	}
	_, image := imageTypes[strings.ToLower(filepath.Ext(path))]
	// either token names the project's own file, so only a name neither
	// carries is copied in
	if rel, err := filepath.Rel(h.Dirs.ProjectDir, path); err == nil && h.inRoots(path, h.Dirs.ProjectDir) &&
		(canRef(rel) || canCarry(rel)) {
		if fi.IsDir() {
			return rel + string(filepath.Separator)
		}
		return attachRefToken(rel, image)
	}
	if fi.IsDir() || (image && fi.Size() > attachImageMax) || (!image && fi.Size() > attachMax) {
		return tools.ShellQuote(path)
	}
	data, err := os.ReadFile(path)
	if err != nil || (!image && !utf8.Valid(data)) {
		return tools.ShellQuote(path)
	}
	name, err := h.saveDrop(filepath.Base(path), data)
	if err != nil {
		return tools.ShellQuote(path)
	}
	return attachToken(name, image)
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
	return attachToken(name, true), nil
}

// dropUnsafe is what a name cannot carry: a cell that would end the token, and
// the separators that would make it a path.
var dropUnsafe = regexp.MustCompile("[\x00-\x1f\x7f/\\\\\\]]+")

// saveDrop writes data under an unused name of its own (the same content
// keeps its name) and returns that name. The name is the source's, cut to
// what a token carries and shortened only where it breaks one.
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
	stem := sanitize.Truncate(strings.TrimSuffix(base, ext), max(dropNameMax-len([]rune(ext)), 1))
	for i := 1; ; i++ {
		name := stem + ext
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		// a name the project root has too would resolve to that file instead
		if _, err := os.Stat(filepath.Join(h.Dirs.ProjectDir, name)); err == nil {
			continue
		}
		target := filepath.Join(dir, name)
		old, err := os.ReadFile(target)
		switch {
		case err == nil && string(old) == string(data):
			t := time.Now()
			_ = os.Chtimes(target, t, t) // attaching it again keeps it alive
			return name, nil
		case errors.Is(err, fs.ErrNotExist):
			return name, os.WriteFile(target, data, 0o600)
		case err != nil:
			return "", err
		}
	}
}

// sweepDrops removes copies idle past dropTTL; without it the directory
// grows for the life of the install. An attachment whose copy is older than that
// stops resolving, like a scratchpad file.
func sweepDrops(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > dropTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
