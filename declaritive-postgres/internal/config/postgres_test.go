package config

import (
	"os"
	"strings"
	"testing"
)

func TestWritePostgresFilesUsesConnectionDefaultsAndOverrides(t *testing.T) {
	configDirectory := t.TempDir()
	cfg := Config{Settings: map[string]MaybeEnv{
		"listen_addresses":        "localhost",
		"Port":                    "5433",
		"unix_socket_directories": "/tmp/postgres",
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := WritePostgresFiles(cfg, RuntimeOptions{PGData: t.TempDir(), ConfigDirectory: configDirectory}); err != nil {
		t.Fatalf("WritePostgresFiles() error = %v", err)
	}
	contents, err := os.ReadFile(configDirectory + "/postgresql.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"listen_addresses = 'localhost'", "Port = '5433'", "unix_socket_directories = '/tmp/postgres'"} {
		t.Run(setting, func(t *testing.T) {
			if !strings.Contains(string(contents), setting) {
				t.Fatalf("postgresql.conf does not contain %q", setting)
			}
		})
	}
	if cfg.Settings["listen_addresses"] != "localhost" || cfg.Settings["Port"] != "5433" || cfg.Settings["unix_socket_directories"] != "/tmp/postgres" {
		t.Fatalf("configured connection settings changed: %#v", cfg.Settings)
	}
	connection := cfg.ConnectionOptions()
	if connection.ListenAddresses != "localhost" || connection.Port != "5433" || connection.SocketDirectories != "/tmp/postgres" || connection.SocketDirectory != "/tmp/postgres" {
		t.Fatalf("connection options = %#v", connection)
	}
	service, err := os.ReadFile(configDirectory + "/pg_service.conf")
	if err != nil {
		t.Fatal(err)
	}
	if string(service) != "[postgres-supervisor]\nhost=/tmp/postgres\nport=5433\n" {
		t.Fatalf("pg_service.conf = %q", service)
	}
}

func TestValidateAddsConnectionDefaults(t *testing.T) {
	cfg := Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Settings["listen_addresses"] != "*" || cfg.Settings["port"] != "5432" || cfg.Settings["unix_socket_directories"] != "/var/run/postgresql" {
		t.Fatalf("connection defaults = %#v", cfg.Settings)
	}
	connection := cfg.ConnectionOptions()
	if connection.ListenAddresses != "*" || connection.Port != "5432" || connection.SocketDirectories != "/var/run/postgresql" || connection.SocketDirectory != "/var/run/postgresql" {
		t.Fatalf("connection options = %#v", connection)
	}
}

func TestValidateRejectsInvalidConnectionSettings(t *testing.T) {
	tests := map[string]struct {
		setting string
		value   MaybeEnv
		message string
	}{
		"empty socket directory":     {"unix_socket_directories", "", "must include a socket directory"},
		"multiline socket directory": {"unix_socket_directories", "/tmp\ninclude=/tmp/other", "must each be a single line"},
		"non-numeric port":           {"port", "postgres", "must be an integer between 1 and 65535"},
		"port below range":           {"port", "0", "must be an integer between 1 and 65535"},
		"port above range":           {"port", "65536", "must be an integer between 1 and 65535"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Settings: map[string]MaybeEnv{test.setting: test.value}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Validate() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestValidateRejectsSupervisorManagedSettings(t *testing.T) {
	for _, setting := range []string{"data_directory", "hba_file", "ident_file", "config_file", "logging_collector", "log_destination"} {
		t.Run(setting, func(t *testing.T) {
			cfg := Config{Settings: map[string]MaybeEnv{setting: "value"}}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "managed by the supervisor") {
				t.Fatalf("Validate() error = %v, want managed-setting error", err)
			}
		})
	}
}
