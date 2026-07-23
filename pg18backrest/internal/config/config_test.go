package config

import (
	"os"
	"strings"
	"testing"
)

func TestPGBackRestResolveUsesConfiguredSecretKeys(t *testing.T) {
	options, err := PGBackRest{
		Stanza: "app-prod",
		S3: S3{
			Host:      "s3.example.internal",
			Bucket:    "postgres-backups",
			AccessKey: "access-key",
			SecretKey: "secret-key",
		},
	}.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if options.S3AccessKey != "access-key" || options.S3SecretKey != "secret-key" || options.S3Port != 443 {
		t.Fatalf("resolved credentials and port = %q, %q, %d", options.S3AccessKey, options.S3SecretKey, options.S3Port)
	}
	if options.DiffSchedule != "0 2 * * 1-6" || options.Timezone != "UTC" {
		t.Fatalf("defaults = schedule %q, timezone %q", options.DiffSchedule, options.Timezone)
	}
}

func TestSecretValueResolvesLiteral(t *testing.T) {
	value, err := SecretValue("literal-value").Resolve("test.secret", true)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if value != "literal-value" {
		t.Fatalf("Resolve() value = %q", value)
	}
}

func TestLoadResolvesStringEnvironmentReferencesOnce(t *testing.T) {
	t.Setenv("TEST_PASSWORD", "password-from-environment")
	t.Setenv("TEST_HBA", "host all all 10.0.0.0/8 scram-sha-256")
	t.Setenv("TEST_ROLE", "app_user")
	t.Setenv("TEST_HOST", "s3.example.internal")
	t.Setenv("TEST_PORT", "8443")
	t.Setenv("TEST_NESTED", "${TEST_PASSWORD}")
	path := t.TempDir() + "/config.yaml"
	contents := "initdb:\n  postgres_password: ${TEST_PASSWORD}\nhba:\n  - ${TEST_HBA}\nroles:\n  - name:\n      value: ${TEST_ROLE}\npgbackrest:\n  enabled: true\n  stanza: app\n  s3:\n    host: ${TEST_HOST}\n    port: ${TEST_PORT}\n    bucket: backups\n    access_key: key\n    secret_key: secret\nsettings:\n  application_name: ${TEST_NESTED}\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.InitDB.PostgresPassword != "password-from-environment" || cfg.HBA[0] != "host all all 10.0.0.0/8 scram-sha-256" || cfg.Roles[0].Name.Value != "app_user" || cfg.PGBackRest.S3.Host != "s3.example.internal" || cfg.PGBackRest.S3.Port != "8443" {
		t.Fatalf("resolved config = %#v", cfg)
	}
	if cfg.Settings["application_name"] != "${TEST_PASSWORD}" {
		t.Fatalf("application_name = %q, want unexpanded environment value", cfg.Settings["application_name"])
	}
	if strings.Join(cfg.EnvironmentReferences(), ",") != "TEST_HBA,TEST_HOST,TEST_NESTED,TEST_PASSWORD,TEST_PORT,TEST_ROLE" {
		t.Fatalf("environment references = %q", cfg.EnvironmentReferences())
	}
}

func TestLoadRejectsSecretSourceObject(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("initdb:\n  postgres_password:\n    env_file_path_key: POSTGRES_PASSWORD_FILE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want scalar secret error")
	}
}

func TestInitDBResolveUsesBootstrapUserAsDatabaseDefault(t *testing.T) {
	options, err := InitDB{
		PostgresUser:     ValueSource{Value: "bootstrap_admin"},
		PostgresPassword: "postgres-password",
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
	options, err := InitDB{
		PostgresUser:     ValueSource{Env: "POSTGRES_USER"},
		PostgresPassword: "postgres-password",
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
