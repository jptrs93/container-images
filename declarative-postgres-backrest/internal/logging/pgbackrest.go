package logging

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// PGBackRestLogDirectory is where pgBackRest file logging is pointed; it holds
// only named pipes, so nothing is persisted to disk.
const PGBackRestLogDirectory = "/var/log/pgbackrest"

// The archive processes are the one part of pgBackRest whose output cannot be
// captured from a stream: the foreground archive-push/archive-get run under
// PostgreSQL with their console silenced, and the async processes they spawn
// detach entirely. Both roles log through files instead, named
// <stanza>-<command>.log and <stanza>-<command>-async.log.
var pgBackRestLogNames = []string{
	"archive-push", "archive-push-async",
	"archive-get", "archive-get-async",
}

// StartPGBackRestLogRelay creates the archive log pipes for the stanza and
// relays them to stdout for the remaining lifetime of the process. Like the
// collector pipes, they are opened read-write so the readers never see EOF.
func StartPGBackRestLogRelay(stanza string) error {
	if err := os.MkdirAll(PGBackRestLogDirectory, 0750); err != nil {
		return fmt.Errorf("create pgBackRest log directory: %w", err)
	}
	if err := os.Chown(PGBackRestLogDirectory, postgresUID, postgresGID); err != nil {
		return fmt.Errorf("own pgBackRest log directory: %w", err)
	}
	for _, name := range pgBackRestLogNames {
		path := filepath.Join(PGBackRestLogDirectory, stanza+"-"+name+".log")
		if err := ensureFifo(path); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open pgBackRest log pipe %s: %w", path, err)
		}
		go tailPGBackRestLog(file)
	}
	return nil
}

func tailPGBackRestLog(file *os.File) {
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, _, err := readLine(reader)
		if !isProcessStartBanner(line) {
			emitLine(line)
		}
		if err != nil {
			return
		}
	}
}

// isProcessStartBanner reports whether the line is the dashed banner pgBackRest
// writes at the top of every file-logged process.
func isProcessStartBanner(line []byte) bool {
	trimmed := bytes.Trim(line, "-")
	return len(trimmed) != len(line) && string(trimmed) == "PROCESS START"
}
