// drop.go takes files dragged onto the terminal and images on the
// clipboard. A terminal delivers a drop as a paste of the file's path:
// shell-escaped (My\ File.png), quoted ('/a b.png'), or as a file:// URL,
// several separated by spaces or newlines. A paste made only of existing
// paths becomes attachment tokens (hostapi.AttachPath); anything else stays
// text. ctrl+v reads an image from the system clipboard, or its text.
package ui

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/proc"
)

// parseDrop reports the paths a paste consists of; ok is false unless every
// token names an existing file or directory.
func parseDrop(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64<<10 {
		return nil, false
	}
	var tokens []string
	var cur strings.Builder
	var quote rune
	started, escaped := false, false
	escapes := runtime.GOOS != "windows" // backslash separates Windows paths
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && escapes && quote != '\'':
			escaped, started = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case unicode.IsSpace(r):
			if started {
				tokens = append(tokens, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if started {
		tokens = append(tokens, cur.String())
	}
	if len(tokens) == 0 || len(tokens) > 50 {
		return nil, false
	}
	paths := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if strings.HasPrefix(t, "file://") {
			u, err := url.Parse(t)
			if err != nil {
				return nil, false
			}
			t = u.Path
		} else if strings.HasPrefix(t, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, false
			}
			t = filepath.Join(home, t[2:])
		}
		if !filepath.IsAbs(t) {
			return nil, false
		}
		if _, err := os.Stat(t); err != nil {
			return nil, false
		}
		paths = append(paths, filepath.Clean(t))
	}
	return paths, true
}

// insertAttachments puts one token per dropped path at the cursor, spaced
// from the text around it.
func (a *App) insertAttachments(paths []string) {
	tokens := make([]string, 0, len(paths))
	for _, p := range paths {
		tokens = append(tokens, a.host.AttachPath(p))
	}
	a.insertToken(strings.Join(tokens, " "))
}

func (a *App) insertToken(tok string) {
	r := a.composer.runes()
	if c := a.composer.cursor; c > 0 && c <= len(r) && !unicode.IsSpace(r[c-1]) {
		tok = " " + tok
	}
	a.composer.insert(tok + " ")
	a.completion.close()
}

type clipboardMsg struct {
	token string
	err   error
}

// pasteClipboard reads the system clipboard off the key loop: an image
// becomes an attachment, text is pasted like a terminal paste.
func (a *App) pasteClipboard() tea.Cmd {
	return func() tea.Msg {
		if data := clipboardImage(); len(data) > 0 {
			tok, err := a.host.AttachImage(data, ".png")
			return clipboardMsg{token: tok, err: err}
		}
		if text := clipboardText(); text != "" {
			return tea.PasteMsg{Content: text}
		}
		return clipboardMsg{err: errors.New("the clipboard has no image or text zemi can read")}
	}
}

func clipOut(name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, name, args...)
	out := &proc.Buffer{Max: 12 << 20}
	cmd.Stdout = out
	if proc.Run(cmd) != nil {
		return nil
	}
	return []byte(out.String())
}

func has(name string) bool {
	_, err := execLookPath(name)
	return err == nil
}

// clipboardImage returns a PNG from the clipboard, or nil.
func clipboardImage() []byte {
	switch {
	case runtime.GOOS == "darwin":
		f, err := os.CreateTemp("", "zemi-clip-*.png")
		if err != nil {
			return nil
		}
		f.Close()
		defer os.Remove(f.Name())
		script := `set f to open for access (POSIX file "` + f.Name() + `") with write permission
write (the clipboard as «class PNGf») to f
close access f`
		if clipOut("osascript", "-e", script) == nil {
			return nil
		}
		data, _ := os.ReadFile(f.Name())
		return data
	}
	// each in turn: wl-paste is installed on X11 sessions too, where it
	// answers nothing and xclip has to get its chance
	if has("wl-paste") && strings.Contains(string(clipOut("wl-paste", "--list-types")), "image/png") {
		if out := clipOut("wl-paste", "--type", "image/png"); len(out) > 0 {
			return out
		}
	}
	if has("xclip") && strings.Contains(string(clipOut("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")), "image/png") {
		return clipOut("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	}
	return nil
}

func clipboardText() string {
	if runtime.GOOS == "darwin" {
		return string(clipOut("pbpaste"))
	}
	// each in turn, like the copy path: wl-paste is installed on X11
	// sessions too, where it exits without output
	for _, argv := range [][]string{
		{"wl-paste", "--no-newline"},
		{"xclip", "-selection", "clipboard", "-o"},
		{"xsel", "--clipboard", "--output"},
	} {
		if has(argv[0]) {
			if out := clipOut(argv[0], argv[1:]...); len(out) > 0 {
				return string(out)
			}
		}
	}
	return ""
}

var execLookPath = exec.LookPath // swapped in tests
