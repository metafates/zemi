package ui

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
)

// brainSeed writes m to its partition's brain the way a store write did:
// the line with its id, origin and age, the quote and provenance in the
// ledger. (ui tests have no hostapi test helpers.)
func brainSeed(t *testing.T, host *hostapi.Host, m *store.Memory) {
	t.Helper()
	user := brain.UserPartition(host.Dirs.DataHome, host.Dirs.StateHome)
	private := brain.PrivatePartition(host.Dirs.StateHome, host.Dirs.TrustRoot())
	p := private
	if m.Scope == store.ScopeUser {
		p = user
	}
	if p.Zero() {
		t.Fatalf("no %s partition to seed", m.Scope)
	}
	if _, err := os.Stat(p.DB); errors.Is(err, fs.ErrNotExist) {
		db, err := store.Open(p.DB)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	date := time.UnixMilli(m.UpdatedAt).UTC()
	if m.UpdatedAt == 0 {
		date = time.Now().UTC()
	}
	line := brain.Line{ID: m.ID, Origin: m.Origin, Date: date, Text: m.Text}
	path := filepath.Join(p.Dir, brain.MemFile)
	cur, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(cur, []byte(line.String()+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	op := &brain.Op{
		Op: brain.OpAdd, ID: m.ID, Text: m.Text, Quote: m.Quote,
		SessionID: cmp.Or(m.ProvenanceSessionID, "seed"), Hash: brain.FileHash([]byte(m.Text)),
		Scope: m.Scope, Origin: m.Origin, TS: m.UpdatedAt,
	}
	if err := lg.Insert(op); err != nil {
		t.Fatal(err)
	}
}

// TestGoldenMemory pins /memory: empty, then a suggestion over each scope's
// memories under a title with its count and budget, a suggestion's detail
// and a typed forget armed; an action applies at once and u undoes it.
func TestGoldenMemory(t *testing.T) {
	app, host := bootAgentAppPinned(t, 80, 24)
	ctx := t.Context()
	// the private partition keys on the repository, so the golden seeds one;
	// a fixed basename keeps the statusline's project label stable, and a long
	// one keeps it the name: a shortened temp path varies by machine
	repo := filepath.Join(t.TempDir(), "memory-golden-repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	host.Dirs.ProjectDir = repo
	host.RekeyProject()
	reloadUI(t, host)
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
		if err := host.RunCommand(ctx, "memory", args, false); err != nil {
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
		{ID: "aa1mem1m", Scope: "project", Text: "the fts join needs m-qualified columns, since the memory table and its fts index share column names", Origin: store.OriginLearned, Quote: "qualify the fts columns with m"},
		{ID: "aa2mem2m", Scope: "user", Text: "the user prefers short replies", Origin: store.OriginUser},
	} {
		m.ProvenanceSessionID, m.CreatedAt, m.UpdatedAt = "seed", old, old
		brainSeed(t, host, m)
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
	run("forget aa2mem2m")
	frame("forget armed")
}
