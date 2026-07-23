package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	InitDB     *InitDB        `yaml:"initdb"`
	Settings   map[string]any `yaml:"settings"`
	HBA        []string       `yaml:"hba"`
	Roles      []Role         `yaml:"roles"`
	Databases  []Database     `yaml:"databases"`
	PGBackRest *PGBackRest    `yaml:"pgbackrest"`
}

type ValueSource struct {
	Value string `yaml:"value"`
	Env   string `yaml:"env"`
}

type Role struct {
	Name        ValueSource  `yaml:"name"`
	Password    SecretSource `yaml:"password"`
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
	PostgresUser     ValueSource  `yaml:"postgres_user"`
	PostgresPassword SecretSource `yaml:"postgres_password"`
	PostgresDB       ValueSource  `yaml:"postgres_db"`
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
	RepositoryCipherPass SecretSource    `yaml:"repository_cipher_pass"`
}

type S3 struct {
	Endpoint  string       `yaml:"endpoint"`
	Bucket    string       `yaml:"bucket"`
	Region    string       `yaml:"region"`
	URIStyle  string       `yaml:"uri_style"`
	VerifyTLS *bool        `yaml:"verify_tls"`
	AccessKey SecretSource `yaml:"access_key"`
	SecretKey SecretSource `yaml:"secret_key"`
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

type SecretSource struct {
	EnvValueKey    string `yaml:"env_value_key"`
	EnvFilePathKey string `yaml:"env_file_path_key"`
}

type PGBackRestOptions struct {
	Stanza                string
	S3Endpoint            string
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
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
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
	password, err := initDB.PostgresPassword.Resolve("initdb.postgres_password", "POSTGRES_PASSWORD", "POSTGRES_PASSWORD_FILE", true)
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
	if pgBackRest.S3.Endpoint == "" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.endpoint must be set")
	}
	if pgBackRest.S3.Bucket == "" {
		return PGBackRestOptions{}, fmt.Errorf("pgbackrest.s3.bucket must be set")
	}

	accessKey, err := pgBackRest.S3.AccessKey.Resolve("pgbackrest.s3.access_key", "PGBACKREST_S3_KEY", "PGBACKREST_S3_KEY_FILE", true)
	if err != nil {
		return PGBackRestOptions{}, err
	}
	secretKey, err := pgBackRest.S3.SecretKey.Resolve("pgbackrest.s3.secret_key", "PGBACKREST_S3_KEY_SECRET", "PGBACKREST_S3_KEY_SECRET_FILE", true)
	if err != nil {
		return PGBackRestOptions{}, err
	}
	cipherPass, err := pgBackRest.RepositoryCipherPass.Resolve("pgbackrest.repository_cipher_pass", "PGBACKREST_REPO_CIPHER_PASS", "PGBACKREST_REPO_CIPHER_PASS_FILE", pgBackRest.RepositoryCipherPass.Configured())
	if err != nil {
		return PGBackRestOptions{}, err
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
		S3Endpoint:            pgBackRest.S3.Endpoint,
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

func (source SecretSource) Resolve(field, defaultValueKey, defaultFilePathKey string, required bool) (string, error) {
	valueKey := source.EnvValueKey
	filePathKey := source.EnvFilePathKey
	if valueKey == "" && filePathKey == "" {
		valueKey = defaultValueKey
		filePathKey = defaultFilePathKey
	}
	value, hasValue := "", false
	if valueKey != "" {
		value, hasValue = os.LookupEnv(valueKey)
	}
	filePath, hasFilePath := "", false
	if filePathKey != "" {
		filePath, hasFilePath = os.LookupEnv(filePathKey)
	}
	if hasValue && hasFilePath {
		return "", fmt.Errorf("%s must set exactly one of %s and %s", field, valueKey, filePathKey)
	}
	if hasValue {
		if value == "" {
			return "", fmt.Errorf("%s environment variable %s is empty", field, valueKey)
		}
		return value, nil
	}
	if hasFilePath {
		if filePath == "" {
			return "", fmt.Errorf("%s environment variable %s is empty", field, filePathKey)
		}
		contents, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("read %s from %s: %w", field, filePathKey, err)
		}
		secret := strings.TrimSuffix(string(contents), "\n")
		if secret == "" {
			return "", fmt.Errorf("%s file %s is empty", field, filePath)
		}
		return secret, nil
	}
	if required {
		keys := make([]string, 0, 2)
		if valueKey != "" {
			keys = append(keys, valueKey)
		}
		if filePathKey != "" {
			keys = append(keys, filePathKey)
		}
		return "", fmt.Errorf("%s must set one of %s", field, strings.Join(keys, " and "))
	}
	return "", nil
}

func (source SecretSource) Configured() bool {
	return source.EnvValueKey != "" || source.EnvFilePathKey != ""
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
