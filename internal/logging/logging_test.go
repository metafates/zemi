package logging

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPruneKeepsNewestLogs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"2024-01-01_000001.log", "2024-01-02_000002.log", "2024-01-03_000003.log", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prune(dir, 2)
	var got []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"2024-01-02_000002.log", "2024-01-03_000003.log", "notes.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestLogIsOneLine(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Info("lua\nWARN", "msg\nERROR forged", Fields{"k\nERROR forged": "v\n"})
	l.Close()
	names, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	data, err := os.ReadFile(names[0])
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("%d lines: %q", n, data)
	}
}
