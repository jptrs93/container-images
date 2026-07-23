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

func TestMaybeEnvResolvesLiteral(t *testing.T) {
	value, err := MaybeEnv("literal-value").Resolve()
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
	contents := "initdb:\n  postgres_password: ${TEST_PASSWORD}\nhba:\n  - ${TEST_HBA}\nroles:\n  - name: ${TEST_ROLE}\npgbackrest:\n  enabled: true\n  stanza: app\n  s3:\n    host: ${TEST_HOST}\n    port: ${TEST_PORT}\n    bucket: backups\n    access_key: key\n    secret_key: secret\nsettings:\n  application_name: ${TEST_NESTED}\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.InitDB.PostgresPassword != "password-from-environment" || cfg.HBA[0] != "host all all 10.0.0.0/8 scram-sha-256" || cfg.Roles[0].Name != "app_user" || cfg.PGBackRest.S3.Host != "s3.example.internal" || cfg.PGBackRest.S3.Port != "8443" {
		t.Fatalf("resolved config = %#v", cfg)
	}
	if cfg.Settings["application_name"] != "${TEST_PASSWORD}" {
		t.Fatalf("application_name = %q, want unexpanded environment value", cfg.Settings["application_name"])
	}
	if strings.Join(cfg.EnvironmentReferences(), ",") != "TEST_HBA,TEST_HOST,TEST_NESTED,TEST_PASSWORD,TEST_PORT,TEST_ROLE" {
		t.Fatalf("environment references = %q", cfg.EnvironmentReferences())
	}
}

func TestLoadResolvesEveryMaybeEnvField(t *testing.T) {
	t.Setenv("TEST_VALUE", "resolved")
	path := t.TempDir() + "/config.yaml"
	contents := "settings:\n  application_name: ${TEST_VALUE}\nhba:\n  - ${TEST_VALUE}\ninitdb:\n  postgres_user: ${TEST_VALUE}\n  postgres_password: ${TEST_VALUE}\n  postgres_db: ${TEST_VALUE}\nroles:\n  - name: ${TEST_VALUE}\n    password: ${TEST_VALUE}\n    permissions:\n      - database: ${TEST_VALUE}\n        schema: ${TEST_VALUE}\n        grants: [USAGE]\ndatabases:\n  - name: ${TEST_VALUE}\n    owner: ${TEST_VALUE}\n    schemas:\n      - ${TEST_VALUE}\n    extensions:\n      - ${TEST_VALUE}\npgbackrest:\n  enabled: true\n  stanza: ${TEST_VALUE}\n  repository_cipher_pass: ${TEST_VALUE}\n  s3:\n    host: ${TEST_VALUE}\n    port: ${TEST_VALUE}\n    bucket: ${TEST_VALUE}\n    region: ${TEST_VALUE}\n    uri_style: ${TEST_VALUE}\n    access_key: ${TEST_VALUE}\n    secret_key: ${TEST_VALUE}\n  schedules:\n    full: ${TEST_VALUE}\n    differential: ${TEST_VALUE}\n    check: ${TEST_VALUE}\n  archive:\n    push_queue_max: ${TEST_VALUE}\n  timezone: ${TEST_VALUE}\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	values := []MaybeEnv{
		cfg.Settings["application_name"], cfg.HBA[0], cfg.InitDB.PostgresUser, cfg.InitDB.PostgresPassword, cfg.InitDB.PostgresDB,
		cfg.Roles[0].Name, cfg.Roles[0].Password, cfg.Roles[0].Permissions[0].Database, cfg.Roles[0].Permissions[0].Schema,
		cfg.Databases[0].Name, cfg.Databases[0].Owner, cfg.Databases[0].Schemas[0], cfg.Databases[0].Extensions[0],
		cfg.PGBackRest.Stanza, cfg.PGBackRest.RepositoryCipherPass, cfg.PGBackRest.S3.Host, cfg.PGBackRest.S3.Port,
		cfg.PGBackRest.S3.Bucket, cfg.PGBackRest.S3.Region, cfg.PGBackRest.S3.URIStyle, cfg.PGBackRest.S3.AccessKey,
		cfg.PGBackRest.S3.SecretKey, cfg.PGBackRest.Schedules.Full, cfg.PGBackRest.Schedules.Differential,
		cfg.PGBackRest.Schedules.Check, cfg.PGBackRest.Archive.PushQueueMax, cfg.PGBackRest.Timezone,
	}
	for _, value := range values {
		if value != "resolved" {
			t.Fatalf("resolved value = %q, want resolved", value)
		}
	}
	if strings.Join(cfg.EnvironmentReferences(), ",") != "TEST_VALUE" {
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
		PostgresUser:     "bootstrap_admin",
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
		PostgresPassword: "postgres-password",
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
			Name: "app_owner",
			Permissions: []Permission{{
				Database: "app",
				Schema:   "app",
				Grants:   []string{"USAGE"},
			}},
		}},
		Databases: []Database{{
			Name:    "app",
			Owner:   "app_owner",
			Schemas: []MaybeEnv{"app"},
		}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "owned by role") {
		t.Fatalf("Validate() error = %v, want schema-owner grant error", err)
	}
}
