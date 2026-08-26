package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	// LogDirectory holds the named pipes the PostgreSQL logging collector
	// writes to; nothing is persisted to disk.
	LogDirectory = "/var/run/postgres-supervisor/log"
	// LogFileName is the forced log_filename. The collector derives
	// <LogFileName>.json for jsonlog output and uses the bare name for raw
	// stderr it captures from child processes such as archive_command.
	LogFileName = "postgres"

	postgresUID = 999
	postgresGID = 999
)

func jsonLogPath() string { return filepath.Join(LogDirectory, LogFileName+".json") }
func rawLogPath() string  { return filepath.Join(LogDirectory, LogFileName) }

// EnsureLogFifos creates the log directory and the two named pipes the
// logging collector will open, owned by the postgres user.
func EnsureLogFifos() error {
	if err := os.MkdirAll(LogDirectory, 0755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := os.Chown(LogDirectory, postgresUID, postgresGID); err != nil {
		return fmt.Errorf("own log directory: %w", err)
	}
	for _, path := range []string{jsonLogPath(), rawLogPath()} {
		if err := ensureFifo(path); err != nil {
			return err
		}
	}
	return nil
}

// ensureFifo makes path a named pipe owned by the postgres user, replacing any
// regular file left behind at that path.
func ensureFifo(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeNamedPipe == 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace %s with a pipe: %w", path, err)
		}
		err = os.ErrNotExist
	}
	if err != nil {
		if err := syscall.Mkfifo(path, 0600); err != nil {
			return fmt.Errorf("create log pipe %s: %w", path, err)
		}
	}
	if err := os.Chown(path, postgresUID, postgresGID); err != nil {
		return fmt.Errorf("own log pipe %s: %w", path, err)
	}
	return nil
}

// StartLogTailers relays collector output from the log pipes to stdout for the
// remaining lifetime of the process. The pipes are opened read-write so the
// readers survive collector restarts without ever seeing EOF.
func StartLogTailers() error {
	jsonFile, err := os.OpenFile(jsonLogPath(), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open jsonlog pipe: %w", err)
	}
	rawFile, err := os.OpenFile(rawLogPath(), os.O_RDWR, 0)
	if err != nil {
		jsonFile.Close()
		return fmt.Errorf("open stderr log pipe: %w", err)
	}
	go tailJSONLog(jsonFile)
	go tailRawLog(rawFile)
	return nil
}

const maxLogLineLength = 1 << 20

func tailJSONLog(file *os.File) {
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, truncated, err := readLine(reader)
		if len(bytes.TrimSpace(line)) > 0 {
			if remapped := RemapPostgresJSON(line); remapped != nil && !truncated {
				_, _ = os.Stdout.Write(remapped)
			} else {
				emitLine(line)
			}
		}
		if err != nil {
			return
		}
	}
}

func tailRawLog(file *os.File) {
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, _, err := readLine(reader)
		emitLine(line)
		if err != nil {
			return
		}
	}
}

func readLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	truncated := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if remaining := maxLogLineLength - len(line); len(chunk) > remaining {
			chunk = chunk[:remaining]
			truncated = true
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSuffix(line, []byte("\n")), truncated, err
	}
}

// postgresLogTime is the fixed format jsonlog uses for its timestamp field,
// with the zone name pinned to UTC by the forced log_timezone.
const postgresLogTime = "2006-01-02 15:04:05.000 MST"

// RemapPostgresJSON rewrites one PostgreSQL jsonlog line onto the standard
// keys the supervisor logs with: time (RFC3339), level, and msg. Only the keys
// are renamed — level carries the PostgreSQL severity verbatim, and is left
// out like any other absent field — while every other field passes through
// unchanged and flat. It returns nil when the line is not a jsonlog record.
func RemapPostgresJSON(line []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields["timestamp"] == nil {
		return nil
	}

	buf := make([]byte, 0, len(line)+32)
	buf = append(buf, `{"time":`...)
	buf = append(buf, remapTimestamp(fields["timestamp"])...)
	delete(fields, "timestamp")

	if level := fields["error_severity"]; level != nil {
		delete(fields, "error_severity")
		buf = append(buf, `,"level":`...)
		buf = append(buf, level...)
	}

	if message := fields["message"]; message != nil {
		delete(fields, "message")
		buf = append(buf, `,"msg":`...)
		buf = append(buf, message...)
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		buf = append(buf, ',')
		buf = appendJSONString(buf, key)
		buf = append(buf, ':')
		buf = append(buf, fields[key]...)
	}
	return append(buf, '}', '\n')
}

func remapTimestamp(raw json.RawMessage) []byte {
	value := stringField(raw)
	parsed, err := time.Parse(postgresLogTime, value)
	if err != nil {
		return raw
	}
	quoted, _ := json.Marshal(parsed.Format(time.RFC3339Nano))
	return quoted
}

func stringField(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func appendJSONString(buf []byte, value string) []byte {
	quoted, _ := json.Marshal(value)
	return append(buf, quoted...)
}
