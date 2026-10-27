package hostapi

import (
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

// TestResetEmitsSessionReset: a reset tells Lua, so state derived from the
// chat can drop with it. Another window's reset reaches the same event
// through its own chatDropped.
func TestResetEmitsSessionReset(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("session.reset", function(ev) zemi.state.set("reset_seen", ev.name) end)`)
	if err := h.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("reset_seen"); v != `"session.reset"` {
			return errors.New("handler has not run")
		}
		return nil
	})
}

// TestResetChat: /reset refuses mid-turn, asks alone (the armed screen),
// wipes the log, the tree and the view once answered, clears the todos and
// the goal, and the next turn is the log's seq 0 again on a builder that
// learned the empty state.
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
	if err := h.DB.SetTodos(store.MainSessionID, []store.Todo{{Content: "a", Status: store.TodoPending}}); err != nil {
		t.Fatal(err)
	}
	h.agents.mu.Lock()
	h.agents.goal = &Goal{Condition: "hold", SetAt: 0}
	h.agents.shellNotes = []string{agent.AttachElement(`shell command="make test" status="0"`, "ok")}
	h.agents.mu.Unlock()

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

	// --yes wipes and starts over; the one row left is the reset's own note
	if err := h.ResetChat(true); err != nil {
		t.Fatal(err)
	}
	if n, _ := h.DB.LogCount(); n != 1 {
		t.Fatalf("%d log rows survived the reset, want the reset note", n)
	}
	note, _ := h.DB.LogTail(1)
	if len(note) != 1 || note[0].Kind != kindNote || note[0].Text != "the chat was reset" {
		t.Fatalf("the reset's row = %+v, want the reset note", note)
	}
	if raw, err := h.DB.GetKV(store.CVKey); err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the compaction view survived the reset: %q, %v", raw, err)
	}
	if _, ok := h.Goal(); ok {
		t.Fatal("the goal survived the reset")
	}
	if notes := h.takeShellNotes(); notes != "" {
		t.Fatalf("shell output from the wiped chat survived: %q", notes)
	}
	if todos := h.todoSnapshot(); len(todos) != 0 {
		t.Fatalf("todos %+v survived the reset", todos)
	}

	// a next turn works, behind the reset's note, and the log restarts at 0
	replyStub(t, h, "fresh reply")
	if err := h.AgentSend("again"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	rows, err := h.DB.LogFrom(0, 10)
	if err != nil || len(rows) != 3 || rows[0].Seq != 0 || rows[0].Kind != kindNote ||
		rows[1].Kind != kindUser || rows[2].Kind != kindAgent {
		t.Fatalf("the next turn logged %+v, %v", rows, err)
	}
	waitFor(t, func() error {
		if _, err := h.DB.NodeAt(0, 1); err != nil {
			return errors.New("the builder did not rebuild over the empty log")
		}
		return nil
	})
	if tr := h.Session().Transcript; len(tr) == 0 || tr[0].Text != "again" || tr[0].Seq != 1 {
		t.Fatalf("the chat restarted with %+v", tr)
	}
}
