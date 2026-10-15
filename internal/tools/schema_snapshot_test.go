package tools

import (
	"bytes"
	"encoding/json/v2"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateSchemaSnapshot rewrites the golden the snapshot test compares.
var updateSchemaSnapshot = flag.Bool("update-schema", false, "rewrite the tool schema snapshot")

// TestToolSchemaSnapshot pins the marshaled [Parameters, Required, Parallel]
// of every builtin tool: the pin key hashes these bytes, so the typed-args
// migration must not move one. Temporary; deleted once the migration lands.
func TestToolSchemaSnapshot(t *testing.T) {
	var got []any
	for _, tl := range All(&Pipeline{}) {
		info := tl.Info()
		got = append(got, []any{info.Parameters, info.Required, info.Parallel})
	}
	b, err := json.Marshal(got, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "toolschemas.golden")
	if *updateSchemaSnapshot {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, want) {
		t.Errorf("tool schema bytes drifted (the pin key hashes these):\n%s\nwant:\n%s", b, want)
	}
}
