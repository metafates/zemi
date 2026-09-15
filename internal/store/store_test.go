package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "parent0", "first", "coder"); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "first" || s.Profile != "coder" || s.ParentID != "parent0" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if _, err := db.GetSession("missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.RenameSession("s1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUsage("s1", 10, 20, 0.5); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetSession("s1")
	if s.Title != "renamed" || s.PromptTokens != 10 || s.CompletionTokens != 20 || s.Cost != 0.5 {
		t.Fatalf("usage/rename not applied: %+v", s)
	}
	if err := db.CreateSession("top", "", "top", ""); err != nil {
		t.Fatal(err)
	}
	all, err := db.ListSessions(10)
	if err != nil || len(all) != 1 || all[0].ID != "top" {
		t.Fatalf("list must hold only the top-level session: %v %v", all, err)
	}
}

func TestMessagesAndToolCalls(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	m := &Message{SessionID: "s1", Role: "user", PartsJSON: `[{"type":"text","text":"hi"}]`}
	if err := db.AddMessage(m); err != nil {
		t.Fatal(err)
	}
	if m.ID == "" || m.CreatedAt == 0 {
		t.Fatal("AddMessage must fill id and timestamp")
	}
	if err := db.AddMessage(&Message{ID: "m2", SessionID: "s1", Role: "assistant", PartsJSON: `[]`}); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.Messages("s1")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages: %v %v", msgs, err)
	}
	if msgs[0].ID != m.ID {
		t.Fatalf("messages not in order: %+v", msgs)
	}
	if err := db.AddToolCall(&ToolCall{MessageID: m.ID, Tool: "bash", InputJSON: `{}`, OK: false, ErrorKind: "denied", Denied: true, DecidedBy: "user"}); err != nil {
		t.Fatal(err)
	}
}

func TestTodosReplace(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	in := []Todo{{Content: "a", Status: "pending"}, {Content: "b", Status: "done", ActiveForm: "doing b"}}
	if err := db.SetTodos("s1", in); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTodos("s1", in); err != nil {
		t.Fatal(err)
	}
	out, err := db.Todos("s1")
	if err != nil || len(out) != 2 {
		t.Fatalf("todos: %v %v", out, err)
	}
	if out[1].Content != "b" || out[1].Ord != 1 {
		t.Fatalf("order wrong: %+v", out)
	}
}

func TestKVRoundtrip(t *testing.T) {
	db := openTest(t)
	if _, err := db.GetKV("k"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.SetKV("k", `{"v":1}`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKV("k", `{"v":2}`); err != nil {
		t.Fatal(err)
	}
	v, err := db.GetKV("k")
	if err != nil || v != `{"v":2}` {
		t.Fatalf("kv: %q %v", v, err)
	}
}

func TestProjectDBPath(t *testing.T) {
	if got := ProjectDBPath("/state", ""); got != "/state/zemi.db" {
		t.Fatalf("global path: %s", got)
	}
	p := ProjectDBPath("/state", "/home/u/proj")
	if filepath.Dir(filepath.Dir(p)) != filepath.Join("/state", "projects") {
		t.Fatalf("project path: %s", p)
	}
	if p == ProjectDBPath("/state", "/home/u/other") {
		t.Fatal("different projects must hash differently")
	}
}

func TestOpenForDirs(t *testing.T) {
	state := t.TempDir()
	g, p, err := OpenForDirs(state, "/tmp/someproj")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defer p.Close()
	if err := p.CreateSession("s1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.SetKV("x", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSince(t *testing.T) {
	db := openTest(t)
	for _, s := range []struct{ id, parent string }{{"a", ""}, {"b", ""}, {"child", "a"}} {
		if err := db.CreateSession(s.id, s.parent, s.id, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AddUsage(s.id, 100, 10, 0.5); err != nil {
			t.Fatal(err)
		}
	}
	u, err := db.UsageSince(0)
	if err != nil || u.Sessions != 2 || u.Prompt != 200 || u.Completion != 20 || u.Cost != 1 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	if u, _ := db.UsageSince(now() + 60_000); u.Sessions != 0 {
		t.Fatalf("future window = %+v", u)
	}
}
