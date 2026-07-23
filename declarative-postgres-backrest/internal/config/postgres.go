package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type RuntimeOptions struct {
	PGData          string
	ConfigDirectory string
	ArchiveCommand  string
	ArchiveTimeout  string
}

var settingName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

func WritePostgresFiles(cfg Config, options RuntimeOptions) error {
	if err := os.MkdirAll(options.ConfigDirectory, 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	hbaPath := filepath.Join(options.ConfigDirectory, "pg_hba.conf")
	identPath := filepath.Join(options.ConfigDirectory, "pg_ident.conf")
	postgresPath := filepath.Join(options.ConfigDirectory, "postgresql.conf")

	hba := []string{"local all all trust"}
	if len(cfg.HBA) == 0 {
		hba = append(hba, "host all all all scram-sha-256")
	} else {
		for _, record := range cfg.HBA {
			hba = append(hba, string(record))
		}
	}
	if err := os.WriteFile(hbaPath, []byte(strings.Join(hba, "\n")+"\n"), 0644); err != nil {
		return fmt.Errorf("write pg_hba.conf: %w", err)
	}
	if err := os.WriteFile(identPath, []byte{}, 0644); err != nil {
		return fmt.Errorf("write pg_ident.conf: %w", err)
	}

	lines := []string{
		"listen_addresses = '*'",
		"port = 5432",
		"unix_socket_directories = '/var/run/postgresql'",
		"data_directory = " + quote(options.PGData),
		"hba_file = " + quote(hbaPath),
		"ident_file = " + quote(identPath),
		"logging_collector = off",
		"log_destination = 'stderr'",
	}

	keys := make([]string, 0, len(cfg.Settings))
	for key := range cfg.Settings {
		if !settingName.MatchString(key) {
			return fmt.Errorf("invalid PostgreSQL setting name %q", key)
		}
		switch key {
		case "data_directory", "hba_file", "ident_file", "config_file", "logging_collector", "log_destination":
			return fmt.Errorf("PostgreSQL setting %q is managed by the supervisor", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := settingValue(cfg.Settings[key])
		if err != nil {
			return fmt.Errorf("setting %s: %w", key, err)
		}
		lines = append(lines, key+" = "+value)
	}

	if options.ArchiveCommand != "" {
		lines = append(lines,
			"archive_mode = on",
			"archive_command = "+quote(options.ArchiveCommand),
			"archive_timeout = "+options.ArchiveTimeout,
		)
	}
	if err := os.WriteFile(postgresPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return fmt.Errorf("write postgresql.conf: %w", err)
	}
	return nil
}

func settingValue(value MaybeEnv) (string, error) {
	return quote(string(value)), nil
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
