package config

import (
	"os"
	"strings"
	"testing"
)

func TestPGBackRestResolveUsesConfiguredSecretKeys(t *testing.T) {
	t.Setenv("TEST_S3_ACCESS_KEY", "access-key")
	t.Setenv("TEST_S3_SECRET_KEY", "secret-key")
	t.Setenv("PGBACKREST_S3_KEY_FILE", "/not-used-by-custom-source")
	options, err := PGBackRest{
		Stanza: "app-prod",
		S3: S3{
			Endpoint: "https://s3.example.internal",
			Bucket:   "postgres-backups",
			AccessKey: SecretSource{
				EnvValueKey: "TEST_S3_ACCESS_KEY",
			},
			SecretKey: SecretSource{
				EnvValueKey: "TEST_S3_SECRET_KEY",
			},
		},
	}.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if options.S3AccessKey != "access-key" || options.S3SecretKey != "secret-key" {
		t.Fatalf("resolved credentials = %q, %q", options.S3AccessKey, options.S3SecretKey)
	}
	if options.DiffSchedule != "0 2 * * 1-6" || options.Timezone != "UTC" {
		t.Fatalf("defaults = schedule %q, timezone %q", options.DiffSchedule, options.Timezone)
	}
}

func TestSecretSourceRejectsValueAndFilePath(t *testing.T) {
	t.Setenv("TEST_VALUE", "value")
	t.Setenv("TEST_FILE", "/tmp/secret")
	_, err := SecretSource{
		EnvValueKey:    "TEST_VALUE",
		EnvFilePathKey: "TEST_FILE",
	}.Resolve("test.secret", "DEFAULT_VALUE", "DEFAULT_FILE", true)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Resolve() error = %v, want exactly-one-source error", err)
	}
}

func TestSecretSourceReadsConfiguredFilePath(t *testing.T) {
	path := t.TempDir() + "/secret"
	if err := os.WriteFile(path, []byte("secret-from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET_FILE", path)
	value, err := SecretSource{EnvFilePathKey: "TEST_SECRET_FILE"}.Resolve("test.secret", "DEFAULT_VALUE", "DEFAULT_FILE", true)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if value != "secret-from-file" {
		t.Fatalf("Resolve() value = %q", value)
	}
}

func TestInitDBResolveUsesBootstrapUserAsDatabaseDefault(t *testing.T) {
	t.Setenv("TEST_POSTGRES_PASSWORD", "postgres-password")
	options, err := InitDB{
		PostgresUser: ValueSource{Value: "bootstrap_admin"},
		PostgresPassword: SecretSource{
			EnvValueKey: "TEST_POSTGRES_PASSWORD",
		},
	}.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if options.PostgresUser != "bootstrap_admin" || options.PostgresDB != "bootstrap_admin" {
		t.Fatalf("resolved user and database = %q, %q", options.PostgresUser, options.PostgresDB)
	}
}

func TestInitDBResolveUsesOfficialUserAndDatabaseDefaults(t *testing.T) {
	t.Setenv("POSTGRES_USER", "")
	t.Setenv("POSTGRES_DB", "")
	t.Setenv("POSTGRES_PASSWORD", "postgres-password")
	options, err := InitDB{
		PostgresUser:     ValueSource{Env: "POSTGRES_USER"},
		PostgresPassword: SecretSource{EnvValueKey: "POSTGRES_PASSWORD"},
		PostgresDB:       ValueSource{Env: "POSTGRES_DB"},
	}.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if options.PostgresUser != "postgres" || options.PostgresDB != "postgres" {
		t.Fatalf("resolved user and database = %q, %q", options.PostgresUser, options.PostgresDB)
	}
}

func TestValidateRejectsGrantsForSchemaOwner(t *testing.T) {
	cfg := Config{
		Roles: []Role{{
			Name: ValueSource{Value: "app_owner"},
			Permissions: []Permission{{
				Database: "app",
				Schema:   "app",
				Grants:   []string{"USAGE"},
			}},
		}},
		Databases: []Database{{
			Name:    "app",
			Owner:   "app_owner",
			Schemas: []string{"app"},
		}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "owned by role") {
		t.Fatalf("Validate() error = %v, want schema-owner grant error", err)
	}
}
