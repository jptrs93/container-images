package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jptrs93/goutil/logu"
)

func captureRelay(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := relay
	buffer := &bytes.Buffer{}
	relay = logu.NewJSONLogger(buffer, slog.LevelDebug)
	t.Cleanup(func() { relay = previous })
	return buffer
}

func TestRemapPostgresJSONStandardisesFields(t *testing.T) {
	line := `{"timestamp":"2026-08-23 04:05:06.789 UTC","user":"app_user","dbname":"app","pid":123,` +
		`"error_severity":"ERROR","state_code":"42P01","message":"relation \"missing\" does not exist",` +
		`"statement":"select *\nfrom missing","backend_type":"client backend","query_id":0}`
	remapped := RemapPostgresJSON([]byte(line))
	if remapped == nil {
		t.Fatal("RemapPostgresJSON() = nil")
	}
	if !bytes.HasSuffix(remapped, []byte("}\n")) || !bytes.HasPrefix(remapped, []byte(`{"time":`)) {
		t.Fatalf("remapped line framing = %q", remapped)
	}
	var record map[string]any
	if err := json.Unmarshal(remapped, &record); err != nil {
		t.Fatalf("remapped line is not JSON: %v", err)
	}
	if record["time"] != "2026-08-23T04:05:06.789Z" {
		t.Fatalf("time = %v", record["time"])
	}
	if record["level"] != "ERROR" {
		t.Fatalf("level = %v", record["level"])
	}
	if record["msg"] != `relation "missing" does not exist` {
		t.Fatalf("msg = %v", record["msg"])
	}
	if record["statement"] != "select *\nfrom missing" || record["pid"] != float64(123) || record["backend_type"] != "client backend" {
		t.Fatalf("passthrough fields = %v", record)
	}
	for _, key := range []string{"timestamp", "error_severity", "message"} {
		if _, exists := record[key]; exists {
			t.Fatalf("original key %q still present", key)
		}
	}
}

func TestRemapPostgresJSONKeepsSeverityValuesVerbatim(t *testing.T) {
	for _, severity := range []string{"DEBUG3", "LOG", "INFO", "NOTICE", "WARNING", "ERROR", "FATAL", "PANIC"} {
		t.Run(severity, func(t *testing.T) {
			line := `{"timestamp":"2026-08-23 04:05:06.789 UTC","error_severity":"` + severity + `","message":"m"}`
			var record map[string]any
			if err := json.Unmarshal(RemapPostgresJSON([]byte(line)), &record); err != nil {
				t.Fatal(err)
			}
			if record["level"] != severity {
				t.Fatalf("level = %v, want %s", record["level"], severity)
			}
			if _, exists := record["severity"]; exists {
				t.Fatalf("unexpected severity key in %v", record)
			}
		})
	}
}

func TestRemapPostgresJSONOmitsAbsentFields(t *testing.T) {
	line := `{"timestamp":"2026-08-23 04:05:06.789 UTC","pid":7}`
	var record map[string]any
	if err := json.Unmarshal(RemapPostgresJSON([]byte(line)), &record); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"level", "msg"} {
		if _, exists := record[key]; exists {
			t.Fatalf("key %q fabricated: %v", key, record)
		}
	}
	if record["time"] != "2026-08-23T04:05:06.789Z" || record["pid"] != float64(7) {
		t.Fatalf("record = %v", record)
	}
}

func TestRemapPostgresJSONKeepsUnparseableTimestamp(t *testing.T) {
	line := `{"timestamp":"not a time","error_severity":"LOG","message":"m"}`
	var record map[string]any
	if err := json.Unmarshal(RemapPostgresJSON([]byte(line)), &record); err != nil {
		t.Fatal(err)
	}
	if record["time"] != "not a time" {
		t.Fatalf("time = %v", record["time"])
	}
}

func TestRemapPostgresJSONRejectsNonRecords(t *testing.T) {
	for name, line := range map[string]string{
		"plain text":        "FATAL:  the database system is shutting down",
		"json without time": `{"message":"m"}`,
		"json array":        `["timestamp"]`,
	} {
		t.Run(name, func(t *testing.T) {
			if remapped := RemapPostgresJSON([]byte(line)); remapped != nil {
				t.Fatalf("RemapPostgresJSON() = %q, want nil", remapped)
			}
		})
	}
}

func TestDetectLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"2026-08-23 04:05:06.789 UTC [1] LOG:  starting PostgreSQL": slog.LevelInfo,
		"FATAL:  role \"missing\" does not exist":                   slog.LevelError,
		"P00   WARN: unable to check pg1":                           slog.LevelWarn,
		"P00   INFO: archive-push command begin":                    slog.LevelInfo,
		"ERROR: [049]: unable to get info":                          slog.LevelError,
		"DETAIL:  key (id)=(1) already exists.":                     slog.LevelDebug,
		"The files belonging to this database system":               slog.LevelInfo,
	}
	for line, level := range tests {
		if detected := DetectLevel([]byte(line)); detected != level {
			t.Fatalf("DetectLevel(%q) = %v, want %v", line, detected, level)
		}
	}
}

func TestLineWriterEmitsRecordsPerLine(t *testing.T) {
	buffer := captureRelay(t)
	writer := NewLineWriter()
	if _, err := writer.Write([]byte("P00   INFO: check comm")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("and begin\r\nERROR: [049]: failed\npartial")); err != nil {
		t.Fatal(err)
	}
	writer.Flush()

	lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("record count = %d: %q", len(lines), buffer.String())
	}
	expected := []struct{ level, msg string }{
		{"INFO", "P00   INFO: check command begin"},
		{"ERROR", "ERROR: [049]: failed"},
		{"INFO", "partial"},
	}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record %d is not JSON: %v", i, err)
		}
		if record["level"] != expected[i].level || record["msg"] != expected[i].msg {
			t.Fatalf("record %d = %v, want %+v", i, record, expected[i])
		}
		if _, err := time.Parse(time.RFC3339Nano, record["time"].(string)); err != nil {
			t.Fatalf("record %d time: %v", i, err)
		}
	}
}

func TestLineWriterSkipsBlankLines(t *testing.T) {
	buffer := captureRelay(t)
	writer := NewLineWriter()
	if _, err := writer.Write([]byte("\n   \r\n\n")); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	if buffer.Len() != 0 {
		t.Fatalf("blank input produced records: %q", buffer.String())
	}
}
