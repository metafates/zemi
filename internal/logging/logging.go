// Package logging opens zemi's log file: one per launch, pruned to the
// newest keepLogs, written through log/slog.
package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// keepLogs bounds the log directory: one file per launch, newest kept.
const keepLogs = 20

// New returns a logger writing one record per line to a new file under
// dir, the level var its handler reads, the file's path, and the func that
// closes the file; with no dir, or no file, the logger discards. The text
// handler quotes any key or value holding a newline, an escape or another
// unprintable rune, so Lua and the model cannot forge a record or reach the
// terminal of whoever reads the file.
func New(dir string) (*slog.Logger, *slog.LevelVar, string, func()) {
	if dir == "" || os.MkdirAll(dir, 0o755) != nil {
		return slog.New(slog.DiscardHandler), &slog.LevelVar{}, "", func() {}
	}
	prune(dir, keepLogs)
	path := filepath.Join(dir, time.Now().Format("2006-01-02_150405")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.New(slog.DiscardHandler), &slog.LevelVar{}, "", func() {}
	}
	var level slog.LevelVar
	level.Set(slog.LevelDebug)
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: &level})), &level, path, func() { _ = f.Close() }
}

// prune deletes all but the newest keep log files. The name is a
// timestamp, so lexical order is age order.
func prune(dir string, keep int) {
	names, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(names) <= keep {
		return
	}
	slices.Sort(names)
	for _, p := range names[:len(names)-keep] {
		_ = os.Remove(p)
	}
}
