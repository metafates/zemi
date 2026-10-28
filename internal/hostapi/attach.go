// attach.go expands @ references in a user message: each referenced skill or
// file - a project file, or any path outside it - is appended to what the
// model receives, behind agent.AttachMarker, while the transcript keeps the
// typed text. Images (png, jpeg, gif, webp) travel as file parts for vision
// models. A file that is too big, not text or not there gets a note saying
// so in place of its content, so the model knows to reach for it with its
// tools.

package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
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
	// dropNameMax is how long a copied file's name may be
	dropNameMax = 60
	// dirListMax is how many files a directory's listing may name
	dirListMax = 500
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
			if err := h.chat.Echo("attach", true, attached, StreamTool{}); err != nil {
				h.logAppendFailed(err)
			}
		}
	}
	parts := []fantasy.MessagePart{fantasy.TextPart{Text: expanded}}
	for _, img := range imgs {
		parts = append(parts, img)
	}
	return fantasy.Message{Role: fantasy.MessageRoleUser, Content: parts}
}

// attachRef finds what attaches: an @ word at a word start, quoted when the
// path cannot ride a bare word. A quoted word's closing quote is optional,
// so a word still being typed resolves too.
var attachRef = regexp.MustCompile(`(?:^|\s)(?:@"([^"\n]+)"?|@'([^'\n]+)'?|@(\S+))`)

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
		// the word normalizes first: a quoted word unquotes (the grammar
		// holds no quote), a bare one trims trailing sentence punctuation.
		// Then the whole word names a skill, and a miss names a path.
		word := m[3]
		if m[1] != "" || m[2] != "" {
			word = cmp.Or(m[1], m[2])
		} else {
			word = strings.TrimRight(word, ".,;:!?)")
		}
		if s, ok := h.skillByName(word); ok {
			// a skill attaches once, like a repeated @file
			if !seen.Has(s.Name) {
				seen.Add(s.Name)
				h.attachSkill(&b, s)
			}
			continue
		}
		// a path, wherever it lives: the user named it, and what attaches
		// shows in the attach echo
		path, err := perm.Resolve(h.pathAbs(word))
		if err != nil {
			continue
		}
		abs := path.String()
		if seen.Has(abs) {
			continue
		}
		seen.Add(abs)
		name := abs
		if root, err := perm.Resolve(h.Dirs.ProjectDir); err == nil {
			if rel, ok := perm.Under(root.String(), abs); ok {
				name = rel
			}
		}
		mt, image := imageTypes[strings.ToLower(filepath.Ext(abs))]
		limit, tag := int64(attachMax), "file"
		if image {
			limit, tag = attachImageMax, "image"
		}
		// say why instead of dropping the reference: the model is told the
		// file was meant to be there and can still read it itself, by the
		// path the note carries (absolute for one outside the project, where
		// the read tool refuses ~)
		note := func(why string) {
			b.WriteString(agent.AttachElement(fmt.Sprintf("%s path=%q", tag, name), "not attached: "+why))
			refused = append(refused, perm.TildePath(name)+": "+why)
		}
		// a FIFO blocks the read, and an @word naming no file is just prose;
		// a path that is there but not a regular file says why, like every
		// other refusal - but a directory attaches its files listed
		if fi, err := os.Stat(abs); err == nil && !fi.Mode().IsRegular() {
			if fi.IsDir() {
				attachDir(&b, abs, name, &refused)
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

// attachDir is what a directory attaches: its files listed, one path
// relative to it per line, shallowest first, so the model sees the tree
// and reads what it needs with its tools. The listing shows at most
// dirListMax files and attachMax bytes; the tail names what it does not
// show, and so does an unreadable subdirectory.
func attachDir(b *strings.Builder, abs, name string, refused *[]string) {
	l, err := listDir(abs)
	if err != nil {
		b.WriteString(agent.AttachElement(fmt.Sprintf("dir path=%q", name), "not attached: it could not be read"))
		*refused = append(*refused, perm.TildePath(name)+": it could not be read")
		return
	}
	var body strings.Builder
	shown := 0
	for _, p := range l.files {
		if body.Len()+len(p)+1 > attachMax {
			break
		}
		body.WriteString(p + "\n")
		shown++
	}
	for _, rel := range l.unreadable {
		body.WriteString(rel + "/ unreadable\n")
	}
	if shown < l.total {
		fmt.Fprintf(&body, "... %d more", l.total-shown)
	}
	if body.Len() == 0 {
		body.WriteString("(no files)")
	}
	b.WriteString(agent.AttachElement(fmt.Sprintf("dir path=%q files=%d", name, l.total), body.String()))
}

// dirList is what a directory holds: the files the listing names (capped at
// dirListMax), the count of every file the walk saw, and the subdirectories
// it could not read.
type dirList struct {
	files      []string
	total      int
	unreadable []string
}

// listWait bounds a directory's walk, the bound git's file listing and the
// popup's listing take too (tools.listWait, ui.dirListWait).
const listWait = 2 * time.Second

// listDir walks the regular files under dir under the listWait bound, so a
// mount that answers slowly cannot hold a turn.
func listDir(root string) (dirList, error) {
	type res struct {
		l   dirList
		err error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := walkDir(root)
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		return r.l, r.err
	case <-time.After(listWait):
		// NOTE: a mount hung past the bound leaves the walk goroutine parked on its read
		return dirList{}, errors.New("listing timed out")
	}
}

// walkDir walks the regular files under dir, by path relative to it,
// shallow-first, counting past the cap so the tail can name the true
// remainder. It skips .git and the directories tools.SkipDir names -
// matched against the name as-is and dot-stripped, so .venv dies with
// venv - and keeps every other hidden entry. The root's own read error
// returns; a subdirectory's is named in unreadable. listDir bounds it.
func walkDir(root string) (l dirList, rootErr error) {
	var walk func(dir, rel string, top bool)
	walk = func(dir, rel string, top bool) {
		des, err := os.ReadDir(dir)
		if err != nil {
			if top {
				rootErr = err
			} else {
				l.unreadable = append(l.unreadable, rel)
			}
			return
		}
		var subs, subrels []string
		for _, de := range des {
			name := de.Name()
			if de.IsDir() {
				if name == ".git" || tools.SkipDir(name) || tools.SkipDir(strings.TrimPrefix(name, ".")) {
					continue
				}
				subs = append(subs, filepath.Join(dir, name))
				subrels = append(subrels, path.Join(rel, name))
				continue
			}
			if !de.Type().IsRegular() {
				continue
			}
			l.total++
			if len(l.files) >= dirListMax {
				continue
			}
			if r, err := filepath.Rel(root, filepath.Join(dir, name)); err == nil {
				l.files = append(l.files, filepath.ToSlash(r))
			}
		}
		for i, sub := range subs {
			walk(sub, subrels[i], false)
		}
	}
	walk(root, ".", true)
	if rootErr != nil {
		return
	}
	tools.SortShallow(l.files)
	return l, nil
}

// pathAbs spells an @ word out to a path: ~ under home, the rest against the
// project directory.
func (h *Host) pathAbs(word string) string {
	if strings.HasPrefix(word, "~") {
		return perm.ExpandHome(word)
	}
	return h.projectAbs(word)
}

// dropsDir holds copies of pasted images, the one attachment with no source
// path. A reference typed in a message attaches what it names directly, so
// no copy stands between it and the file.
func (h *Host) dropsDir() string { return joinIf(h.Dirs.DataHome, "drops") }

// CanRef reports whether a word rides in an @(\S+) token and reads back
// unchanged: it ends at whitespace, and its trailing punctuation is trimmed
// as a sentence's is.
func CanRef(word string) bool {
	return word != "" && !strings.ContainsAny(word, " \t\n\f\r") && strings.TrimRight(word, ".,;:!?)") == word
}

// AttachWord is the @ word the composer inserts for path: relative for a
// project file, ./ when its bare name would answer as a skill, ~-spelled for
// a path under home, absolute for the rest, and quoted when the word cannot
// ride a bare token, because it holds whitespace or ends in sentence
// punctuation the expander would trim. A directory carries the trailing
// slash, which keeps a completion walking into it.
func (h *Host) AttachWord(path string) string {
	fi, _ := os.Stat(path)
	dir := fi != nil && fi.IsDir()
	rel, inside := "", false
	if r, err := filepath.Rel(h.Dirs.ProjectDir, path); err == nil && h.inRoots(path, h.Dirs.ProjectDir) {
		rel, inside = r, true
	}
	word := perm.TildePath(path)
	if inside {
		word = rel
		// a bare name a skill also answers would attach the skill; the file
		// escapes through the ./ a path carries. A directory's word ends in
		// the slash, which no skill name holds.
		if !dir && !strings.ContainsAny(rel, `/\`) {
			if _, ok := h.skillByName(rel); ok {
				word = "./" + rel
			}
		}
	}
	if dir && !strings.HasSuffix(word, "/") {
		word += "/"
	}
	if !CanRef(word) {
		// the word cannot ride a bare token: quoted and absolute, where the
		// read tool opens it whatever the frame it was typed in
		w := perm.TildePath(path)
		if dir && !strings.HasSuffix(w, "/") {
			w += "/"
		}
		return `@"` + w + `"`
	}
	return "@" + word
}

// AttachImage stores image data (a clipboard paste) and returns its @ word:
// a copy under the drops directory, the only attachment with no source path.
func (h *Host) AttachImage(data []byte, ext string) (string, error) {
	if len(data) > attachImageMax {
		return "", fmt.Errorf("the image is %d MB; the limit is %d MB", len(data)>>20, attachImageMax>>20)
	}
	name, err := h.saveDrop("pasted-"+time.Now().Format("150405")+ext, data)
	if err != nil {
		return "", err
	}
	return h.AttachWord(filepath.Join(h.dropsDir(), name)), nil
}

// dropUnsafe is what a name cannot carry: a cell that would end the token, and
// the separators that would make it a path.
var dropUnsafe = regexp.MustCompile("[\x00-\x1f\x7f/\\\\]+")

// saveDrop writes data under an unused name of its own (the same content
// keeps its name) and returns that name. The name is the source's, cut to
// what a word carries and shortened only where it breaks one.
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
