package hostapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
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

// TestRewindSnapshotEdges builds the checkpoint rows by hand: a file
// checkpointed under a symlink and under its target is one file (also when
// the first turn creates it through a linked directory), and a
// rewind over a turn whose snapshots aged out warns even when later turns
// left some.
func TestRewindSnapshotEdges(t *testing.T) {
	for _, c := range []struct {
		name      string
		firstAge  time.Duration
		viaLink   bool   // the first turn names the file through a symlink
		create    bool   // the first turn creates the file through a linked directory
		keepFirst bool   // the sweep left the first turn's snapshot
		want      string // file content after rewinding to the first turn
		warn      string // substring of the rewind's warning, "" for none
	}{
		{name: "symlink and target", viaLink: true, keepFirst: true, want: "orig"},
		{name: "created through a linked directory", create: true, keepFirst: true, want: "<missing>"},
		{name: "partly aged out", firstAge: store.CheckpointTTL + time.Hour, want: "v1", warn: "1 of those turn"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			real := filepath.Join(h.Dirs.ProjectDir, "f.txt")
			link := filepath.Join(h.Dirs.ProjectDir, "link.txt")
			if err := os.WriteFile(real, []byte("orig"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(h.Dirs.ProjectDir, filepath.Join(h.Dirs.ProjectDir, "dir")); err != nil {
				t.Fatal(err)
			}
			if c.create {
				os.Remove(real)
			}
			if err := h.DB.CreateSession("s", "", "s", ""); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for i, text := range []string{"first", "second"} {
				parts, _ := json.Marshal(fantasy.NewUserMessage(text))
				m := &store.Message{SessionID: "s", Role: "user", PartsJSON: string(parts)}
				if i == 0 && c.firstAge > 0 {
					m.CreatedAt = time.Now().Add(-c.firstAge).UnixMilli()
				}
				if err := h.DB.AddMessage(m); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.ID)
			}
			if err := h.ResumeSession("s"); err != nil {
				t.Fatal(err)
			}
			for i, v := range []string{"v1", "v2"} {
				path := real
				if i == 0 && c.viaLink {
					path = link
				}
				if i == 0 && c.create {
					path = filepath.Join(h.Dirs.ProjectDir, "dir", "f.txt")
				}
				if i > 0 || c.keepFirst {
					h.agents.turnMsgID = ids[i]
					h.checkpointFile(path)
				}
				if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.RewindTo(ids[0]); err != nil {
				t.Fatal(err)
			}
			got := "<missing>"
			if b, err := os.ReadFile(real); err == nil {
				got = string(b)
			}
			if got != c.want {
				t.Fatalf("file = %q, want %q", got, c.want)
			}
			warned := ""
			for _, n := range h.evlog.notes {
				if n.level == "warn" {
					warned += n.body
				}
			}
			if (c.warn == "") != (warned == "") || !strings.Contains(warned, c.warn) {
				t.Fatalf("warning %q, want %q", warned, c.warn)
			}
		})
	}
}

// TestRestoreRefusesSwappedSymlink: a directory replaced by a symlink after
// the snapshot must not carry the restored content out of the project.
func TestRestoreRefusesSwappedSymlink(t *testing.T) {
	proj, outside := perm.RealPath(t.TempDir()), t.TempDir()
	sub := filepath.Join(proj, "sub")
	cp := store.Checkpoint{Path: filepath.Join(sub, "a.sh"), Existed: true, Content: []byte("payload")}
	if err := restoreCheckpoint(cp); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(sub)
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if err := restoreCheckpoint(cp); err == nil {
		t.Fatal("restored through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "a.sh")); err == nil {
		t.Fatal("wrote outside the project")
	}
}
