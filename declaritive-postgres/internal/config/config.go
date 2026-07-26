package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	InitDB                *InitDB             `yaml:"initdb"`
	Settings              map[string]MaybeEnv `yaml:"settings"`
	HBA                   []MaybeEnv          `yaml:"hba"`
	Roles                 []Role              `yaml:"roles"`
	Databases             []Database          `yaml:"databases"`
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
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
		return Config{}, fmt.Errorf("parse config: multiple YAML documents are not supported")
	}
	if err := cfg.resolveEnvironmentReferences(); err != nil {
		return Config{}, fmt.Errorf("resolve config values: %w", err)
	}
	return cfg, nil
}

func (cfg Config) EnvironmentReferences() []string {
	return cfg.environmentReferences
}

func (cfg *Config) Validate() error {
	if err := validatePostgresSettings(cfg.Settings); err != nil {
		return err
	}
	for index, record := range cfg.HBA {
		if strings.TrimSpace(string(record)) == "" {
			return fmt.Errorf("hba[%d] cannot be empty", index)
		}
		if strings.ContainsAny(string(record), "\r\n") {
			return fmt.Errorf("hba[%d] must be a single line", index)
		}
	}
	initDBUser := ""
	if cfg.InitDB != nil {
		options, err := cfg.InitDB.Resolve()
		if err != nil {
			return err
		}
		initDBUser = options.PostgresUser
	}

	roleNames := make(map[string]struct{}, len(cfg.Roles))
	for roleIndex, role := range cfg.Roles {
		name, err := role.Name.Required(fmt.Sprintf("roles[%d].name", roleIndex))
		if err != nil {
			return err
		}
		if _, exists := roleNames[name]; exists {
			return fmt.Errorf("roles[%d].name %q is declared more than once", roleIndex, name)
		}
		if name == initDBUser {
			return fmt.Errorf("roles[%d].name %q duplicates initdb.postgres_user", roleIndex, name)
		}
		roleNames[name] = struct{}{}
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

	databaseNames := make(map[string]struct{}, len(cfg.Databases))
	for databaseIndex, database := range cfg.Databases {
		name, err := database.Name.Required(fmt.Sprintf("databases[%d].name", databaseIndex))
		if err != nil {
			return err
		}
		if _, exists := databaseNames[name]; exists {
			return fmt.Errorf("databases[%d].name %q is declared more than once", databaseIndex, name)
		}
		databaseNames[name] = struct{}{}

		schemas := make(map[string]struct{}, len(database.Schemas))
		for schemaIndex, schema := range database.Schemas {
			value, err := schema.Required(fmt.Sprintf("databases[%d].schemas[%d]", databaseIndex, schemaIndex))
			if err != nil {
				return err
			}
			if _, exists := schemas[value]; exists {
				return fmt.Errorf("databases[%d].schemas[%d] %q is declared more than once", databaseIndex, schemaIndex, value)
			}
			schemas[value] = struct{}{}
		}

		extensions := make(map[string]struct{}, len(database.Extensions))
		for extensionIndex, extension := range database.Extensions {
			value, err := extension.Required(fmt.Sprintf("databases[%d].extensions[%d]", databaseIndex, extensionIndex))
			if err != nil {
				return err
			}
			if _, exists := extensions[value]; exists {
				return fmt.Errorf("databases[%d].extensions[%d] %q is declared more than once", databaseIndex, extensionIndex, value)
			}
			extensions[value] = struct{}{}
		}
	}
	cfg.insertDefaultsIfMissing()
	return nil
}

// schemaGrants and tableGrants are the privileges PostgreSQL defines for each
// object class. Sequence privileges are not declared directly: they follow from
// the table grants, because a role that can INSERT also needs to draw from the
// sequences behind any serial column.
var (
	schemaGrants = []string{"CREATE", "USAGE"}
	tableGrants  = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}
)

func (permission Permission) Validate() error {
	if permission.Database == "" || (len(permission.Grants) == 0 && len(permission.TableGrants) == 0) {
		return fmt.Errorf("database and at least one grant are required")
	}
	if err := validateGrants("schema", permission.Grants, schemaGrants); err != nil {
		return err
	}
	return validateGrants("table", permission.TableGrants, tableGrants)
}

// validateGrants accepts any combination of the supported privileges, or the
// single value ALL. Mixing ALL with named privileges is rejected rather than
// silently treated as ALL, so the YAML always states one intent.
func validateGrants(class string, grants, supported []string) error {
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if grant != "ALL" && !slices.Contains(supported, grant) {
			return fmt.Errorf("unsupported %s grant %q", class, grant)
		}
		if _, exists := seen[grant]; exists {
			return fmt.Errorf("%s grant %q is declared more than once", class, grant)
		}
		seen[grant] = struct{}{}
	}
	if _, exists := seen["ALL"]; exists && len(grants) > 1 {
		return fmt.Errorf("%s grant ALL cannot be combined with other privileges", class)
	}
	return nil
}

func (value MaybeEnv) Resolve() (string, error) {
	matches := environmentReference.FindStringSubmatch(string(value))
	if len(matches) == 0 {
		return string(value), nil
	}
	name := matches[1]
	hasDefault := matches[2] != ""
	resolved, ok := os.LookupEnv(name)
	if ok && resolved != "" {
		return resolved, nil
	}
	if hasDefault {
		return matches[3], nil
	}
	return "", fmt.Errorf("environment variable %s is empty", name)
}

func (value MaybeEnv) Required(field string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return string(value), nil
}

func (value MaybeEnv) ResolveWithDefault(defaultEnv, fallback string) (string, error) {
	if value == "" {
		return ResolveEnvironmentOrFile(defaultEnv, fallback)
	}
	return string(value), nil
}

func (value MaybeEnv) Configured() bool {
	return value != ""
}

func (initDB InitDB) Resolve() (InitDBOptions, error) {
	user, err := initDB.PostgresUser.ResolveWithDefault("POSTGRES_USER", "postgres")
	if err != nil {
		return InitDBOptions{}, err
	}
	password, err := initDB.PostgresPassword.Required("initdb.postgres_password")
	if err != nil {
		return InitDBOptions{}, err
	}
	database, err := initDB.PostgresDB.ResolveWithDefault("POSTGRES_DB", user)
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
	return initDB.PostgresUser.ResolveWithDefault("POSTGRES_USER", "postgres")
}

func ResolveEnvironmentOrFile(name, fallback string) (string, error) {
	value := os.Getenv(name)
	file := os.Getenv(name + "_FILE")
	if value != "" && file != "" {
		return "", fmt.Errorf("both %s and %s_FILE are set", name, name)
	}
	if value != "" {
		return value, nil
	}
	if file == "" {
		return fallback, nil
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	value = strings.TrimRight(string(contents), "\r\n")
	return value, nil
}

// environmentReference matches ${NAME} or ${NAME:-default}. The default may be
// empty (${NAME:-}) and may contain any characters except '}'.
var environmentReference = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}$`)

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
	for name := range references {
		cfg.environmentReferences = append(cfg.environmentReferences, name)
	}
	sort.Strings(cfg.environmentReferences)
	return nil
}
