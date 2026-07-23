package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	InitDB                *InitDB        `yaml:"initdb"`
	Settings              map[string]any `yaml:"settings"`
	HBA                   []string       `yaml:"hba"`
	Roles                 []Role         `yaml:"roles"`
	Databases             []Database     `yaml:"databases"`
	PGBackRest            *PGBackRest    `yaml:"pgbackrest"`
	environmentReferences []string
}

type ValueSource struct {
	Value string `yaml:"value"`
	Env   string `yaml:"env"`
}

type Role struct {
	Name        ValueSource  `yaml:"name"`
	Password    SecretValue  `yaml:"password"`
	Permissions []Permission `yaml:"permissions"`
}

type Permission struct {
	Database    string   `yaml:"database"`
	Schema      string   `yaml:"schema"`
	Grants      []string `yaml:"grants"`
	TableGrants []string `yaml:"table_grants"`
}

type Database struct {
	Name       string   `yaml:"name"`
	Owner      string   `yaml:"owner"`
	Schemas    []string `yaml:"schemas"`
	Extensions []string `yaml:"extensions"`
}

type InitDB struct {
	PostgresUser     ValueSource `yaml:"postgres_user"`
	PostgresPassword SecretValue `yaml:"postgres_password"`
	PostgresDB       ValueSource `yaml:"postgres_db"`
}

type InitDBOptions struct {
	PostgresUser     string
	PostgresPassword string
	PostgresDB       string
}

type PGBackRest struct {
	Enabled              bool            `yaml:"enabled"`
	Stanza               string          `yaml:"stanza"`
	S3                   S3              `yaml:"s3"`
	Retention            Retention       `yaml:"retention"`
	Schedules            BackupSchedules `yaml:"schedules"`
	Archive              Archive         `yaml:"archive"`
	ProcessMax           int             `yaml:"process_max"`
	InitialBackup        *bool           `yaml:"initial_backup"`
	Timezone             string          `yaml:"timezone"`
	RepositoryCipherPass SecretValue     `yaml:"repository_cipher_pass"`
}

type S3 struct {
	Host      string      `yaml:"host"`
	Port      string      `yaml:"port"`
	Bucket    string      `yaml:"bucket"`
	Region    string      `yaml:"region"`
	URIStyle  string      `yaml:"uri_style"`
	VerifyTLS *bool       `yaml:"verify_tls"`
	AccessKey SecretValue `yaml:"access_key"`
	SecretKey SecretValue `yaml:"secret_key"`
}

type Retention struct {
	Full    int `yaml:"full"`
	Archive int `yaml:"archive"`
}

type BackupSchedules struct {
	Full         string `yaml:"full"`
	Differential string `yaml:"differential"`
	Check        string `yaml:"check"`
}

type Archive struct {
	PushQueueMax   string `yaml:"push_queue_max"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type SecretValue string

type PGBackRestOptions struct {
	Stanza                string
	S3Host                string
	S3Port                int
	S3Bucket              string
	S3Region              string
	S3URIStyle            string
	S3VerifyTLS           bool
	S3AccessKey           string
	S3SecretKey           string
	RepositoryCipherPass  string
	RetentionFull         int
	RetentionArchive      int
	FullSchedule          string
	DiffSchedule          string
	CheckSchedule         string
	ArchivePushQueueMax   string
	ArchiveTimeoutSeconds int
	ProcessMax            int
	InitialBackup         bool
	Timezone              string
}

func Load(path string) (Config, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	references := make(map[string]struct{})
	if err := resolveEnvironmentReferences(&document, references); err != nil {
		return Config{}, fmt.Errorf("resolve config values: %w", err)
	}
	resolved, err := yaml.Marshal(&document)
	if err != nil {
		return Config{}, fmt.Errorf("encode resolved config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(resolved))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	for name := range references {
		cfg.environmentReferences = append(cfg.environmentReferences, name)
	}
	sort.Strings(cfg.environmentReferences)
	return cfg, nil
}

func (cfg Config) EnvironmentReferences() []string {
	return cfg.environmentReferences
}

func (cfg Config) Validate() error {
	for roleIndex, role := range cfg.Roles {
		name, err := role.Name.Resolve(fmt.Sprintf("roles[%d].name", roleIndex))
		if err != nil {
			return err
		}
		for permissionIndex, permission := range role.Permissions {
			if err := permission.Validate(); err != nil {
				return fmt.Errorf("roles[%d].permissions[%d]: %w", roleIndex, permissionIndex, err)
			}
			for _, database := range cfg.Databases {
				if database.Name != permission.Database || database.Owner != name {
					continue
				}
				for _, schema := range database.Schemas {
					if permission.Schema == "" || permission.Schema == schema {
						return fmt.Errorf("roles[%d].permissions[%d] cannot manage grants on schema %q owned by role %q", roleIndex, permissionIndex, schema, name)
					}
				}
			}
		}
	}
	return nil
}

func (permission Permission) Validate() error {
	if permission.Database == "" || (len(permission.Grants) == 0 && len(permission.TableGrants) == 0) {
		return fmt.Errorf("database and at least one grant are required")
	}
	for _, grant := range permission.Grants {
		if grant != "CREATE" && grant != "USAGE" {
			return fmt.Errorf("unsupported schema grant %q", grant)
		}
	}
	for _, grant := range permission.TableGrants {
		if grant != "SELECT" && grant != "INSERT" && grant != "UPDATE" && grant != "DELETE" {
			return fmt.Errorf("unsupported table grant %q", grant)
		}
	}
	return nil
}

func (source ValueSource) Resolve(field string) (string, error) {
	if source.Value != "" && source.Env != "" {
		return "", fmt.Errorf("%s cannot set both value and env", field)
	}
	if source.Env != "" {
		value := os.Getenv(source.Env)
		if value == "" {
			return "", fmt.Errorf("%s environment variable %s is empty", field, source.Env)
		}
		return value, nil
	}
	if source.Value == "" {
		return "", fmt.Errorf("%s must set value or env", field)
	}
	return source.Value, nil
}

func (source ValueSource) ResolveWithDefault(field, defaultEnv, fallback string) (string, error) {
	if source.Value == "" && source.Env == "" {
		if value := os.Getenv(defaultEnv); value != "" {
			return value, nil
		}
		return fallback, nil
	}
	if source.Env == defaultEnv {
		if value := os.Getenv(defaultEnv); value != "" {
			return value, nil
		}
		return fallback, nil
	}
	return source.Resolve(field)
}

func (initDB InitDB) Resolve() (InitDBOptions, error) {
	user, err := initDB.PostgresUser.ResolveWithDefault("initdb.postgres_user", "POSTGRES_USER", "postgres")
	if err != nil {
		return InitDBOptions{}, err
	}
	password, err := initDB.PostgresPassword.Resolve("initdb.postgres_password", true)
	if err != nil {
		return InitDBOptions{}, err
	}
	database, err := initDB.PostgresDB.ResolveWithDefault("initdb.postgres_db", "POSTGRES_DB", user)
	if err != nil {
		return InitDBOptions{}, err
	}
	return InitDBOptions{
		PostgresUser:     user,
		PostgresPassword: password,
		PostgresDB:       database,
	}, nil
}

func (initDB InitDB) ResolveUser() (string, error) {
	return initDB.PostgresUser.ResolveWithDefault("initdb.postgres_user", "POSTGRES_USER", "postgres")
}

func (pgBackRest PGBackRest) Resolve() (PGBackRestOptions, error) {
	if pgBackRest.Stanza == "" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.stanza must be set")
	}
	if pgBackRest.S3.Host == "" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.host must be set")
	}
	if pgBackRest.S3.Bucket == "" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.bucket must be set")
	}

	accessKey, err := pgBackRest.S3.AccessKey.Resolve("pgbackrest.s3.access_key", true)
	if err != nil {
		return PGBackRestOptions{}, err
	}
	secretKey, err := pgBackRest.S3.SecretKey.Resolve("pgbackrest.s3.secret_key", true)
	if err != nil {
		return PGBackRestOptions{}, err
	}
	cipherPass, err := pgBackRest.RepositoryCipherPass.Resolve("pgbackrest.repository_cipher_pass", pgBackRest.RepositoryCipherPass.Configured())
	if err != nil {
		return PGBackRestOptions{}, err
	}
	port := 443
	if pgBackRest.S3.Port != "" {
		port, err = strconv.Atoi(pgBackRest.S3.Port)
		if err != nil || port < 1 || port > 65535 {
			return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.port must be an integer between 1 and 65535")
		}
	}

	uriStyle := defaultString(pgBackRest.S3.URIStyle, "path")
	if uriStyle != "path" && uriStyle != "host" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.uri_style must be path or host")
	}
	if pgBackRest.Retention.Full < 0 || pgBackRest.Retention.Archive < 0 {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest retention values cannot be negative")
	}
	if pgBackRest.ProcessMax < 0 {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.process_max cannot be negative")
	}
	if pgBackRest.Archive.TimeoutSeconds < 0 {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.archive.timeout_seconds cannot be negative")
	}

	return PGBackRestOptions{
		Stanza:                pgBackRest.Stanza,
		S3Host:                pgBackRest.S3.Host,
		S3Port:                port,
		S3Bucket:              pgBackRest.S3.Bucket,
		S3Region:              defaultString(pgBackRest.S3.Region, "us-east-1"),
		S3URIStyle:            uriStyle,
		S3VerifyTLS:           defaultBool(pgBackRest.S3.VerifyTLS, true),
		S3AccessKey:           accessKey,
		S3SecretKey:           secretKey,
		RepositoryCipherPass:  cipherPass,
		RetentionFull:         defaultInt(pgBackRest.Retention.Full, 2),
		RetentionArchive:      defaultInt(pgBackRest.Retention.Archive, 2),
		FullSchedule:          defaultString(pgBackRest.Schedules.Full, "0 2 * * 0"),
		DiffSchedule:          defaultString(pgBackRest.Schedules.Differential, "0 2 * * 1-6"),
		CheckSchedule:         defaultString(pgBackRest.Schedules.Check, "*/5 * * * *"),
		ArchivePushQueueMax:   defaultString(pgBackRest.Archive.PushQueueMax, "1GiB"),
		ArchiveTimeoutSeconds: defaultInt(pgBackRest.Archive.TimeoutSeconds, 60),
		ProcessMax:            defaultInt(pgBackRest.ProcessMax, 4),
		InitialBackup:         defaultBool(pgBackRest.InitialBackup, true),
		Timezone:              defaultString(pgBackRest.Timezone, "UTC"),
	}, nil
}

func (value SecretValue) Resolve(field string, required bool) (string, error) {
	if value == "" {
		if required {
			return "", fmt.Errorf("%s is required", field)
		}
		return "", nil
	}
	return string(value), nil
}

func (value SecretValue) Configured() bool {
	return value != ""
}

var environmentReference = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func resolveEnvironmentReferences(node *yaml.Node, references map[string]struct{}) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := resolveEnvironmentReferences(child, references); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for index := 1; index < len(node.Content); index += 2 {
			if err := resolveEnvironmentReferences(node.Content[index], references); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return nil
		}
		matches := environmentReference.FindStringSubmatch(node.Value)
		if len(matches) == 0 {
			return nil
		}
		value, ok := os.LookupEnv(matches[1])
		if !ok || value == "" {
			return fmt.Errorf("environment variable %s is empty", matches[1])
		}
		node.Value = value
		references[matches[1]] = struct{}{}
	}
	return nil
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func defaultInt(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

func defaultBool(value *bool, fallback bool) bool {
	if value != nil {
		return *value
	}
	return fallback
}
