package reconciler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/config"
)

func Reconcile(ctx context.Context, cfg config.Config, initDB *config.InitDBOptions, connection config.ConnectionOptions, supervisorUser string) error {
	adminURL := os.Getenv("POSTGRES_SUPERVISOR_ADMIN_URL")
	if adminURL == "" {
		adminURL = socketURL(supervisorUser, "postgres", connection)
	}

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect as supervisor admin: %w", err)
	}
	defer admin.Close(ctx)

	for index, role := range cfg.Roles {
		name, err := role.Name.Required(fmt.Sprintf("roles[%d].name", index))
		if err != nil {
			return err
		}
		password := ""
		if role.Password.Configured() {
			password, err = role.Password.Required(fmt.Sprintf("roles[%d].password", index))
		}
		if err != nil {
			return err
		}
		if err := ensureRole(ctx, admin, name, password, role.CanLogin()); err != nil {
			return err
		}
	}
	if initDB != nil {
		if err := setRolePassword(ctx, admin, initDB.PostgresUser, initDB.PostgresPassword); err != nil {
			return fmt.Errorf("reconcile initdb superuser password: %w", err)
		}
	}

	// Granted roles are reconciled once every declared role exists, so a role can
	// be granted another role declared later in the file.
	for index, role := range cfg.Roles {
		name, err := role.Name.Required(fmt.Sprintf("roles[%d].name", index))
		if err != nil {
			return err
		}
		// The two lists differ only in what they document: PostgreSQL has one
		// membership mechanism, so they are reconciled as a single set.
		groups := make([]string, 0, len(role.Roles)+len(role.DefaultRoles))
		for _, list := range []struct {
			field   string
			entries []config.MaybeEnv
		}{{"roles", role.Roles}, {"default_roles", role.DefaultRoles}} {
			for grantedIndex, member := range list.entries {
				group, err := member.Required(fmt.Sprintf("roles[%d].%s[%d]", index, list.field, grantedIndex))
				if err != nil {
					return err
				}
				groups = append(groups, group)
			}
		}
		if err := ensureRoleMemberships(ctx, admin, name, groups); err != nil {
			return err
		}
	}

	// An omitted owner leaves the database with the bootstrap superuser, which is
	// also the role the supervisor reconciles as.
	defaultOwner := supervisorUser
	if defaultOwner == "" {
		defaultOwner = "postgres"
	}
	for _, database := range cfg.Databases {
		if err := ensureDatabase(ctx, admin, adminURL, database, defaultOwner); err != nil {
			return err
		}
	}

	roleNames := make([]string, 0, len(cfg.Roles))
	for index, role := range cfg.Roles {
		name, err := role.Name.Required(fmt.Sprintf("roles[%d].name", index))
		if err != nil {
			return err
		}
		roleNames = append(roleNames, name)
	}

	// PostgreSQL scopes default privileges to the role that creates an object, so
	// the collapsed semantics of this config - one grant covering existing and
	// future objects alike - only hold if every role able to create objects in a
	// schema carries a matching default-privilege entry. The creator set comes
	// from the catalog, which resolves ownership, membership and superuser status
	// for us. It is snapshotted before anything is revoked so that entries left
	// by roles which have since lost CREATE, or dropped out of the config
	// entirely, are still cleaned up.
	databases := permissionDatabases(cfg)
	previousCreators, err := snapshotCreators(ctx, adminURL, databases)
	if err != nil {
		return err
	}

	for index, name := range roleNames {
		if err := resetPermissions(ctx, adminURL, name, databases, previousCreators); err != nil {
			return err
		}
		for _, permission := range cfg.Roles[index].Permissions {
			if err := applyGrants(ctx, adminURL, name, permission); err != nil {
				return err
			}
		}
	}

	// Every CREATE grant the config asks for has now been applied, so the creator
	// set is final and default privileges can be pinned to it.
	currentCreators, err := snapshotCreators(ctx, adminURL, databases)
	if err != nil {
		return err
	}
	for index, name := range roleNames {
		for _, permission := range cfg.Roles[index].Permissions {
			if err := applyDefaultPrivileges(ctx, adminURL, name, permission, currentCreators); err != nil {
				return err
			}
		}
	}

	return nil
}

// schemaKey identifies one schema within one database.
type schemaKey struct {
	database string
	schema   string
}

// permissionDatabases lists every database whose grants this run may touch.
func permissionDatabases(cfg config.Config) []string {
	names := make(map[string]struct{}, len(cfg.Databases))
	for _, database := range cfg.Databases {
		names[string(database.Name)] = struct{}{}
	}
	for _, role := range cfg.Roles {
		for _, permission := range role.Permissions {
			names[string(permission.Database)] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	return ordered
}

// snapshotCreators records, for every schema of every given database, the roles
// whose newly created objects would pick up default privileges there.
func snapshotCreators(ctx context.Context, adminURL string, databases []string) (map[schemaKey][]string, error) {
	creators := make(map[schemaKey][]string)
	for _, database := range databases {
		conn, err := pgx.Connect(ctx, databaseURL(adminURL, database))
		if err != nil {
			return nil, fmt.Errorf("connect to permission database %q: %w", database, err)
		}
		schemas, err := nonSystemSchemas(ctx, conn, database)
		if err == nil {
			for _, schema := range schemas {
				var roles []string
				if roles, err = schemaCreators(ctx, conn, database, schema); err != nil {
					break
				}
				creators[schemaKey{database, schema}] = roles
			}
		}
		conn.Close(ctx)
		if err != nil {
			return nil, err
		}
	}
	return creators, nil
}

// hasRoutineGrants reports whether a role holds an explicit privilege on any
// routine in a schema. Routines with a null proacl are left at the PostgreSQL
// default, which names no role directly and so cannot need revoking.
func hasRoutineGrants(ctx context.Context, conn *pgx.Conn, schema, role string) (bool, error) {
	const query = `select exists(
	select 1 from pg_proc
	join pg_namespace on pg_namespace.oid = pg_proc.pronamespace
	where pg_namespace.nspname = $1 and pg_proc.proacl is not null
		and exists(select 1 from aclexplode(pg_proc.proacl) where grantee = $2::regrole)
)`

	var granted bool
	if err := conn.QueryRow(ctx, query, schema, role).Scan(&granted); err != nil {
		return false, fmt.Errorf("check routine grants for role %q in schema %q: %w", role, schema, err)
	}
	return granted, nil
}

// schemaCreators lists the roles that can create objects in a schema.
// has_schema_privilege answers this exactly: it accounts for the schema owner,
// superusers, roles granted CREATE outright, and roles that inherit CREATE
// through membership. Predefined pg_ roles are excluded because they cannot be
// logged into and never end up owning objects.
func schemaCreators(ctx context.Context, conn *pgx.Conn, database, schema string) ([]string, error) {
	const query = `select rolname from pg_roles
where rolname not like 'pg\_%' and has_schema_privilege(oid, $1, 'CREATE')
order by rolname`

	rows, err := conn.Query(ctx, query, schema)
	if err != nil {
		return nil, fmt.Errorf("list creators for schema %q in database %q: %w", schema, database, err)
	}
	defer rows.Close()

	var creators []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scan creator: %w", err)
		}
		creators = append(creators, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate creators for schema %q in database %q: %w", schema, database, err)
	}
	return creators, nil
}

func resetPermissions(ctx context.Context, adminURL, role string, databases []string, creators map[schemaKey][]string) error {
	for _, database := range databases {
		conn, err := pgx.Connect(ctx, databaseURL(adminURL, database))
		if err != nil {
			return fmt.Errorf("connect to permission database %q: %w", database, err)
		}
		schemas, err := nonSystemSchemas(ctx, conn, database)
		if err == nil {
			for _, schema := range schemas {
				owner, ownerErr := schemaOwner(ctx, conn, database, schema)
				if ownerErr != nil {
					err = ownerErr
					break
				}
				// A role that owns the schema keeps its rights: they come from
				// ownership rather than from anything this config granted.
				if owner == role {
					continue
				}
				if err = revokeSchemaPrivileges(ctx, conn, role, schema, creators[schemaKey{database, schema}]); err != nil {
					break
				}
			}
		}
		conn.Close(ctx)
		if err != nil {
			return fmt.Errorf("reset permissions for role %q in database %q: %w", role, database, err)
		}
	}
	return nil
}

// revokeSchemaPrivileges strips everything this config could have granted a role
// in one schema, so that what the YAML no longer asks for genuinely disappears.
func revokeSchemaPrivileges(ctx context.Context, conn *pgx.Conn, role, schema string, creators []string) error {
	for _, class := range []string{"schema " + identifier(schema), "all tables in schema " + identifier(schema),
		"all sequences in schema " + identifier(schema)} {
		if _, err := conn.Exec(ctx, "revoke all privileges on "+class+" from "+identifier(role)); err != nil {
			return err
		}
	}
	// Functions carry an implicit EXECUTE for PUBLIC, and revoking from a role
	// that holds nothing still rewrites every function's ACL to spell that
	// default out. Behaviour would be unchanged, but the catalog and every
	// pg_dump after it would be littered with grants nobody asked for, so the
	// revoke only runs when there is genuinely something to take away.
	granted, err := hasRoutineGrants(ctx, conn, schema, role)
	if err != nil {
		return err
	}
	if granted {
		if _, err := conn.Exec(ctx, "revoke all privileges on all routines in schema "+identifier(schema)+" from "+identifier(role)); err != nil {
			return err
		}
	}
	for _, creator := range creators {
		prefix := "alter default privileges for role " + identifier(creator) + " in schema " + identifier(schema) + " revoke all on "
		for _, class := range []string{"tables", "sequences", "routines"} {
			if _, err := conn.Exec(ctx, prefix+class+" from "+identifier(role)); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureRole(ctx context.Context, conn *pgx.Conn, name, password string, canLogin bool) error {
	attribute := "nologin"
	if canLogin {
		attribute = "login"
	}

	var currentLogin *bool
	if err := conn.QueryRow(ctx, "select rolcanlogin from pg_roles where rolname = $1", name).Scan(&currentLogin); err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("check role %q: %w", name, err)
	}
	switch {
	case currentLogin == nil:
		if _, err := conn.Exec(ctx, "create role "+identifier(name)+" "+attribute); err != nil {
			return fmt.Errorf("create role %q: %w", name, err)
		}
	case *currentLogin != canLogin:
		// Login is reconciled like everything else, so turning a role into a group
		// after the fact takes its ability to connect away.
		if _, err := conn.Exec(ctx, "alter role "+identifier(name)+" "+attribute); err != nil {
			return fmt.Errorf("set %s on role %q: %w", attribute, name, err)
		}
	}
	if password != "" {
		if err := setRolePassword(ctx, conn, name, password); err != nil {
			return fmt.Errorf("set password for role %q: %w", name, err)
		}
	}
	return nil
}

// ensureRoleMemberships makes the role's memberships match the declaration
// exactly: memberships that are not declared are revoked, so removing an entry
// from the YAML converges rather than leaving the grant behind. Only roles
// declared in the configuration are touched.
func ensureRoleMemberships(ctx context.Context, conn *pgx.Conn, role string, groups []string) error {
	rows, err := conn.Query(ctx, "select granted.rolname from pg_auth_members join pg_roles granted on granted.oid = pg_auth_members.roleid join pg_roles member on member.oid = pg_auth_members.member where member.rolname = $1", role)
	if err != nil {
		return fmt.Errorf("list memberships of role %q: %w", role, err)
	}
	current := map[string]struct{}{}
	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			rows.Close()
			return fmt.Errorf("scan membership of role %q: %w", role, err)
		}
		current[group] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate memberships of role %q: %w", role, err)
	}

	declared := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		declared[group] = struct{}{}
		if _, exists := current[group]; exists {
			continue
		}
		if _, err := conn.Exec(ctx, "grant "+identifier(group)+" to "+identifier(role)); err != nil {
			return fmt.Errorf("grant membership of %q to role %q: %w", group, role, err)
		}
	}

	revoked := make([]string, 0, len(current))
	for group := range current {
		if _, exists := declared[group]; !exists {
			revoked = append(revoked, group)
		}
	}
	sort.Strings(revoked)
	for _, group := range revoked {
		if _, err := conn.Exec(ctx, "revoke "+identifier(group)+" from "+identifier(role)); err != nil {
			return fmt.Errorf("revoke membership of %q from role %q: %w", group, role, err)
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

func ensureDatabase(ctx context.Context, conn *pgx.Conn, adminURL string, database config.Database, defaultOwner string) error {
	if database.Name == "" {
		return fmt.Errorf("database name is required")
	}
	name := string(database.Name)
	owner := string(database.Owner)
	if owner == "" {
		owner = defaultOwner
	}

	var exists bool
	if err := conn.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check database %q: %w", name, err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, "create database "+identifier(name)+" owner "+identifier(owner)); err != nil {
			return fmt.Errorf("create database %q: %w", name, err)
		}
	} else {
		var currentOwner string
		if err := conn.QueryRow(ctx, "select pg_get_userbyid(datdba) from pg_database where datname = $1", name).Scan(&currentOwner); err != nil {
			return fmt.Errorf("get owner of database %q: %w", name, err)
		}
		if currentOwner != owner {
			if _, err := conn.Exec(ctx, "alter database "+identifier(name)+" owner to "+identifier(owner)); err != nil {
				return fmt.Errorf("set owner of database %q to %q: %w", name, owner, err)
			}
		}
	}

	targetURL := databaseURL(adminURL, name)
	databaseConn, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		return fmt.Errorf("connect to database %q: %w", name, err)
	}
	defer databaseConn.Close(ctx)

	for _, schema := range database.Schemas {
		if schema == "" {
			return fmt.Errorf("database %q has an empty schema", name)
		}
		statement := "create schema if not exists " + identifier(string(schema)) + " authorization " + identifier(owner)
		if _, err := databaseConn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create schema %q in database %q: %w", schema, name, err)
		}
		currentOwner, err := schemaOwner(ctx, databaseConn, name, string(schema))
		if err != nil {
			return err
		}
		if currentOwner != owner {
			statement = "alter schema " + identifier(string(schema)) + " owner to " + identifier(owner)
			if _, err := databaseConn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("set owner of schema %q in database %q to %q: %w", schema, name, owner, err)
			}
		}
	}
	for _, extension := range database.Extensions {
		if extension == "" {
			return fmt.Errorf("database %q has an empty extension", name)
		}
		if _, err := databaseConn.Exec(ctx, "create extension if not exists "+identifier(string(extension))); err != nil {
			return fmt.Errorf("create extension %q in database %q: %w", extension, name, err)
		}
	}
	return nil
}

// applyGrants grants a permission over the objects that exist right now.
func applyGrants(ctx context.Context, adminURL, role string, permission config.Permission) error {
	return eachPermissionSchema(ctx, adminURL, permission, func(conn *pgx.Conn, database, schema string) error {
		if len(permission.Grants) != 0 {
			statement := "grant " + strings.Join(permission.Grants, ", ") + " on schema " + identifier(schema) + " to " + identifier(role)
			if _, err := conn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("grant on schema %q in database %q: %w", schema, database, err)
			}
		}
		for _, class := range objectClasses(permission) {
			statement := "grant " + class.grants + " on all " + class.plural + " in schema " + identifier(schema) + " to " + identifier(role)
			if _, err := conn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("grant on %s in schema %q in database %q: %w", class.plural, schema, database, err)
			}
		}
		return nil
	})
}

// objectClass pairs the privileges a permission asks for with the PostgreSQL
// keyword naming the object class they apply to.
type objectClass struct {
	plural string
	grants string
}

// objectClasses lists the non-empty object-class grants of a permission. Each
// class is declared outright rather than inferred from another, so the YAML
// states every privilege PostgreSQL will be asked to hand out.
func objectClasses(permission config.Permission) []objectClass {
	classes := make([]objectClass, 0, 3)
	for _, class := range []struct {
		plural string
		grants []string
	}{
		{"tables", permission.TableGrants},
		{"sequences", permission.SequenceGrants},
		{"routines", permission.RoutineGrants},
	} {
		if len(class.grants) != 0 {
			classes = append(classes, objectClass{class.plural, strings.Join(class.grants, ", ")})
		}
	}
	return classes
}

// applyDefaultPrivileges mirrors a permission onto every role that can create
// objects in the schema. That is what makes a grant in this config mean the same
// thing whenever it is applied relative to the objects it covers: whoever ends
// up creating an object, the grant is already waiting for it.
func applyDefaultPrivileges(ctx context.Context, adminURL, role string, permission config.Permission, creators map[schemaKey][]string) error {
	return eachPermissionSchema(ctx, adminURL, permission, func(conn *pgx.Conn, database, schema string) error {
		for _, creator := range creators[schemaKey{database, schema}] {
			// Objects a role creates are owned by it outright, so it needs no
			// default privilege pointing back at itself.
			if creator == role {
				continue
			}
			prefix := "alter default privileges for role " + identifier(creator) + " in schema " + identifier(schema) + " grant "
			for _, class := range objectClasses(permission) {
				if _, err := conn.Exec(ctx, prefix+class.grants+" on "+class.plural+" to "+identifier(role)); err != nil {
					return fmt.Errorf("grant default %s privileges in schema %q in database %q: %w", class.plural, schema, database, err)
				}
			}
		}
		return nil
	})
}

// eachPermissionSchema resolves the schemas a permission covers - the one it
// names, or every non-system schema when it names none - and runs apply against
// each of them on a single connection to the permission's database.
func eachPermissionSchema(ctx context.Context, adminURL string, permission config.Permission, apply func(conn *pgx.Conn, database, schema string) error) error {
	if err := permission.Validate(); err != nil {
		return err
	}

	database := string(permission.Database)
	conn, err := pgx.Connect(ctx, databaseURL(adminURL, database))
	if err != nil {
		return fmt.Errorf("connect to permission database %q: %w", database, err)
	}
	defer conn.Close(ctx)

	schemas := []string{string(permission.Schema)}
	if permission.Schema == "" {
		schemas, err = nonSystemSchemas(ctx, conn, database)
		if err != nil {
			return err
		}
	}
	for _, schema := range schemas {
		if err := apply(conn, database, schema); err != nil {
			return err
		}
	}
	return nil
}

func nonSystemSchemas(ctx context.Context, conn *pgx.Conn, database string) ([]string, error) {
	rows, err := conn.Query(ctx, "select nspname from pg_namespace where nspname <> 'information_schema' and nspname not like 'pg_%'")
	if err != nil {
		return nil, fmt.Errorf("list schemas in database %q: %w", database, err)
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, fmt.Errorf("scan schema: %w", err)
		}
		schemas = append(schemas, schema)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schemas: %w", err)
	}
	return schemas, nil
}

// schemaOwner reports the role that effectively owns a schema. Schemas owned by
// the implicit pg_database_owner role - which is how PostgreSQL 15 and later
// leave the public schema - resolve to the current owner of the database, since
// that is the role that owns the objects created in them.
func schemaOwner(ctx context.Context, conn *pgx.Conn, database, schema string) (string, error) {
	const query = `select case
	when pg_namespace.nspowner = 'pg_database_owner'::regrole
	then (select pg_get_userbyid(datdba) from pg_database where datname = current_database())
	else pg_get_userbyid(pg_namespace.nspowner)
end from pg_namespace where nspname = $1`

	var owner string
	if err := conn.QueryRow(ctx, query, schema).Scan(&owner); err != nil {
		return "", fmt.Errorf("get owner of schema %q in database %q: %w", schema, database, err)
	}
	return owner, nil
}

func socketURL(user, database string, connection config.ConnectionOptions) string {
	if user == "" {
		user = "postgres"
	}
	url := &url.URL{Scheme: "postgres", User: url.User(user), Path: "/" + database}
	query := url.Query()
	query.Set("host", connection.SocketDirectory)
	query.Set("port", connection.Port)
	url.RawQuery = query.Encode()
	return url.String()
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
