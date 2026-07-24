package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jptrs93/container-images/declaritive-postgres/internal/config"
)

func TestPostgresEnvInjectsInitDBValuesWithoutConflictingFileVariables(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD_FILE", "/run/secrets/postgres-password")
	t.Setenv("POSTGRES_INITDB_ARGS", "--data-checksums")
	t.Setenv("PGPORT", "5433")

	values := environmentMap(postgresEnv(config.Config{}, &config.InitDBOptions{
		PostgresUser:     "admin",
		PostgresPassword: "password",
		PostgresDB:       "app",
	}))
	if values["POSTGRES_USER"] != "admin" || values["POSTGRES_PASSWORD"] != "password" || values["POSTGRES_DB"] != "app" {
		t.Fatalf("injected initdb environment = %#v", values)
	}
	if _, exists := values["POSTGRES_PASSWORD_FILE"]; exists {
		t.Fatal("POSTGRES_PASSWORD_FILE was passed with POSTGRES_PASSWORD")
	}
	if values["POSTGRES_INITDB_ARGS"] != "--data-checksums" {
		t.Fatal("POSTGRES_INITDB_ARGS was not preserved")
	}
	if _, exists := values["PGPORT"]; exists {
		t.Fatal("PGPORT was passed to the YAML-configured PostgreSQL child")
	}
}

func TestPostgresEnvPreservesReferencedOfficialBootstrapVariable(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "password")
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("roles:\n  - name: app\n    password: ${POSTGRES_PASSWORD}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if values := environmentMap(postgresEnv(cfg, nil)); values["POSTGRES_PASSWORD"] != "password" {
		t.Fatal("referenced POSTGRES_PASSWORD was removed from the PostgreSQL child environment")
	}
}

func TestPostgresChildEnvUsesConfiguredSocketAndPort(t *testing.T) {
	t.Setenv("PGHOST", "/external/socket")
	t.Setenv("PGPORT", "5444")
	values := environmentMap(postgresChildEnv(config.Config{}, nil, config.ConnectionOptions{
		SocketDirectory: "/configured/socket",
		Port:            "5433",
	}))
	if values["PGHOST"] != "/configured/socket" || values["PGPORT"] != "5433" || values["PGSERVICE"] != "postgres-supervisor" || values["PGSERVICEFILE"] != "/etc/postgres-supervisor/pg_service.conf" {
		t.Fatalf("PostgreSQL child connection environment = %#v", values)
	}
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, content, _ := strings.Cut(value, "=")
		result[name] = content
	}
	return result
}
