package reconciler

import (
	"slices"
	"testing"

	"github.com/jptrs93/container-images/declaritive-postgres/internal/config"
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

func TestObjectClasses(t *testing.T) {
	for name, testCase := range map[string]struct {
		permission config.Permission
		want       []objectClass
	}{
		"tables only": {
			config.Permission{TableGrants: []string{"SELECT", "INSERT"}},
			[]objectClass{{"tables", "SELECT, INSERT"}},
		},
		"every class": {
			config.Permission{TableGrants: []string{"ALL"}, SequenceGrants: []string{"USAGE", "SELECT"}, RoutineGrants: []string{"EXECUTE"}},
			[]objectClass{{"tables", "ALL"}, {"sequences", "USAGE, SELECT"}, {"routines", "EXECUTE"}},
		},
		"sequences without tables": {
			config.Permission{SequenceGrants: []string{"USAGE"}},
			[]objectClass{{"sequences", "USAGE"}},
		},
		"schema grants only": {
			config.Permission{Grants: []string{"USAGE"}},
			nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := objectClasses(testCase.permission)
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("objectClasses() = %v, want %v", got, testCase.want)
			}
		})
	}
}
