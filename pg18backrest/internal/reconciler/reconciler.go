package reconciler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jptrs93/container-images/pg18backrest/internal/config"
)

func Reconcile(ctx context.Context, cfg config.Config, initDB *config.InitDBOptions) error {
	adminURL := os.Getenv("POSTGRES_SUPERVISOR_ADMIN_URL")
	if adminURL == "" {
		adminURL = socketURL(adminUser(initDB), "postgres")
	}

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect as supervisor admin: %w", err)
	}
	defer admin.Close(ctx)

	for index, role := range cfg.Roles {
		name, err := role.Name.Resolve(fmt.Sprintf("roles[%d].name", index))
		if err != nil {
			return err
		}
		password, err := role.Password.Resolve(fmt.Sprintf("roles[%d].password", index), "", "", role.Password.Configured())
		if err != nil {
			return err
		}
		if err := ensureRole(ctx, admin, name, password); err != nil {
			return err
		}
	}
	if initDB != nil {
		if err := setRolePassword(ctx, admin, initDB.PostgresUser, initDB.PostgresPassword); err != nil {
			return fmt.Errorf("reconcile initdb superuser password: %w", err)
		}
	}

	for _, database := range cfg.Databases {
		if err := ensureDatabase(ctx, admin, adminURL, database); err != nil {
			return err
		}
	}

	for index, role := range cfg.Roles {
		name, err := role.Name.Resolve(fmt.Sprintf("roles[%d].name", index))
		if err != nil {
			return err
		}
		for _, permission := range role.Permissions {
			if err := applyPermission(ctx, adminURL, name, permission); err != nil {
				return err
			}
		}
	}

	return nil
}

func ensureRole(ctx context.Context, conn *pgx.Conn, name, password string) error {
	var exists bool
	if err := conn.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check role %q: %w", name, err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "create role "+identifier(name)+" login"); err != nil {
			return fmt.Errorf("create role %q: %w", name, err)
		}
	}
	if password != "" {
		if err := setRolePassword(ctx, conn, name, password); err != nil {
			return fmt.Errorf("set password for role %q: %w", name, err)
		}
	}
	return nil
}

func setRolePassword(ctx context.Context, conn *pgx.Conn, name, password string) error {
	if _, err := conn.Exec(ctx, "select set_config('postgres_supervisor.role', $1, false), set_config('postgres_supervisor.password', $2, false)", name, password); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "select set_config('postgres_supervisor.role', '', false), set_config('postgres_supervisor.password', '', false)")
	_, err := conn.Exec(ctx, "do $supervisor$ begin execute format('alter role %I password %L', current_setting('postgres_supervisor.role'), current_setting('postgres_supervisor.password')); end $supervisor$")
	return err
}

func ensureDatabase(ctx context.Context, conn *pgx.Conn, adminURL string, database config.Database) error {
	if database.Name == "" || database.Owner == "" {
		return fmt.Errorf("database name and owner are required")
	}

	var exists bool
	if err := conn.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", database.Name).Scan(&exists); err != nil {
		return fmt.Errorf("check database %q: %w", database.Name, err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "create database "+identifier(database.Name)+" owner "+identifier(database.Owner)); err != nil {
			return fmt.Errorf("create database %q: %w", database.Name, err)
		}
	}

	targetURL := databaseURL(adminURL, database.Name)
	databaseConn, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		return fmt.Errorf("connect to database %q: %w", database.Name, err)
	}
	defer databaseConn.Close(ctx)

	for _, schema := range database.Schemas {
		if schema == "" {
			return fmt.Errorf("database %q has an empty schema", database.Name)
		}
		statement := "create schema if not exists " + identifier(schema) + " authorization " + identifier(database.Owner)
		if _, err := databaseConn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create schema %q in database %q: %w", schema, database.Name, err)
		}
	}
	for _, extension := range database.Extensions {
		if extension == "" {
			return fmt.Errorf("database %q has an empty extension", database.Name)
		}
		if _, err := databaseConn.Exec(ctx, "create extension if not exists "+identifier(extension)); err != nil {
			return fmt.Errorf("create extension %q in database %q: %w", extension, database.Name, err)
		}
	}
	return nil
}

func applyPermission(ctx context.Context, adminURL, role string, permission config.Permission) error {
	if permission.Database == "" || len(permission.Grants) == 0 {
		return fmt.Errorf("permission database and grants are required")
	}
	for _, grant := range permission.Grants {
		if grant != "CREATE" && grant != "USAGE" {
			return fmt.Errorf("unsupported schema grant %q", grant)
		}
	}

	targetURL := databaseURL(adminURL, permission.Database)
	conn, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		return fmt.Errorf("connect to permission database %q: %w", permission.Database, err)
	}
	defer conn.Close(ctx)

	schemas := []string{permission.Schema}
	if permission.Schema == "" {
		rows, err := conn.Query(ctx, "select nspname from pg_namespace where nspname <> 'information_schema' and nspname not like 'pg_%'")
		if err != nil {
			return fmt.Errorf("list schemas in database %q: %w", permission.Database, err)
		}
		defer rows.Close()
		schemas = nil
		for rows.Next() {
			var schema string
			if err := rows.Scan(&schema); err != nil {
				return fmt.Errorf("scan schema: %w", err)
			}
			schemas = append(schemas, schema)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate schemas: %w", err)
		}
	}

	for _, schema := range schemas {
		statement := "grant " + strings.Join(permission.Grants, ", ") + " on schema " + identifier(schema) + " to " + identifier(role)
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("grant on schema %q in database %q: %w", schema, permission.Database, err)
		}
	}
	return nil
}

func socketURL(user, database string) string {
	if user == "" {
		user = adminUser(nil)
	}
	url := &url.URL{Scheme: "postgres", User: url.User(user), Path: "/" + database}
	query := url.Query()
	query.Set("host", "/var/run/postgresql")
	url.RawQuery = query.Encode()
	return url.String()
}

func adminUser(initDB *config.InitDBOptions) string {
	if user := os.Getenv("POSTGRES_SUPERVISOR_ADMIN_USER"); user != "" {
		return user
	}
	if initDB != nil {
		return initDB.PostgresUser
	}
	if user := os.Getenv("POSTGRES_USER"); user != "" {
		return user
	}
	return "postgres"
}

func databaseURL(adminURL, database string) string {
	parsed, err := url.Parse(adminURL)
	if err != nil {
		return adminURL
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func identifier(name string) string {
	return pgx.Identifier{name}.Sanitize()
}
