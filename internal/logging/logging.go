// Package logging writes sanitized, one-record-per-line entries to a file.
package logging

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/sanitize"
)

// Level is a log severity.
type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	default:
		return "ERROR"
	}
}

// Fields are structured key/value pairs appended to a record.
type Fields map[string]any

// Logger is safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	file io.WriteCloser
}

// keepLogs bounds the log directory: one file per launch, newest kept.
const keepLogs = 20

// New opens (or creates) the log file under dir.
func New(dir string) *Logger {
	l := &Logger{}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			prune(dir, keepLogs)
			path := filepath.Join(dir, time.Now().Format("2006-01-02_150405")+".log")
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				l.file = f
			}
		}
	}
	return l
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

// Close flushes and closes the file sink.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// Log writes one record.
func (l *Logger) Log(level Level, origin, msg string, fields Fields) {
	// Lua picks the field keys, so they are sanitized like the values
	var line strings.Builder
	fmt.Fprintf(&line, "%s %s %s %s", time.Now().Format(time.RFC3339Nano), level, sanitize.LogLine(origin), sanitize.LogLine(msg))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		fmt.Fprintf(&line, " %s=%s", sanitize.LogLine(k), sanitize.LogLine(fmt.Sprint(fields[k])))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		fmt.Fprintln(l.file, line.String())
	}
}

func (l *Logger) Debug(origin, msg string, fields Fields) { l.Log(Debug, origin, msg, fields) }
func (l *Logger) Info(origin, msg string, fields Fields)  { l.Log(Info, origin, msg, fields) }
func (l *Logger) Warn(origin, msg string, fields Fields)  { l.Log(Warn, origin, msg, fields) }
func (l *Logger) Error(origin, msg string, fields Fields) { l.Log(Error, origin, msg, fields) }
