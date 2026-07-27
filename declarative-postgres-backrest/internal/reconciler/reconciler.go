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

	return reconcilePermissions(ctx, adminURL, cfg, roleNames)
}

// schemaKey identifies one schema within one database.
type schemaKey struct {
	database string
	schema   string
}

// roleSchema identifies one role's privileges in one schema of one database.
type roleSchema struct {
	role     string
	database string
	schema   string
}

// defaultKey identifies the default privileges one creator's new objects of one
// class hand to one role in one schema.
type defaultKey struct {
	role     string
	database string
	schema   string
	creator  string
	class    string
}

// grantState is the privileges held on each object class, rendered as sorted
// comma-separated text so two states compare as plain strings. The empty string
// means the role holds nothing on that class.
type grantState struct {
	schema    string
	tables    string
	sequences string
	routines  string
}

// classState is what a role holds on one object class in one schema, plus
// whether every object of that class agrees. Objects that disagree mean the
// schema has drifted whatever the privileges happen to be.
type classState struct {
	privileges string
	uniform    bool
}

// objectClasses names the three grantable classes as PostgreSQL spells them in
// GRANT, alongside the pg_default_acl code for the same class.
var grantClasses = []struct {
	plural  string
	defacl  string
	extract func(grantState) string
}{
	{"tables", "r", func(state grantState) string { return state.tables }},
	{"sequences", "S", func(state grantState) string { return state.sequences }},
	{"routines", "f", func(state grantState) string { return state.routines }},
}

// connector hands out one reusable connection per database. Reconciliation is
// strictly sequential, so a cache is all a pool would amount to here.
type connector struct {
	ctx      context.Context
	adminURL string
	open     map[string]*pgx.Conn
}

func newConnector(ctx context.Context, adminURL string) *connector {
	return &connector{ctx: ctx, adminURL: adminURL, open: map[string]*pgx.Conn{}}
}

func (c *connector) get(database string) (*pgx.Conn, error) {
	if conn, exists := c.open[database]; exists {
		return conn, nil
	}
	conn, err := pgx.Connect(c.ctx, databaseURL(c.adminURL, database))
	if err != nil {
		return nil, fmt.Errorf("connect to permission database %q: %w", database, err)
	}
	c.open[database] = conn
	return conn, nil
}

func (c *connector) closeAll() {
	for _, conn := range c.open {
		conn.Close(context.Background())
	}
}

// reconcilePermissions brings every declared role's privileges in line with the
// configuration. It first asks whether anything actually differs; when nothing
// does the whole phase is read-only, which is the common case on a restart with
// an unchanged file. When something has drifted it falls back to revoking and
// re-granting outright, which converges from any state without this code having
// to reproduce PostgreSQL's ACL semantics.
func reconcilePermissions(ctx context.Context, adminURL string, cfg config.Config, roleNames []string) error {
	databases := permissionDatabases(cfg)
	conns := newConnector(ctx, adminURL)
	defer conns.closeAll()

	creators, owners, err := snapshotSchemas(ctx, conns, databases)
	if err != nil {
		return err
	}
	desired := desiredPrivileges(cfg, roleNames, schemasByDatabase(creators))

	drifted, err := hasDrift(ctx, conns, databases, roleNames, desired, creators, owners)
	if err != nil {
		return err
	}
	if !drifted {
		return nil
	}

	if err := rewritePrivileges(ctx, conns, databases, roleNames, desired, creators, owners); err != nil {
		return err
	}

	// Every CREATE grant the config asks for has now been applied, so the creator
	// set is final and default privileges can be pinned to it.
	finalCreators, _, err := snapshotSchemas(ctx, conns, databases)
	if err != nil {
		return err
	}
	return writeDefaultPrivileges(ctx, conns, databases, roleNames, desired, finalCreators, owners)
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

// snapshotSchemas records, for every schema of every given database, the roles
// whose newly created objects would pick up default privileges there and the
// role that owns the schema.
func snapshotSchemas(ctx context.Context, conns *connector, databases []string) (map[schemaKey][]string, map[schemaKey]string, error) {
	creators := make(map[schemaKey][]string)
	owners := make(map[schemaKey]string)
	for _, database := range databases {
		conn, err := conns.get(database)
		if err != nil {
			return nil, nil, err
		}
		schemas, err := nonSystemSchemas(ctx, conn, database)
		if err != nil {
			return nil, nil, err
		}
		for _, schema := range schemas {
			roles, err := schemaCreators(ctx, conn, database, schema)
			if err != nil {
				return nil, nil, err
			}
			owner, err := schemaOwner(ctx, conn, database, schema)
			if err != nil {
				return nil, nil, err
			}
			creators[schemaKey{database, schema}] = roles
			owners[schemaKey{database, schema}] = owner
		}
	}
	return creators, owners, nil
}

func schemasByDatabase(creators map[schemaKey][]string) map[string][]string {
	byDatabase := map[string][]string{}
	for key := range creators {
		byDatabase[key.database] = append(byDatabase[key.database], key.schema)
	}
	for _, schemas := range byDatabase {
		sort.Strings(schemas)
	}
	return byDatabase
}

// desiredPrivileges resolves the configuration into the privileges every role
// should hold in every schema. A role can cover one schema from more than one
// permission, so the declarations are unioned rather than the last one winning,
// which is what revoking and re-granting in file order produces anyway.
func desiredPrivileges(cfg config.Config, roleNames []string, schemasByDB map[string][]string) map[roleSchema]grantState {
	type sets struct{ schema, tables, sequences, routines map[string]struct{} }
	union := map[roleSchema]*sets{}

	for index, name := range roleNames {
		if index >= len(cfg.Roles) {
			break
		}
		for _, permission := range cfg.Roles[index].Permissions {
			database := string(permission.Database)
			schemas := []string{string(permission.Schema)}
			if permission.Schema == "" {
				schemas = schemasByDB[database]
			}
			grants := permission.ExpandedGrants()
			for _, schema := range schemas {
				key := roleSchema{name, database, schema}
				entry := union[key]
				if entry == nil {
					entry = &sets{map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}}
					union[key] = entry
				}
				for _, item := range []struct {
					into map[string]struct{}
					from []string
				}{
					{entry.schema, grants.Schema},
					{entry.tables, grants.Tables},
					{entry.sequences, grants.Sequences},
					{entry.routines, grants.Routines},
				} {
					for _, privilege := range item.from {
						item.into[privilege] = struct{}{}
					}
				}
			}
		}
	}

	desired := make(map[roleSchema]grantState, len(union))
	for key, entry := range union {
		desired[key] = grantState{
			schema:    renderPrivileges(entry.schema),
			tables:    renderPrivileges(entry.tables),
			sequences: renderPrivileges(entry.sequences),
			routines:  renderPrivileges(entry.routines),
		}
	}
	return desired
}

// renderPrivileges sorts a privilege set into the same shape the catalog
// queries produce, so the two can be compared as plain strings.
func renderPrivileges(set map[string]struct{}) string {
	if len(set) == 0 {
		return ""
	}
	ordered := make([]string, 0, len(set))
	for privilege := range set {
		ordered = append(ordered, privilege)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, ",")
}

const schemaACLQuery = `select n.nspname, r.rolname,
	coalesce(array_to_string(array(
		select a.privilege_type from aclexplode(n.nspacl) a
		where a.grantee = r.oid order by 1), ','), '')
from pg_namespace n
cross join pg_roles r
where n.nspname <> 'information_schema' and n.nspname not like 'pg\_%'
	and r.rolname = any($1::text[])`

const relationACLQuery = `select n.nspname, r.rolname,
	count(distinct held.privileges), min(held.privileges)
from pg_namespace n
cross join pg_roles r
join pg_class c on c.relnamespace = n.oid and c.relkind = any($2::"char"[])
cross join lateral (
	select array_to_string(array(
		select a.privilege_type from aclexplode(c.relacl) a
		where a.grantee = r.oid order by 1), ',') as privileges
) held
where n.nspname <> 'information_schema' and n.nspname not like 'pg\_%'
	and r.rolname = any($1::text[])
group by n.nspname, r.rolname`

const routineACLQuery = `select n.nspname, r.rolname,
	count(distinct held.privileges), min(held.privileges)
from pg_namespace n
cross join pg_roles r
join pg_proc p on p.pronamespace = n.oid
cross join lateral (
	select array_to_string(array(
		select a.privilege_type from aclexplode(p.proacl) a
		where a.grantee = r.oid order by 1), ',') as privileges
) held
where n.nspname <> 'information_schema' and n.nspname not like 'pg\_%'
	and r.rolname = any($1::text[])
group by n.nspname, r.rolname`

const defaultACLQuery = `select n.nspname, r.rolname, d.defaclobjtype::text, owner.rolname,
	array_to_string(array(
		select a.privilege_type from aclexplode(d.defaclacl) a
		where a.grantee = r.oid order by 1), ',')
from pg_default_acl d
join pg_namespace n on n.oid = d.defaclnamespace
join pg_roles owner on owner.oid = d.defaclrole
cross join pg_roles r
where r.rolname = any($1::text[])`

// readClassState returns what each role holds on one object class in each
// schema, and whether every object of that class agrees.
func readClassState(ctx context.Context, conn *pgx.Conn, database, query string, roles []string, relkinds []string) (map[roleSchema]classState, error) {
	arguments := []any{roles}
	if relkinds != nil {
		arguments = append(arguments, relkinds)
	}
	rows, err := conn.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("read privileges in database %q: %w", database, err)
	}
	defer rows.Close()

	state := map[roleSchema]classState{}
	for rows.Next() {
		var schema, role, privileges string
		var variants int
		if err := rows.Scan(&schema, &role, &variants, &privileges); err != nil {
			return nil, fmt.Errorf("scan privileges in database %q: %w", database, err)
		}
		state[roleSchema{role, database, schema}] = classState{privileges, variants <= 1}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate privileges in database %q: %w", database, err)
	}
	return state, nil
}

func readSchemaState(ctx context.Context, conn *pgx.Conn, database string, roles []string) (map[roleSchema]classState, error) {
	rows, err := conn.Query(ctx, schemaACLQuery, roles)
	if err != nil {
		return nil, fmt.Errorf("read schema privileges in database %q: %w", database, err)
	}
	defer rows.Close()

	state := map[roleSchema]classState{}
	for rows.Next() {
		var schema, role, privileges string
		if err := rows.Scan(&schema, &role, &privileges); err != nil {
			return nil, fmt.Errorf("scan schema privileges in database %q: %w", database, err)
		}
		state[roleSchema{role, database, schema}] = classState{privileges, true}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema privileges in database %q: %w", database, err)
	}
	return state, nil
}

func readDefaultState(ctx context.Context, conn *pgx.Conn, database string, roles []string) (map[defaultKey]string, error) {
	rows, err := conn.Query(ctx, defaultACLQuery, roles)
	if err != nil {
		return nil, fmt.Errorf("read default privileges in database %q: %w", database, err)
	}
	defer rows.Close()

	state := map[defaultKey]string{}
	for rows.Next() {
		var schema, role, class, creator, privileges string
		if err := rows.Scan(&schema, &role, &class, &creator, &privileges); err != nil {
			return nil, fmt.Errorf("scan default privileges in database %q: %w", database, err)
		}
		// Entries naming no privilege for this role are the same as no entry.
		if privileges == "" {
			continue
		}
		state[defaultKey{role, database, schema, creator, class}] = privileges
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate default privileges in database %q: %w", database, err)
	}
	return state, nil
}

// hasDrift reports whether the cluster already holds exactly what the
// configuration asks for. It is deliberately one-directional: a false answer
// means the rewrite runs and converges anyway, so anything this check cannot
// account for has to read as drift rather than as a match.
func hasDrift(ctx context.Context, conns *connector, databases, roleNames []string,
	desired map[roleSchema]grantState, creators map[schemaKey][]string, owners map[schemaKey]string) (bool, error) {

	for _, database := range databases {
		conn, err := conns.get(database)
		if err != nil {
			return false, err
		}

		schemaState, err := readSchemaState(ctx, conn, database, roleNames)
		if err != nil {
			return false, err
		}
		tableState, err := readClassState(ctx, conn, database, relationACLQuery, roleNames, []string{"r", "v", "m", "p", "f"})
		if err != nil {
			return false, err
		}
		sequenceState, err := readClassState(ctx, conn, database, relationACLQuery, roleNames, []string{"S"})
		if err != nil {
			return false, err
		}
		routineState, err := readClassState(ctx, conn, database, routineACLQuery, roleNames, nil)
		if err != nil {
			return false, err
		}
		defaultState, err := readDefaultState(ctx, conn, database, roleNames)
		if err != nil {
			return false, err
		}

		for key, owner := range owners {
			if key.database != database {
				continue
			}
			for _, role := range roleNames {
				target := roleSchema{role, database, key.schema}
				want := desired[target]

				// A role that owns the schema keeps its rights: the rewrite skips
				// it entirely, so there is nothing to compare. Declaring grants on
				// a schema the role owns is the one case that cannot be settled
				// from the catalog, so it reads as drift.
				if owner == role {
					if want != (grantState{}) {
						return true, nil
					}
					continue
				}

				if schemaState[target].privileges != want.schema {
					return true, nil
				}
				for _, class := range []struct {
					state map[roleSchema]classState
					want  string
				}{
					{tableState, want.tables},
					{sequenceState, want.sequences},
					{routineState, want.routines},
				} {
					// No row means the schema holds no object of that class, so
					// there is nothing a grant could apply to either way.
					held, present := class.state[target]
					if !present {
						continue
					}
					if !held.uniform || held.privileges != class.want {
						return true, nil
					}
				}
			}
		}

		if defaultsDiffer(database, roleNames, desired, creators, owners, defaultState) {
			return true, nil
		}
	}
	return false, nil
}

// defaultsDiffer compares pg_default_acl against what the configuration implies
// in both directions, so an entry left behind by a role that has since dropped
// out of the file counts as drift just as a missing one does.
func defaultsDiffer(database string, roleNames []string, desired map[roleSchema]grantState,
	creators map[schemaKey][]string, owners map[schemaKey]string, actual map[defaultKey]string) bool {

	want := map[defaultKey]string{}
	for key, schemaCreators := range creators {
		if key.database != database {
			continue
		}
		for _, role := range roleNames {
			if owners[key] == role {
				continue
			}
			state := desired[roleSchema{role, database, key.schema}]
			for _, creator := range schemaCreators {
				// Objects a role creates are owned by it outright, so it needs no
				// default privilege pointing back at itself.
				if creator == role {
					continue
				}
				for _, class := range grantClasses {
					if privileges := class.extract(state); privileges != "" {
						want[defaultKey{role, database, key.schema, creator, class.defacl}] = privileges
					}
				}
			}
		}
	}

	if len(want) != len(actual) {
		return true
	}
	for key, privileges := range want {
		if actual[key] != privileges {
			return true
		}
	}
	return false
}

// rewritePrivileges revokes everything this configuration could have granted a
// role and then grants back what the file asks for. Each role's rewrite in one
// database runs as a single transaction, so a client never observes the moment
// in between: PostgreSQL takes no more than an AccessShareLock for this, which
// does not conflict with reads or writes against the same tables.
func rewritePrivileges(ctx context.Context, conns *connector, databases, roleNames []string,
	desired map[roleSchema]grantState, creators map[schemaKey][]string, owners map[schemaKey]string) error {

	for _, database := range databases {
		conn, err := conns.get(database)
		if err != nil {
			return err
		}
		schemas := schemasByDatabase(creators)[database]
		for _, role := range roleNames {
			if err := withTransaction(ctx, conn, func(tx pgx.Tx) error {
				for _, schema := range schemas {
					key := schemaKey{database, schema}
					// A role that owns the schema keeps its rights: they come from
					// ownership rather than from anything this config granted.
					if owners[key] == role {
						continue
					}
					if err := revokeSchemaPrivileges(ctx, tx, role, schema, creators[key]); err != nil {
						return err
					}
					state := desired[roleSchema{role, database, schema}]
					if state.schema != "" {
						if _, err := tx.Exec(ctx, "grant "+state.schema+" on schema "+identifier(schema)+" to "+identifier(role)); err != nil {
							return err
						}
					}
					for _, class := range grantClasses {
						privileges := class.extract(state)
						if privileges == "" {
							continue
						}
						if _, err := tx.Exec(ctx, "grant "+privileges+" on all "+class.plural+" in schema "+identifier(schema)+" to "+identifier(role)); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				return fmt.Errorf("reconcile permissions for role %q in database %q: %w", role, database, err)
			}
		}
	}
	return nil
}

// writeDefaultPrivileges mirrors each permission onto every role that can create
// objects in the schema. That is what makes a grant in this config mean the same
// thing whenever it is applied relative to the objects it covers: whoever ends
// up creating an object, the grant is already waiting for it.
func writeDefaultPrivileges(ctx context.Context, conns *connector, databases, roleNames []string,
	desired map[roleSchema]grantState, creators map[schemaKey][]string, owners map[schemaKey]string) error {

	for _, database := range databases {
		conn, err := conns.get(database)
		if err != nil {
			return err
		}
		schemas := schemasByDatabase(creators)[database]
		for _, role := range roleNames {
			if err := withTransaction(ctx, conn, func(tx pgx.Tx) error {
				for _, schema := range schemas {
					key := schemaKey{database, schema}
					if owners[key] == role {
						continue
					}
					state := desired[roleSchema{role, database, schema}]
					for _, creator := range creators[key] {
						if creator == role {
							continue
						}
						prefix := "alter default privileges for role " + identifier(creator) + " in schema " + identifier(schema) + " grant "
						for _, class := range grantClasses {
							privileges := class.extract(state)
							if privileges == "" {
								continue
							}
							if _, err := tx.Exec(ctx, prefix+privileges+" on "+class.plural+" to "+identifier(role)); err != nil {
								return err
							}
						}
					}
				}
				return nil
			}); err != nil {
				return fmt.Errorf("reconcile default privileges for role %q in database %q: %w", role, database, err)
			}
		}
	}
	return nil
}

func withTransaction(ctx context.Context, conn *pgx.Conn, apply func(pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if err := apply(tx); err != nil {
		tx.Rollback(context.Background())
		return err
	}
	return tx.Commit(ctx)
}

// hasRoutineGrants reports whether a role holds an explicit privilege on any
// routine in a schema. Routines with a null proacl are left at the PostgreSQL
// default, which names no role directly and so cannot need revoking.
func hasRoutineGrants(ctx context.Context, tx pgx.Tx, schema, role string) (bool, error) {
	const query = `select exists(
	select 1 from pg_proc
	join pg_namespace on pg_namespace.oid = pg_proc.pronamespace
	where pg_namespace.nspname = $1 and pg_proc.proacl is not null
		and exists(select 1 from aclexplode(pg_proc.proacl) where grantee = $2::regrole)
)`

	var granted bool
	if err := tx.QueryRow(ctx, query, schema, role).Scan(&granted); err != nil {
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

// revokeSchemaPrivileges strips everything this config could have granted a role
// in one schema, so that what the YAML no longer asks for genuinely disappears.
func revokeSchemaPrivileges(ctx context.Context, tx pgx.Tx, role, schema string, creators []string) error {
	for _, class := range []string{"schema " + identifier(schema), "all tables in schema " + identifier(schema),
		"all sequences in schema " + identifier(schema)} {
		if _, err := tx.Exec(ctx, "revoke all privileges on "+class+" from "+identifier(role)); err != nil {
			return err
		}
	}
	// Functions carry an implicit EXECUTE for PUBLIC, and revoking from a role
	// that holds nothing still rewrites every function's ACL to spell that
	// default out. Behaviour would be unchanged, but the catalog and every
	// pg_dump after it would be littered with grants nobody asked for, so the
	// revoke only runs when there is genuinely something to take away.
	granted, err := hasRoutineGrants(ctx, tx, schema, role)
	if err != nil {
		return err
	}
	if granted {
		if _, err := tx.Exec(ctx, "revoke all privileges on all routines in schema "+identifier(schema)+" from "+identifier(role)); err != nil {
			return err
		}
	}
	for _, creator := range creators {
		prefix := "alter default privileges for role " + identifier(creator) + " in schema " + identifier(schema) + " revoke all on "
		for _, class := range []string{"tables", "sequences", "routines"} {
			if _, err := tx.Exec(ctx, prefix+class+" from "+identifier(role)); err != nil {
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
