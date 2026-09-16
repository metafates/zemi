// Package logging writes sanitized, one-record-per-line entries to a file
// and keeps an in-memory ring for the /errors surface.
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

const ringSize = 512

// Entry is one log record kept in the ring.
type Entry struct {
	Time    time.Time
	Level   Level
	Origin  string
	Message string
	Fields  string
}

// Logger is safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	file io.WriteCloser
	ring []Entry
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
	origin, msg = sanitize.LogLine(origin), sanitize.LogLine(msg)
	pairs := make([]string, 0, len(fields))
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		pairs = append(pairs, sanitize.LogLine(k)+"="+sanitize.LogLine(fmt.Sprint(fields[k])))
	}
	fs := strings.Join(pairs, " ")
	e := Entry{Time: time.Now(), Level: level, Origin: origin, Message: msg, Fields: fs}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring = append(l.ring, e)
	if len(l.ring) > ringSize {
		l.ring = l.ring[len(l.ring)-ringSize:]
	}
	if l.file != nil {
		line := fmt.Sprintf("%s %s %s %s", e.Time.Format(time.RFC3339Nano), level, origin, msg)
		if fs != "" {
			line += " " + fs
		}
		fmt.Fprintln(l.file, line)
	}
}

func (l *Logger) Debug(origin, msg string, fields Fields) { l.Log(Debug, origin, msg, fields) }
func (l *Logger) Info(origin, msg string, fields Fields)  { l.Log(Info, origin, msg, fields) }
func (l *Logger) Warn(origin, msg string, fields Fields)  { l.Log(Warn, origin, msg, fields) }
func (l *Logger) Error(origin, msg string, fields Fields) { l.Log(Error, origin, msg, fields) }

// Recent returns the newest n entries (oldest first).
func (l *Logger) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n = min(n, len(l.ring))
	if n <= 0 {
		return nil
	}
	out := make([]Entry, n)
	copy(out, l.ring[len(l.ring)-n:])
	return out
}
