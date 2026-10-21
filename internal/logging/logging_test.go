package logging

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
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
	l, _, path, closeLog := New(dir)
	forged := "\nERROR forged\x1b[31m\u009b\x9b\u202e\t"
	l.Info("msg"+forged, "origin", "lua"+forged, "k"+forged, "v"+forged, "error", errors.New("e"+forged))
	closeLog()
	if path == "" || !strings.HasSuffix(path, ".log") {
		t.Fatalf("no log file path: %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line, ok := strings.CutSuffix(string(data), "\n")
	if !ok || strings.ContainsFunc(line, func(r rune) bool { return !unicode.IsPrint(r) && r != ' ' }) {
		t.Fatalf("not one printable line: %q", data)
	}
}
