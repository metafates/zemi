package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

// TestRewind runs two turns that write files, then rewinds the second and
// the first: each rewind restores the files as they were before that turn
// and cuts the conversation back to the prompt before it.
func TestRewind(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	edited := filepath.Join(h.Dirs.ProjectDir, "a.txt")
	created := filepath.Join(h.Dirs.ProjectDir, "sub", "new.txt")
	if err := os.WriteFile(edited, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) []fantasy.StreamPart {
		return toolCallParts("c-"+content, "write", fmt.Sprintf(`{"path":%q,"content":%q}`, path, content))
	}
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			// writing over a file means reading it first
			case last == "first":
				return toolCallParts("c-read", "read", fmt.Sprintf(`{"path":%q}`, edited))
			case strings.Contains(last, "v1"):
				return write(edited, "v2")
			case last == "second":
				return write(created, "x")
			case toolResult:
				return textParts("done")
			}
			return textParts("?")
		}}, nil
	}
	var prompts []string
	for _, text := range []string{"first", "second"} {
		if err := h.AgentSend(text); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, h)
		rows, _ := h.DB.Messages(h.AgentSessionID())
		for _, r := range rows {
			if s, ok := userText(r); ok && s == text {
				prompts = append(prompts, r.ID)
			}
		}
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	if read(edited) != "v2" || read(created) != "x" {
		t.Fatalf("turns did not write: %q %q", read(edited), read(created))
	}

	for _, c := range []struct {
		prompt           int
		files            int
		edited, created  string
		transcriptBefore int // user messages left
	}{
		{prompt: 1, files: 1, edited: "v2", created: "<missing>", transcriptBefore: 1},
		{prompt: 0, files: 1, edited: "v1", created: "<missing>", transcriptBefore: 0},
	} {
		n, err := h.RewindTo(prompts[c.prompt])
		if err != nil {
			t.Fatal(err)
		}
		if n != c.files || read(edited) != c.edited || read(created) != c.created {
			t.Fatalf("rewind %d: restored %d, files %q %q", c.prompt, n, read(edited), read(created))
		}
		users := 0
		for _, m := range h.Session().Transcript {
			if m.Role == "user" {
				users++
			}
		}
		rows, _ := h.DB.Messages(h.AgentSessionID())
		left := 0
		for _, r := range rows {
			if _, ok := userText(r); ok {
				left++
			}
		}
		if users != c.transcriptBefore || left != c.transcriptBefore {
			t.Fatalf("rewind %d: transcript users %d, stored prompts %d, want %d", c.prompt, users, left, c.transcriptBefore)
		}
	}
	if _, err := h.RewindTo(prompts[0]); err == nil {
		t.Fatal("rewinding to a removed prompt must fail")
	}
}
