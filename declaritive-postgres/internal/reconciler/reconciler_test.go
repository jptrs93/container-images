package reconciler

import "testing"

func TestSequencePrivileges(t *testing.T) {
	for name, testCase := range map[string]struct {
		tableGrants []string
		want        string
	}{
		"insert implies usage":    {[]string{"SELECT", "INSERT"}, "USAGE"},
		"all implies all":         {[]string{"ALL"}, "ALL"},
		"read only implies none":  {[]string{"SELECT"}, ""},
		"write without insert":    {[]string{"UPDATE", "DELETE"}, ""},
		"no table grants":         {nil, ""},
		"insert with maintenance": {[]string{"INSERT", "TRUNCATE", "TRIGGER"}, "USAGE"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sequencePrivileges(testCase.tableGrants); got != testCase.want {
				t.Fatalf("sequencePrivileges(%v) = %q, want %q", testCase.tableGrants, got, testCase.want)
			}
		})
	}
}
