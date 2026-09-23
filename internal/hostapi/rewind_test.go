package hostapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// TestRewind runs turns that write files and todo lists, then rewinds them
// one by one: each rewind restores the files and the todo list as they were
// before that turn, cuts the conversation back to the prompt before it and
// undoes what the session learned since.
func TestRewind(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	t.Chdir(h.Dirs.ProjectDir) // where bash runs and snapshots
	edited := filepath.Join(h.Dirs.ProjectDir, "a.txt")
	created := filepath.Join(h.Dirs.ProjectDir, "sub", "new.txt")
	if err := os.WriteFile(edited, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a repository, so a command's changes are snapshotted
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		if out, err := h.runGit(h.Dirs.ProjectDir, nil, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(path, content string) []fantasy.StreamPart {
		return toolCallParts("c-"+content, "write", fmt.Sprintf(`{"path":%q,"content":%q}`, path, content))
	}
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case last == "first":
				return toolCallParts("c-todo1", "todos", `{"todos":[{"content":"a","status":"pending"}]}`)
			// writing over a file means reading it first
			case strings.HasPrefix(last, "updated todos: 0/1"):
				return toolCallParts("c-read", "read", fmt.Sprintf(`{"path":%q,"limit":0}`, edited))
			case strings.Contains(last, "v1"):
				return write(edited, "v2")
			case last == "second":
				return toolCallParts("c-todo2", "todos", `{"todos":[{"content":"a","status":"done"},{"content":"b","status":"pending"}]}`)
			case strings.HasPrefix(last, "updated todos: 1/2"):
				return write(created, "x")
			// a command deletes one file and overwrites another: rewind restores both
			case last == "third":
				return toolCallParts("c-rm", "bash", fmt.Sprintf(`{"command":"rm %s && echo y > %s"}`, edited, created))
			case toolResult:
				return textParts("done")
			}
			return textParts("?")
		}}, nil
	}
	var prompts []string
	for _, text := range []string{"first", "second", "third"} {
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
	if read(edited) != "<missing>" || read(created) != "y\n" {
		t.Fatalf("turns did not write: %q %q", read(edited), read(created))
	}
	// each prompt counts the files a rewind to it restores, later turns' too
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.OpenRewind(); err != nil || len(ui.opened) != 1 {
		t.Fatal(err)
	}
	for i, want := range []string{"2 files", "2 files", "2 files"} {
		if desc := ui.opened[0].Items[i].(map[string]any)["desc"].(string); !strings.HasSuffix(desc, want) {
			t.Fatalf("prompt %d: %q, want %s", i, desc, want)
		}
	}

	// what the session learned in the rewound turns goes; the user's own and
	// what was learned after the last row (a reflector run, an accept) stay
	sid := h.AgentSessionID()
	rows, _ := h.DB.Messages(sid)
	last := rows[len(rows)-1].CreatedAt
	old := &store.Memory{Scope: "project", Text: "old", Origin: "learned", ProvenanceSessionID: sid, CreatedAt: 1}
	learned := &store.Memory{Scope: "project", Text: "learned", Origin: "learned", ProvenanceSessionID: sid, CreatedAt: last}
	reflected := &store.Memory{Scope: "project", Text: "reflected", Origin: "learned", ProvenanceSessionID: sid, CreatedAt: last + 1}
	mine := &store.Memory{Scope: "project", Text: "mine", Origin: "user", ProvenanceSessionID: sid}
	for _, m := range []*store.Memory{old, learned, reflected, mine} {
		if err := h.DB.AddMemory(m); err != nil {
			t.Fatal(err)
		}
	}
	h.DB.SupersedeMemory(old.ID, learned.ID)
	for _, at := range []int64{last, last + 1} {
		payload, _ := json.Marshal(memoryProposal{Memory: store.Memory{Scope: "project", Text: fmt.Sprint(at), Origin: "learned", ProvenanceSessionID: sid}})
		if err := h.DB.AddProposal(&store.Proposal{Kind: "memory", Payload: string(payload), CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		prompt           int
		files            int
		edited, created  string
		todos            int
		transcriptBefore int // user messages left
	}{
		{prompt: 2, files: 2, edited: "v2", created: "x", todos: 2, transcriptBefore: 2},
		{prompt: 1, files: 1, edited: "v2", created: "<missing>", todos: 1, transcriptBefore: 1},
		{prompt: 0, files: 1, edited: "v1", created: "<missing>", todos: 0, transcriptBefore: 0},
	} {
		n, err := h.RewindTo(prompts[c.prompt])
		if err != nil {
			t.Fatal(err)
		}
		if n != c.files || read(edited) != c.edited || read(created) != c.created {
			t.Fatalf("rewind %d: restored %d, files %q %q", c.prompt, n, read(edited), read(created))
		}
		if todos := h.todoSnapshot(); len(todos) != c.todos {
			t.Fatalf("rewind %d: todos %+v, want %d", c.prompt, todos, c.todos)
		}
		// the next send's compaction check reads it
		if h.AgentStream().LastIn == 0 {
			t.Fatalf("rewind %d: no context estimate", c.prompt)
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
	live, _ := h.DB.ListMemory("project", false)
	var texts []string
	for _, m := range live {
		texts = append(texts, m.Text)
	}
	if slices.Sort(texts); !slices.Equal(texts, []string{"mine", "old", "reflected"}) || len(h.memoryProposals()) != 1 {
		t.Fatalf("memories %q and %d proposals, want mine, old and reflected and one", texts, len(h.memoryProposals()))
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
		name     string
		firstAge time.Duration
		viaLink  bool   // the first turn names the file through a symlink
		create   bool   // the first turn creates the file through a linked directory
		want     string // file content after rewinding to the first turn
		warn     string // substring of the rewind's warning, "" for none
	}{
		{name: "symlink and target", viaLink: true, want: "orig"},
		{name: "created through a linked directory", create: true, want: "<missing>"},
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
				if i > 0 || c.firstAge == 0 {
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

// TestRewindBashSymlink: a rewind puts back a symbolic link a command
// deleted or retargeted and removes one it created.
func TestRewindBashSymlink(t *testing.T) {
	for _, c := range []struct {
		name, path string
		run        func(dir string) error
		want       string // the link's target after the rewind, "" for no file there
	}{
		{name: "deleted", path: "link", want: "a.txt", run: func(dir string) error { return os.Remove(filepath.Join(dir, "link")) }},
		{name: "retargeted", path: "link", want: "a.txt", run: func(dir string) error {
			os.Remove(filepath.Join(dir, "link"))
			return os.Symlink("b.txt", filepath.Join(dir, "link"))
		}},
		{name: "created", path: "new", run: func(dir string) error { return os.Symlink("a.txt", filepath.Join(dir, "new")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			dir := h.Dirs.ProjectDir
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
			os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
			if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
				if out, err := h.runGit(dir, nil, args...); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			h.DB.CreateSession("s", "", "s", "")
			parts, _ := json.Marshal(fantasy.NewUserMessage("go"))
			m := &store.Message{SessionID: "s", Role: "user", PartsJSON: string(parts)}
			h.DB.AddMessage(m)
			if err := h.ResumeSession("s"); err != nil {
				t.Fatal(err)
			}
			h.agents.turnMsgID = m.ID
			done := h.bashChanges(dir)
			if err := c.run(dir); err != nil {
				t.Fatal(err)
			}
			done()
			if _, err := h.RewindTo(m.ID); err != nil {
				t.Fatal(err)
			}
			got, err := os.Readlink(filepath.Join(dir, c.path))
			if _, missing := os.Lstat(filepath.Join(dir, c.path)); err != nil && missing == nil {
				got = "<not a link>"
			}
			if got != c.want {
				t.Fatalf("%s is %q, want %q", c.path, got, c.want)
			}
		})
	}
}
