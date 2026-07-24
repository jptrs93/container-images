package config

import (
	"strings"
	"testing"
)

func TestWritePostgresFilesRejectsSupervisorConnectionSettings(t *testing.T) {
	for _, setting := range []string{"port", "Port", "unix_socket_directories", "listen_addresses", "archive_library"} {
		t.Run(setting, func(t *testing.T) {
			err := WritePostgresFiles(Config{Settings: map[string]MaybeEnv{setting: "5433"}}, RuntimeOptions{
				PGData:          t.TempDir(),
				ConfigDirectory: t.TempDir(),
			})
			if err == nil || !strings.Contains(err.Error(), "managed by the supervisor") {
				t.Fatalf("WritePostgresFiles() error = %v, want managed-setting error", err)
			}
		})
	}
}
