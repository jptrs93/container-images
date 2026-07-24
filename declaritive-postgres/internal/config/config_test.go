package config

import (
	"os"
	"strings"
	"testing"
)

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
	t.Setenv("TEST_NESTED", "${TEST_PASSWORD}")
	path := t.TempDir() + "/config.yaml"
	contents := "initdb:\n  postgres_password: ${TEST_PASSWORD}\nhba:\n  - ${TEST_HBA}\nroles:\n  - name: ${TEST_ROLE}\nsettings:\n  application_name: ${TEST_NESTED}\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.InitDB.PostgresPassword != "password-from-environment" || cfg.HBA[0] != "host all all 10.0.0.0/8 scram-sha-256" || cfg.Roles[0].Name != "app_user" {
		t.Fatalf("resolved config = %#v", cfg)
	}
	if cfg.Settings["application_name"] != "${TEST_PASSWORD}" {
		t.Fatalf("application_name = %q, want unexpanded environment value", cfg.Settings["application_name"])
	}
	if strings.Join(cfg.EnvironmentReferences(), ",") != "TEST_HBA,TEST_NESTED,TEST_PASSWORD,TEST_ROLE" {
		t.Fatalf("environment references = %q", cfg.EnvironmentReferences())
	}
}

func TestLoadResolvesEveryMaybeEnvField(t *testing.T) {
	t.Setenv("TEST_VALUE", "resolved")
	path := t.TempDir() + "/config.yaml"
	contents := "settings:\n  application_name: ${TEST_VALUE}\nhba:\n  - ${TEST_VALUE}\ninitdb:\n  postgres_user: ${TEST_VALUE}\n  postgres_password: ${TEST_VALUE}\n  postgres_db: ${TEST_VALUE}\nroles:\n  - name: ${TEST_VALUE}\n    password: ${TEST_VALUE}\n    permissions:\n      - database: ${TEST_VALUE}\n        schema: ${TEST_VALUE}\n        grants: [USAGE]\ndatabases:\n  - name: ${TEST_VALUE}\n    owner: ${TEST_VALUE}\n    schemas:\n      - ${TEST_VALUE}\n    extensions:\n      - ${TEST_VALUE}\n"
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
	}
	for _, value := range values {
		if value != "resolved" {
			t.Fatalf("resolved value = %q, want resolved", value)
		}
	}
}

func TestLoadRejectsPGBackRestConfiguration(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("pgbackrest:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
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
	options, err := InitDB{PostgresUser: "bootstrap_admin", PostgresPassword: "postgres-password"}.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if options.PostgresUser != "bootstrap_admin" || options.PostgresDB != "bootstrap_admin" {
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
		Databases: []Database{{Name: "app", Owner: "app_owner", Schemas: []MaybeEnv{"app"}}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "owned by role") {
		t.Fatalf("Validate() error = %v, want schema-owner grant error", err)
	}
}

func TestLoadRejectsMultipleYAMLDocuments(t *testing.T) {
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte("settings:\n  max_connections: 10\n---\nsettings:\n  max_connections: 20\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("Load() error = %v, want multiple-document error", err)
	}
}

func TestValidateRejectsDuplicateRolesAndIncompleteDatabases(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		message string
	}{
		{"duplicate role", Config{Roles: []Role{{Name: "reader"}, {Name: "reader"}}}, "declared more than once"},
		{"missing database owner", Config{Databases: []Database{{Name: "app"}}}, "databases[0].owner is required"},
		{"empty schema", Config{Databases: []Database{{Name: "app", Owner: "owner", Schemas: []MaybeEnv{""}}}}, "databases[0].schemas[0] is required"},
		{"empty extension", Config{Databases: []Database{{Name: "app", Owner: "owner", Extensions: []MaybeEnv{""}}}}, "databases[0].extensions[0] is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Validate() error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestResolveEnvironmentOrFile(t *testing.T) {
	path := t.TempDir() + "/postgres-user"
	if err := os.WriteFile(path, []byte("file_admin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_POSTGRES_USER_FILE", path)
	value, err := ResolveEnvironmentOrFile("TEST_POSTGRES_USER", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if value != "file_admin" {
		t.Fatalf("ResolveEnvironmentOrFile() = %q, want file_admin", value)
	}
	t.Setenv("TEST_POSTGRES_USER", "direct_admin")
	if _, err := ResolveEnvironmentOrFile("TEST_POSTGRES_USER", "postgres"); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("ResolveEnvironmentOrFile() conflict error = %v", err)
	}
	t.Setenv("TEST_POSTGRES_USER", "")
	value, err = ResolveEnvironmentOrFile("TEST_POSTGRES_USER", "postgres")
	if err != nil || value != "file_admin" {
		t.Fatalf("ResolveEnvironmentOrFile() with empty direct value = %q, %v", value, err)
	}
	t.Setenv("TEST_POSTGRES_USER_FILE", "")
	value, err = ResolveEnvironmentOrFile("TEST_POSTGRES_USER", "postgres")
	if err != nil || value != "postgres" {
		t.Fatalf("ResolveEnvironmentOrFile() with empty file variable = %q, %v", value, err)
	}
	emptyPath := t.TempDir() + "/empty-user"
	if err := os.WriteFile(emptyPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_POSTGRES_USER_FILE", emptyPath)
	value, err = ResolveEnvironmentOrFile("TEST_POSTGRES_USER", "postgres")
	if err != nil || value != "" {
		t.Fatalf("ResolveEnvironmentOrFile() with empty file = %q, %v", value, err)
	}
}

func TestValidateRejectsInitDBUserDuplicatedAsRole(t *testing.T) {
	cfg := Config{
		InitDB: &InitDB{PostgresUser: "postgres", PostgresPassword: "password"},
		Roles:  []Role{{Name: "postgres", Password: "different-password"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates initdb.postgres_user") {
		t.Fatalf("Validate() error = %v, want initdb user duplication error", err)
	}
}
