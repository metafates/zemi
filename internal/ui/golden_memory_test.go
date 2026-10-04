package ui

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// TestGoldenMemory pins /memory: empty, then a suggestion over each scope's
// memories under a title with its count and budget, a suggestion's detail
// and a typed forget armed; an action applies at once and u undoes it.
func TestGoldenMemory(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	ctx := t.Context()
	if err := host.ForceReload(ctx, []lua.BootStep{host.NamespacesStep(), host.BuiltinCommandsStep(), lua.RequireStep("zemi.tui")}); err != nil {
		t.Fatal(err)
	}
	testStore(t, host)
	frame := func(name string) {
		t.Helper()
		for app.Pump() {
		}
		app.renderSync(t)
		t.Run(name, func(t *testing.T) { golden.RequireEqual(t, app.Frame()) })
	}
	run := func(args string) {
		t.Helper()
		if err := host.RunCommand(ctx, "memory", args); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
	}
	run("")
	frame("empty")
	pressKeys(app, "esc")

	old := time.Now().Add(-12 * 24 * time.Hour).UnixMilli()
	for _, m := range []*store.Memory{
		{ID: "m1", Scope: "project", Text: "the fts join needs m-qualified columns, since the memory table and its fts index share column names", Origin: store.OriginLearned, Quote: "qualify the fts columns with m"},
		{ID: "m2", Scope: "user", Text: "the user prefers short replies", Origin: store.OriginUser},
	} {
		m.ProvenanceSessionID, m.CreatedAt, m.UpdatedAt = "seed", old, old
		db := map[store.Scope]*store.DB{"project": host.DB, "user": host.GlobalDB}[m.Scope]
		if err := db.AddMemory(m); err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := json.Marshal(map[string]any{"ID": "p1", "Scope": "project", "Text": "run make check before every commit", "Origin": "learned", "ProvenanceSessionID": "seed"}, json.Deterministic(true))
	if err := host.DB.AddProposal(&store.Proposal{ID: "p1", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	run("")
	frame("list")
	pressKeys(app, "enter")
	frame("detail")
	pressKeys(app, "esc", "d") // drop the suggestion at once
	for app.Pump() {
	}
	if app.renderSync(t); strings.Contains(app.Frame(), "Suggested") {
		t.Fatalf("a dropped suggestion is still listed:\n%s", app.Frame())
	}
	pressKeys(app, "u")
	for app.Pump() {
	}
	if app.renderSync(t); !strings.Contains(app.Frame(), "Suggested by zemi (1)") {
		t.Fatalf("undo did not bring the suggestion back:\n%s", app.Frame())
	}
	pressKeys(app, "esc")
	run("forget m2")
	frame("forget armed")
}
