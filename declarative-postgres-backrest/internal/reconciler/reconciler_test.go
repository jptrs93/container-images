package reconciler

import (
	"slices"
	"testing"

	"github.com/jptrs93/container-images/declarative-postgres-backrest/internal/config"
)

func TestPermissionDatabases(t *testing.T) {
	cfg := config.Config{
		Databases: []config.Database{{Name: "app"}, {Name: "analytics"}},
		Roles: []config.Role{
			{Name: "reader", Permissions: []config.Permission{{Database: "app"}, {Database: "reporting"}}},
			{Name: "writer", Permissions: []config.Permission{{Database: "app"}}},
		},
	}
	want := []string{"analytics", "app", "reporting"}
	if got := permissionDatabases(cfg); !slices.Equal(got, want) {
		t.Fatalf("permissionDatabases() = %v, want %v", got, want)
	}
}

func TestSchemasByDatabase(t *testing.T) {
	creators := map[schemaKey][]string{
		{"app", "public"}:   {"postgres"},
		{"app", "staging"}:  {"postgres"},
		{"other", "public"}: {"postgres"},
	}
	got := schemasByDatabase(creators)
	if !slices.Equal(got["app"], []string{"public", "staging"}) {
		t.Fatalf("schemasByDatabase()[app] = %v", got["app"])
	}
	if !slices.Equal(got["other"], []string{"public"}) {
		t.Fatalf("schemasByDatabase()[other] = %v", got["other"])
	}
}

// The comparison against the catalog is a string match, so ALL has to expand to
// exactly the privilege list PostgreSQL records in an ACL, in sorted order.
func TestDesiredPrivilegesExpandsAll(t *testing.T) {
	cfg := config.Config{Roles: []config.Role{{
		Name: "app_user",
		Permissions: []config.Permission{{
			Database: "app", Schema: "public", TableGrants: []string{"ALL"},
		}},
	}}}
	desired := desiredPrivileges(cfg, []string{"app_user"}, map[string][]string{"app": {"public"}})
	got := desired[roleSchema{"app_user", "app", "public"}]
	want := "DELETE,INSERT,MAINTAIN,REFERENCES,SELECT,TRIGGER,TRUNCATE,UPDATE"
	if got.tables != want {
		t.Fatalf("tables = %q, want %q", got.tables, want)
	}
}

// An omitted schema covers every non-system schema in the database.
func TestDesiredPrivilegesWildcardSchema(t *testing.T) {
	cfg := config.Config{Roles: []config.Role{{
		Name:        "analyst",
		Permissions: []config.Permission{{Database: "app", TableGrants: []string{"SELECT"}}},
	}}}
	desired := desiredPrivileges(cfg, []string{"analyst"}, map[string][]string{"app": {"public", "staging"}})
	for _, schema := range []string{"public", "staging"} {
		if got := desired[roleSchema{"analyst", "app", schema}].tables; got != "SELECT" {
			t.Fatalf("tables in %q = %q, want SELECT", schema, got)
		}
	}
}

// Two permissions covering one schema union rather than the last one winning,
// which is what revoking and re-granting in file order produces anyway.
func TestDesiredPrivilegesUnionsPermissions(t *testing.T) {
	cfg := config.Config{Roles: []config.Role{{
		Name: "app_user",
		Permissions: []config.Permission{
			{Database: "app", Schema: "public", TableGrants: []string{"SELECT"}},
			{Database: "app", TableGrants: []string{"INSERT"}, SequenceGrants: []string{"USAGE"}},
		},
	}}}
	desired := desiredPrivileges(cfg, []string{"app_user"}, map[string][]string{"app": {"public"}})
	got := desired[roleSchema{"app_user", "app", "public"}]
	if got.tables != "INSERT,SELECT" {
		t.Fatalf("tables = %q, want INSERT,SELECT", got.tables)
	}
	if got.sequences != "USAGE" {
		t.Fatalf("sequences = %q, want USAGE", got.sequences)
	}
}

func TestDefaultsDiffer(t *testing.T) {
	creators := map[schemaKey][]string{{"app", "public"}: {"app_owner", "postgres"}}
	owners := map[schemaKey]string{{"app", "public"}: "app_owner"}
	desired := map[roleSchema]grantState{
		{"reader", "app", "public"}: {tables: "SELECT"},
	}
	matching := map[defaultKey]string{
		{"reader", "app", "public", "app_owner", "r"}: "SELECT",
		{"reader", "app", "public", "postgres", "r"}:  "SELECT",
	}

	for name, testCase := range map[string]struct {
		actual map[defaultKey]string
		want   bool
	}{
		"exact match":     {matching, false},
		"missing creator": {map[defaultKey]string{{"reader", "app", "public", "app_owner", "r"}: "SELECT"}, true},
		"wrong privilege": {map[defaultKey]string{
			{"reader", "app", "public", "app_owner", "r"}: "SELECT",
			{"reader", "app", "public", "postgres", "r"}:  "INSERT",
		}, true},
		"stale extra entry": {map[defaultKey]string{
			{"reader", "app", "public", "app_owner", "r"}: "SELECT",
			{"reader", "app", "public", "postgres", "r"}:  "SELECT",
			{"reader", "app", "public", "postgres", "S"}:  "USAGE",
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := defaultsDiffer("app", []string{"reader"}, desired, creators, owners, testCase.actual)
			if got != testCase.want {
				t.Fatalf("defaultsDiffer() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A role owning the schema is skipped by the rewrite, so it must also be skipped
// when deciding which default privileges should exist.
func TestDefaultsDifferSkipsSchemaOwner(t *testing.T) {
	creators := map[schemaKey][]string{{"app", "public"}: {"app_owner"}}
	owners := map[schemaKey]string{{"app", "public"}: "app_owner"}
	desired := map[roleSchema]grantState{{"app_owner", "app", "public"}: {tables: "SELECT"}}
	if defaultsDiffer("app", []string{"app_owner"}, desired, creators, owners, map[defaultKey]string{}) {
		t.Fatal("defaultsDiffer() = true for a schema the role owns, want false")
	}
}

// A creator never needs a default privilege pointing back at itself, because it
// owns what it creates outright.
func TestDefaultsDifferIgnoresSelfCreator(t *testing.T) {
	creators := map[schemaKey][]string{{"app", "public"}: {"builder"}}
	owners := map[schemaKey]string{{"app", "public"}: "postgres"}
	desired := map[roleSchema]grantState{{"builder", "app", "public"}: {tables: "SELECT"}}
	if defaultsDiffer("app", []string{"builder"}, desired, creators, owners, map[defaultKey]string{}) {
		t.Fatal("defaultsDiffer() = true for a role that is its own creator, want false")
	}
}

func TestRenderPrivileges(t *testing.T) {
	if got := renderPrivileges(map[string]struct{}{}); got != "" {
		t.Fatalf("renderPrivileges(empty) = %q, want empty", got)
	}
	got := renderPrivileges(map[string]struct{}{"USAGE": {}, "CREATE": {}})
	if got != "CREATE,USAGE" {
		t.Fatalf("renderPrivileges() = %q, want CREATE,USAGE", got)
	}
}
