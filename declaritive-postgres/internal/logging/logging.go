package logging

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"regexp"
	"sync"

	"github.com/jptrs93/goutil/logu"
)

// relay wraps plain-text lines from child processes. It accepts every level so
// a DEBUG marker in relayed output is preserved rather than filtered.
var relay = logu.NewJSONLogger(os.Stdout, slog.LevelDebug)

// Setup installs the supervisor's JSON line logger as the slog default.
func Setup() {
	slog.SetDefault(logu.NewJSONLogger(os.Stdout, slog.LevelInfo))
}

// Lines longer than this are emitted in pieces rather than buffered further.
const maxLineLength = 64 << 10

// A LineWriter converts a plain-text stream into one JSON log record per line,
// reading PostgreSQL and pgBackRest severity markers to pick the level.
type LineWriter struct {
	mu  sync.Mutex
	buf []byte
}

func NewLineWriter() *LineWriter {
	return &LineWriter{}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > maxLineLength {
				emitLine(w.buf)
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		emitLine(w.buf[:i])
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
	}
}

// Flush emits any buffered partial line, for use once the stream has ended.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		emitLine(w.buf)
		w.buf = w.buf[:0]
	}
}

func emitLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	relay.Log(context.Background(), DetectLevel(line), string(line))
}

var severityMarker = regexp.MustCompile(`(?:^|[\s\[(])(TRACE|DEBUG[1-5]?|DETAIL|LOG|INFO|NOTICE|STATEMENT|HINT|CONTEXT|QUERY|WARNING|WARN|ERROR|FATAL|PANIC): `)

// DetectLevel maps a PostgreSQL or pgBackRest severity marker within a
// plain-text line to a slog level, defaulting to info.
func DetectLevel(line []byte) slog.Level {
	match := severityMarker.FindSubmatch(line)
	if match == nil {
		return slog.LevelInfo
	}
	switch severity := string(match[1]); severity {
	case "WARNING", "WARN":
		return slog.LevelWarn
	case "ERROR", "FATAL", "PANIC":
		return slog.LevelError
	case "TRACE", "DETAIL":
		return slog.LevelDebug
	default:
		if len(severity) >= 5 && severity[:5] == "DEBUG" {
			return slog.LevelDebug
		}
		return slog.LevelInfo
	}
}
