package hostapi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// TestRewindRefusesMidTurn: every rewind entry refuses while a turn runs -
// /rewind's popup, a direct RewindTo (the overlay's r key and
// zemi.session.rewind) and the /messages r key - and none cuts the log the
// turn is writing; idle, the same entries work.
func TestRewindRefusesMidTurn(t *testing.T) {
	for _, c := range []struct {
		name    string
		run     func(h *Host) error
		refused func(t *testing.T, h *Host, err error, ui *fakeUI)
		idle    func(t *testing.T, h *Host, err error, ui *fakeUI)
	}{
		{
			name: "the /rewind popup",
			run:  func(h *Host) error { return h.OpenRewind() },
			refused: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				if !errors.Is(err, errTurnRunning) {
					t.Fatalf("= %v, want the turn-running refusal", err)
				}
				if len(ui.opened) != 0 {
					t.Fatal("the popup opened mid-turn")
				}
			},
			idle: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				if err != nil || len(ui.opened) == 0 {
					t.Fatalf("= %v with %d popups, want the popup open", err, len(ui.opened))
				}
			},
		},
		{
			name: "RewindTo",
			run:  func(h *Host) error { _, err := h.RewindTo(9999); return err },
			refused: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				if !errors.Is(err, errTurnRunning) {
					t.Fatalf("= %v, want the turn-running refusal", err)
				}
			},
			idle: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				// past the turn guard, it refuses on the prompt it cannot find
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("= %v, want a usage error", err)
				}
			},
		},
		{
			name: "the /messages r key",
			run: func(h *Host) error {
				// the r key runs RewindTo through a command
				if _, err := h.Commands.Add(&Command{
					Name: "rewind", NArgs: NArgsOptional,
					Origin: lua.OriginBuiltin, Gen: h.currentGen(),
					GoFn: func(h *Host, args string, _ bool) error {
						seq, err := atoi64(args)
						if err != nil {
							return err
						}
						_, err = h.RewindTo(seq)
						return err
					},
				}); err != nil {
					return err
				}
				return h.RunCommand(t.Context(), "rewind", "9999", false)
			},
			refused: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				if !errors.Is(err, errTurnRunning) {
					t.Fatalf("= %v, want the turn-running refusal", err)
				}
			},
			idle: func(t *testing.T, h *Host, err error, ui *fakeUI) {
				if !errors.Is(err, ErrUsage) {
					t.Fatalf("= %v, want a usage error", err)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			// a provider that never answers holds the turn open
			h.modelFn = func() (fantasy.LanguageModel, error) {
				return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart { return nil }}, nil
			}
			if err := h.AgentSend("slow"); err != nil {
				t.Fatal(err)
			}
			c.refused(t, h, c.run(h), ui)
			h.AgentCancel()
			waitIdle(t, h)
			c.idle(t, h, c.run(h), ui)
		})
	}
}

func atoi64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// TestResetChat: /reset refuses mid-turn, asks alone (the armed screen),
// wipes the log, the tree and the view once answered, clears the todos, and
// the next turn is the log's seq 0 again on a builder that learned the empty
// state.
func TestResetChat(t *testing.T) {
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if _, err := h.DB.AppendLog(kindUser, "earlier", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.InsertNode(0, 0, "earlier"); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.AddCheckpoint(0, store.Checkpoint{Path: "/x", Existed: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.SetTodos(store.MainSessionID, []store.Todo{{Content: "a", Status: store.TodoPending}}); err != nil {
		t.Fatal(err)
	}

	// a provider that never answers holds the turn open
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart { return nil }}, nil
	}
	if err := h.AgentSend("slow"); err != nil {
		t.Fatal(err)
	}
	if err := h.ResetChat(false); !errors.Is(err, errTurnRunning) {
		t.Fatalf("unconfirmed mid-turn = %v, want the turn-running refusal", err)
	}
	if err := h.ResetChat(true); !errors.Is(err, errTurnRunning) {
		t.Fatalf("confirmed mid-turn = %v, want the turn-running refusal", err)
	}
	h.AgentCancel()
	waitIdle(t, h)

	// alone it asks: the screen opens armed, and nothing moved yet
	before, err := h.DB.LogCount()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ResetChat(false); err != nil {
		t.Fatal(err)
	}
	opened := ui.overlays()
	ask := opened[len(opened)-1]
	if ask.ID != "reset" || ask.Confirm != "reset --yes" || len(ask.Items) != 1 ||
		ask.Items[0].Keys[0].Run != "reset" || !ask.Items[0].Keys[0].Danger {
		t.Fatalf("the ask opened %+v", ask)
	}
	if n, _ := h.DB.LogCount(); n != before {
		t.Fatalf("%d log rows survived the ask, want the %d it held", n, before)
	}

	// --yes wipes and starts over
	if err := h.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.DB.LogCount(); n != 0 {
		t.Fatalf("%d log rows survived the reset", n)
	}
	if nodes, _ := h.DB.BuiltNodes(); len(nodes) != 0 {
		t.Fatalf("%d nodes survived the reset", len(nodes))
	}
	if lines, _ := h.DB.LoadView(); len(lines) != 0 {
		t.Fatalf("%d view lines survived the reset", len(lines))
	}
	if cps, _ := h.DB.CheckpointsFrom(0); len(cps) != 0 {
		t.Fatalf("%d checkpoints survived the reset", len(cps))
	}
	if todos := h.todoSnapshot(); len(todos) != 0 {
		t.Fatalf("todos %+v survived the reset", todos)
	}

	// a next turn works, and the log restarts at 0 with its summary
	reflectStub(t, h, "fresh reply")
	if err := h.AgentSend("again"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	rows, err := h.DB.LogFrom(0, 10)
	if err != nil || len(rows) != 2 || rows[0].Seq != 0 || rows[0].Kind != kindUser || rows[1].Kind != kindAgent {
		t.Fatalf("the next turn logged %+v, %v", rows, err)
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 0); err != nil {
			return errors.New("the builder did not rebuild over the empty log")
		}
		return nil
	})
	if tr := h.Session().Transcript; len(tr) == 0 || tr[0].Text != "again" || tr[0].Seq != 0 {
		t.Fatalf("the chat restarted with %+v", tr)
	}
}

// TestRewind runs turns that write files and todo lists, then rewinds them
// one by one: each rewind restores the files and the todo list as they were
// before that turn and cuts the log back to the prompt before it.
func TestRewind(t *testing.T) {
	h, _ := newTestHostDB(t)
	t.Chdir(h.Dirs.ProjectDir) // where shell runs and snapshots
	edited := filepath.Join(h.Dirs.ProjectDir, "a.txt")
	created := filepath.Join(h.Dirs.ProjectDir, "sub", "new.txt")
	if err := os.WriteFile(edited, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a repository, so a command's changes are snapshotted
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
		if out, err := h.git(h.Dirs.ProjectDir, args...); err != nil {
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
				return toolCallParts("c-rm", "shell", fmt.Sprintf(`{"command":"rm %s && echo y > %s"}`, edited, created))
			case toolResult:
				return textParts("done")
			}
			return textParts("?")
		}}, nil
	}
	var prompts []int64
	for _, text := range []string{"first", "second", "third"} {
		if err := h.AgentSend(text); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, h)
		rows, _ := h.DB.LogKindBack(kindUser, 100)
		for _, r := range rows {
			if strings.TrimSpace(r.Text) == text {
				prompts = append(prompts, r.Seq)
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
		if desc := ui.opened[0].Items[i].Desc; !strings.HasSuffix(desc, want) {
			t.Fatalf("prompt %d: %q, want %s", i, desc, want)
		}
	}

	for _, c := range []struct {
		prompt          int
		files           int
		edited, created string
		todos           int
		promptsLeft     int
	}{
		{prompt: 2, files: 2, edited: "v2", created: "x", todos: 2, promptsLeft: 2},
		{prompt: 1, files: 1, edited: "v2", created: "<missing>", todos: 1, promptsLeft: 1},
		{prompt: 0, files: 1, edited: "v1", created: "<missing>", todos: 0, promptsLeft: 0},
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
		users, _ := h.DB.LogKindBack(kindUser, 100)
		if len(users) != c.promptsLeft {
			t.Fatalf("rewind %d: %d prompts left, want %d", c.prompt, len(users), c.promptsLeft)
		}
	}
	if _, err := h.RewindTo(prompts[0] + 100); err == nil {
		t.Fatal("rewinding past the log must fail")
	}
}

// TestRewindCutPatch: the cut leaves every surviving message covered - the
// straddling view line goes and leaf lines patch its hole - no node
// intersects the cut, and re-appends keep the seq dense.
func TestRewindCutPatch(t *testing.T) {
	h, _ := newTestHostDB(t)
	// twelve messages; the middle four are summarized up to one 4+4 line
	for _, text := range []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11"} {
		if _, err := h.DB.AppendLog(kindAgent, text, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range [][2]int64{{0, 0}, {0, 1}, {1, 0}, {0, 4}, {0, 5}, {0, 6}, {0, 7}, {1, 2}, {1, 3}, {2, 1}} {
		if _, err := h.DB.InsertNode(n[0], n[1], fmt.Sprintf("node %d+%d", n[0], n[1])); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 2}, {L: 1, I: 3}, {L: 2, I: 1}} {
		if n, err := h.DB.MergeViewLines([]store.ViewMerge{m}); err != nil || n != 1 {
			t.Fatalf("merge %v: %d %v", m, n, err)
		}
	}
	// view: 0+4, 4+4 (straddles the cut below), then the leaves 8..11
	cut := int64(6)
	h.chat.quiesce()
	if err := h.DB.DeleteLogFrom(cut); err != nil {
		t.Fatal(err)
	}
	if err := h.patchView(cut); err != nil {
		t.Fatal(err)
	}
	h.chat.resume()
	lines, err := h.DB.LoadView()
	if err != nil {
		t.Fatal(err)
	}
	covered := make([]bool, cut)
	for _, ln := range lines {
		first, end := ln.I<<uint(ln.L), (ln.I+1)<<uint(ln.L)
		for s := max(int64(0), first); s < min(cut, end); s++ {
			covered[s] = true
		}
	}
	for s, ok := range covered {
		if !ok {
			t.Fatalf("message %d is covered by no view line after the cut", s)
		}
	}
	nodes, _ := h.DB.BuiltNodes()
	for _, n := range nodes {
		if end := (n.I + 1) << uint(n.L); end > cut {
			t.Fatalf("node l%d i%d covers up to %d, past the cut %d", n.L, n.I, end-1, cut)
		}
	}
	for _, text := range []string{"n0", "n1"} {
		if _, err := h.DB.AppendLog(kindUser, text, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if count, _ := h.DB.LogCount(); count != cut+2 {
		t.Fatalf("log holds %d rows, want %d dense", count, cut+2)
	}
}

// TestRewindSnapshotEdges builds the checkpoint rows by hand: a file
// checkpointed under a symlink and under its target is one file, also when
// the first turn creates it through a linked directory.
func TestRewindSnapshotEdges(t *testing.T) {
	for _, c := range []struct {
		name    string
		viaLink bool   // the first turn names the file through a symlink
		create  bool   // the first turn creates the file through a linked directory
		want    string // file content after rewinding to the first turn
	}{
		{name: "symlink and target", viaLink: true, want: "orig"},
		{name: "created through a linked directory", create: true, want: "<missing>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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
			var seqs []int64
			for _, text := range []string{"first", "second"} {
				seq, err := h.DB.AppendLog(kindUser, text, "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				seqs = append(seqs, seq)
			}
			for i, v := range []string{"v1", "v2"} {
				path := real
				if i == 0 && c.viaLink {
					path = link
				}
				if i == 0 && c.create {
					path = filepath.Join(h.Dirs.ProjectDir, "dir", "f.txt")
				}
				h.agents.mu.Lock()
				h.agents.turnSeq = seqs[i]
				h.agents.mu.Unlock()
				h.checkpointFile(path)
				if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.RewindTo(seqs[0]); err != nil {
				t.Fatal(err)
			}
			got := "<missing>"
			if b, err := os.ReadFile(real); err == nil {
				got = string(b)
			}
			if got != c.want {
				t.Fatalf("file = %q, want %q", got, c.want)
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

// TestRewindShellSymlink: a rewind puts back a symbolic link a command
// deleted or retargeted and removes one it created.
func TestRewindShellSymlink(t *testing.T) {
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
			t.Parallel()
			h, _ := newTestHostDB(t)
			dir := h.Dirs.ProjectDir
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
			os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
			if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"}} {
				if out, err := h.git(dir, args...); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			seq, err := h.DB.AppendLog(kindUser, "go", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			h.agents.mu.Lock()
			h.agents.turnSeq = seq
			h.agents.mu.Unlock()
			done := h.shellChanges(dir)
			if err := c.run(dir); err != nil {
				t.Fatal(err)
			}
			done()
			if _, err := h.RewindTo(seq); err != nil {
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
