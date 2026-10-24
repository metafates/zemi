package store

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	schemaBegin = "<!-- schema:begin -->"
	schemaEnd   = "<!-- schema:end -->"
)

var update = flag.Bool("update", false, "rewrite runtime/skills/zemi/db.md's schema section")

// TestSchemaDocIsCurrent: the schema section of the db skill rides every
// session that reads it, so it cannot drift from the migrations. -update
// rewrites it (make golden).
func TestSchemaDocIsCurrent(t *testing.T) {
	doc, err := SchemaDoc()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "runtime", "skills", "zemi", "db.md")
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(whole), schemaBegin)
	j := strings.Index(string(whole), schemaEnd)
	if i < 0 || j < i {
		t.Fatalf("%s lost its %s markers", path, schemaBegin)
	}
	want := "\n" + doc
	if *update {
		b := string(whole)[:i+len(schemaBegin)] + want + string(whole)[j:]
		if err := os.WriteFile(path, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got := string(whole)[i+len(schemaBegin) : j]; got != want {
		t.Errorf("%s holds a stale schema (run make golden); got:\n%s\nwant:\n%s", path, got, want)
	}
}
