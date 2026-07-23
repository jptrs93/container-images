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
	InitDB                *InitDB             `yaml:"initdb"`
	Settings              map[string]MaybeEnv `yaml:"settings"`
	HBA                   []MaybeEnv          `yaml:"hba"`
	Roles                 []Role              `yaml:"roles"`
	Databases             []Database          `yaml:"databases"`
	PGBackRest            *PGBackRest         `yaml:"pgbackrest"`
	environmentReferences []string
}

type MaybeEnv string

type Role struct {
	Name        MaybeEnv     `yaml:"name"`
	Password    MaybeEnv     `yaml:"password"`
	Permissions []Permission `yaml:"permissions"`
}

type Permission struct {
	Database    MaybeEnv `yaml:"database"`
	Schema      MaybeEnv `yaml:"schema"`
	Grants      []string `yaml:"grants"`
	TableGrants []string `yaml:"table_grants"`
}

type Database struct {
	Name       MaybeEnv   `yaml:"name"`
	Owner      MaybeEnv   `yaml:"owner"`
	Schemas    []MaybeEnv `yaml:"schemas"`
	Extensions []MaybeEnv `yaml:"extensions"`
}

type InitDB struct {
	PostgresUser     MaybeEnv `yaml:"postgres_user"`
	PostgresPassword MaybeEnv `yaml:"postgres_password"`
	PostgresDB       MaybeEnv `yaml:"postgres_db"`
}

type InitDBOptions struct {
	PostgresUser     string
	PostgresPassword string
	PostgresDB       string
}

type PGBackRest struct {
	Enabled              bool            `yaml:"enabled"`
	Stanza               MaybeEnv        `yaml:"stanza"`
	S3                   S3              `yaml:"s3"`
	Retention            Retention       `yaml:"retention"`
	Schedules            BackupSchedules `yaml:"schedules"`
	Archive              Archive         `yaml:"archive"`
	ProcessMax           int             `yaml:"process_max"`
	InitialBackup        *bool           `yaml:"initial_backup"`
	Timezone             MaybeEnv        `yaml:"timezone"`
	RepositoryCipherPass MaybeEnv        `yaml:"repository_cipher_pass"`
}

type S3 struct {
	Host      MaybeEnv `yaml:"host"`
	Port      MaybeEnv `yaml:"port"`
	Bucket    MaybeEnv `yaml:"bucket"`
	Region    MaybeEnv `yaml:"region"`
	URIStyle  MaybeEnv `yaml:"uri_style"`
	VerifyTLS *bool    `yaml:"verify_tls"`
	AccessKey MaybeEnv `yaml:"access_key"`
	SecretKey MaybeEnv `yaml:"secret_key"`
}

type Retention struct {
	Full    int `yaml:"full"`
	Archive int `yaml:"archive"`
}

type BackupSchedules struct {
	Full         MaybeEnv `yaml:"full"`
	Differential MaybeEnv `yaml:"differential"`
	Check        MaybeEnv `yaml:"check"`
}

type Archive struct {
	PushQueueMax   MaybeEnv `yaml:"push_queue_max"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

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

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.resolveEnvironmentReferences(); err != nil {
		return Config{}, fmt.Errorf("resolve config values: %w", err)
	}
	return cfg, nil
}

func (cfg Config) EnvironmentReferences() []string {
	return cfg.environmentReferences
}

func (cfg Config) Validate() error {
	for roleIndex, role := range cfg.Roles {
		name, err := role.Name.Required(fmt.Sprintf("roles[%d].name", roleIndex))
		if err != nil {
			return err
		}
		for permissionIndex, permission := range role.Permissions {
			if err := permission.Validate(); err != nil {
				return fmt.Errorf("roles[%d].permissions[%d]: %w", roleIndex, permissionIndex, err)
			}
			for _, database := range cfg.Databases {
				if database.Name != permission.Database || string(database.Owner) != name {
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

func (value MaybeEnv) Resolve() (string, error) {
	matches := environmentReference.FindStringSubmatch(string(value))
	if len(matches) == 0 {
		return string(value), nil
	}
	resolved, ok := os.LookupEnv(matches[1])
	if !ok || resolved == "" {
		return "", fmt.Errorf("environment variable %s is empty", matches[1])
	}
	return resolved, nil
}

func (value MaybeEnv) Required(field string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return string(value), nil
}

func (value MaybeEnv) ResolveWithDefault(defaultEnv, fallback string) string {
	if value == "" {
		if value := os.Getenv(defaultEnv); value != "" {
			return value
		}
		return fallback
	}
	return string(value)
}

func (initDB InitDB) Resolve() (InitDBOptions, error) {
	user := initDB.PostgresUser.ResolveWithDefault("POSTGRES_USER", "postgres")
	password, err := initDB.PostgresPassword.Required("initdb.postgres_password")
	if err != nil {
		return InitDBOptions{}, err
	}
	database := initDB.PostgresDB.ResolveWithDefault("POSTGRES_DB", user)
	return InitDBOptions{
		PostgresUser:     user,
		PostgresPassword: password,
		PostgresDB:       database,
	}, nil
}

func (initDB InitDB) ResolveUser() (string, error) {
	return initDB.PostgresUser.ResolveWithDefault("POSTGRES_USER", "postgres"), nil
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

	accessKey, err := pgBackRest.S3.AccessKey.Required("pgbackrest.s3.access_key")
	if err != nil {
		return PGBackRestOptions{}, err
	}
	secretKey, err := pgBackRest.S3.SecretKey.Required("pgbackrest.s3.secret_key")
	if err != nil {
		return PGBackRestOptions{}, err
	}
	cipherPass := ""
	if pgBackRest.RepositoryCipherPass.Configured() {
		cipherPass, err = pgBackRest.RepositoryCipherPass.Required("pgbackrest.repository_cipher_pass")
	}
	if err != nil {
		return PGBackRestOptions{}, err
	}
	port := 443
	if pgBackRest.S3.Port != "" {
		port, err = strconv.Atoi(string(pgBackRest.S3.Port))
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
		Stanza:                string(pgBackRest.Stanza),
		S3Host:                string(pgBackRest.S3.Host),
		S3Port:                port,
		S3Bucket:              string(pgBackRest.S3.Bucket),
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

func (value MaybeEnv) Configured() bool {
	return value != ""
}

var environmentReference = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func (cfg *Config) resolveEnvironmentReferences() error {
	references := make(map[string]struct{})
	resolve := func(field string, value *MaybeEnv) error {
		matches := environmentReference.FindStringSubmatch(string(*value))
		if len(matches) == 0 {
			return nil
		}
		resolved, err := value.Resolve()
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		*value = MaybeEnv(resolved)
		references[matches[1]] = struct{}{}
		return nil
	}

	for name, value := range cfg.Settings {
		if err := resolve("settings."+name, &value); err != nil {
			return err
		}
		cfg.Settings[name] = value
	}
	for index := range cfg.HBA {
		if err := resolve(fmt.Sprintf("hba[%d]", index), &cfg.HBA[index]); err != nil {
			return err
		}
	}
	if cfg.InitDB != nil {
		if err := resolve("initdb.postgres_user", &cfg.InitDB.PostgresUser); err != nil {
			return err
		}
		if err := resolve("initdb.postgres_password", &cfg.InitDB.PostgresPassword); err != nil {
			return err
		}
		if err := resolve("initdb.postgres_db", &cfg.InitDB.PostgresDB); err != nil {
			return err
		}
	}
	for roleIndex := range cfg.Roles {
		role := &cfg.Roles[roleIndex]
		if err := resolve(fmt.Sprintf("roles[%d].name", roleIndex), &role.Name); err != nil {
			return err
		}
		if err := resolve(fmt.Sprintf("roles[%d].password", roleIndex), &role.Password); err != nil {
			return err
		}
		for permissionIndex := range role.Permissions {
			permission := &role.Permissions[permissionIndex]
			if err := resolve(fmt.Sprintf("roles[%d].permissions[%d].database", roleIndex, permissionIndex), &permission.Database); err != nil {
				return err
			}
			if err := resolve(fmt.Sprintf("roles[%d].permissions[%d].schema", roleIndex, permissionIndex), &permission.Schema); err != nil {
				return err
			}
		}
	}
	for databaseIndex := range cfg.Databases {
		database := &cfg.Databases[databaseIndex]
		if err := resolve(fmt.Sprintf("databases[%d].name", databaseIndex), &database.Name); err != nil {
			return err
		}
		if err := resolve(fmt.Sprintf("databases[%d].owner", databaseIndex), &database.Owner); err != nil {
			return err
		}
		for schemaIndex := range database.Schemas {
			if err := resolve(fmt.Sprintf("databases[%d].schemas[%d]", databaseIndex, schemaIndex), &database.Schemas[schemaIndex]); err != nil {
				return err
			}
		}
		for extensionIndex := range database.Extensions {
			if err := resolve(fmt.Sprintf("databases[%d].extensions[%d]", databaseIndex, extensionIndex), &database.Extensions[extensionIndex]); err != nil {
				return err
			}
		}
	}
	if cfg.PGBackRest != nil {
		pgBackRest := cfg.PGBackRest
		values := []struct {
			field string
			value *MaybeEnv
		}{
			{"pgbackrest.stanza", &pgBackRest.Stanza},
			{"pgbackrest.repository_cipher_pass", &pgBackRest.RepositoryCipherPass},
			{"pgbackrest.s3.host", &pgBackRest.S3.Host},
			{"pgbackrest.s3.port", &pgBackRest.S3.Port},
			{"pgbackrest.s3.bucket", &pgBackRest.S3.Bucket},
			{"pgbackrest.s3.region", &pgBackRest.S3.Region},
			{"pgbackrest.s3.uri_style", &pgBackRest.S3.URIStyle},
			{"pgbackrest.s3.access_key", &pgBackRest.S3.AccessKey},
			{"pgbackrest.s3.secret_key", &pgBackRest.S3.SecretKey},
			{"pgbackrest.schedules.full", &pgBackRest.Schedules.Full},
			{"pgbackrest.schedules.differential", &pgBackRest.Schedules.Differential},
			{"pgbackrest.schedules.check", &pgBackRest.Schedules.Check},
			{"pgbackrest.archive.push_queue_max", &pgBackRest.Archive.PushQueueMax},
			{"pgbackrest.timezone", &pgBackRest.Timezone},
		}
		for _, item := range values {
			if err := resolve(item.field, item.value); err != nil {
				return err
			}
		}
	}
	for name := range references {
		cfg.environmentReferences = append(cfg.environmentReferences, name)
	}
	sort.Strings(cfg.environmentReferences)
	return nil
}

func defaultString(value MaybeEnv, fallback string) string {
	if value != "" {
		return string(value)
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
