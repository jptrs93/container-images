package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type RuntimeOptions struct {
	PGData          string
	ConfigDirectory string
}

type ConnectionOptions struct {
	ListenAddresses   string
	SocketDirectories string
	SocketDirectory   string
	Port              string
}

var settingName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

func WritePostgresFiles(cfg Config, options RuntimeOptions) error {
	if err := validatePostgresSettings(cfg.Settings); err != nil {
		return err
	}
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
		"data_directory = " + quote(options.PGData),
		"hba_file = " + quote(hbaPath),
		"ident_file = " + quote(identPath),
		"logging_collector = off",
		"log_destination = 'stderr'",
	}

	keys := make([]string, 0, len(cfg.Settings))
	for key := range cfg.Settings {
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

	if err := os.WriteFile(postgresPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return fmt.Errorf("write postgresql.conf: %w", err)
	}
	connection := cfg.ConnectionOptions()
	service := "[postgres-supervisor]\nhost=" + connection.SocketDirectory + "\nport=" + connection.Port + "\n"
	if err := os.WriteFile(filepath.Join(options.ConfigDirectory, "pg_service.conf"), []byte(service), 0644); err != nil {
		return fmt.Errorf("write pg_service.conf: %w", err)
	}
	return nil
}

func validatePostgresSettings(settings map[string]MaybeEnv) error {
	seen := make(map[string]string, len(settings))
	for key := range settings {
		if !settingName.MatchString(key) {
			return fmt.Errorf("invalid PostgreSQL setting name %q", key)
		}
		normalized := strings.ToLower(key)
		if previous, exists := seen[normalized]; exists {
			return fmt.Errorf("PostgreSQL settings %q and %q differ only by case", previous, key)
		}
		seen[normalized] = key
		switch normalized {
		case "data_directory", "hba_file", "ident_file", "config_file", "logging_collector", "log_destination":
			return fmt.Errorf("PostgreSQL setting %q is managed by the supervisor", key)
		}
	}
	port, portConfigured := postgresSetting(settings, "port")
	if portConfigured {
		value, err := strconv.Atoi(string(port))
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("PostgreSQL setting %q must be an integer between 1 and 65535", "port")
		}
	}
	if socketDirectories, socketConfigured := postgresSetting(settings, "unix_socket_directories"); socketConfigured {
		directory := firstSocketDirectory(socketDirectories)
		if directory == "" {
			return fmt.Errorf("PostgreSQL setting %q must include a socket directory", "unix_socket_directories")
		}
		if strings.ContainsAny(string(socketDirectories), "\r\n") {
			return fmt.Errorf("PostgreSQL setting %q socket directories must each be a single line", "unix_socket_directories")
		}
	}
	return nil
}

func (cfg *Config) insertDefaultsIfMissing() {
	if cfg.Settings == nil {
		cfg.Settings = make(map[string]MaybeEnv)
	}
	for key, value := range map[string]MaybeEnv{
		"listen_addresses":        "*",
		"port":                    "5432",
		"unix_socket_directories": "/var/run/postgresql",
	} {
		if _, exists := postgresSetting(cfg.Settings, key); !exists {
			cfg.Settings[key] = value
		}
	}
}

func (cfg Config) ConnectionOptions() ConnectionOptions {
	listenAddresses, _ := postgresSetting(cfg.Settings, "listen_addresses")
	socketDirectories, _ := postgresSetting(cfg.Settings, "unix_socket_directories")
	port, _ := postgresSetting(cfg.Settings, "port")
	return ConnectionOptions{
		ListenAddresses:   string(listenAddresses),
		SocketDirectories: string(socketDirectories),
		SocketDirectory:   firstSocketDirectory(socketDirectories),
		Port:              string(port),
	}
}

func postgresSetting(settings map[string]MaybeEnv, name string) (MaybeEnv, bool) {
	for key, value := range settings {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

func firstSocketDirectory(value MaybeEnv) string {
	for _, directory := range strings.Split(string(value), ",") {
		if directory = strings.TrimSpace(directory); directory != "" {
			return directory
		}
	}
	return ""
}

func settingValue(value MaybeEnv) (string, error) {
	return quote(string(value)), nil
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
