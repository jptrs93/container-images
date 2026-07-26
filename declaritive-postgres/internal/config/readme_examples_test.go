package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestREADMEExamplesAreValid parses every YAML block in the README with the real
// loader, so a documented example cannot drift into something the image would
// reject at startup. Role and database names must also stay distinct, since an
// example that reuses one name for both reads ambiguously.
func TestREADMEExamplesAreValid(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(contents), -1)
	if len(blocks) == 0 {
		t.Fatal("no YAML examples found in README")
	}
	// Supply every environment reference the examples make that has no default,
	// so the test tracks the README rather than a hand-maintained list.
	references := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	for _, block := range blocks {
		for _, match := range references.FindAllStringSubmatch(block[1], -1) {
			t.Setenv(match[1], "example-value")
		}
	}

	for index, block := range blocks {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(block[1]), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("README example %d failed to load: %v\n%s", index, err, block[1])
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("README example %d failed to validate: %v\n%s", index, err, block[1])
		}
		roles := make(map[string]struct{}, len(cfg.Roles))
		for _, role := range cfg.Roles {
			roles[string(role.Name)] = struct{}{}
		}
		for _, database := range cfg.Databases {
			if _, clash := roles[string(database.Name)]; clash {
				t.Fatalf("README example %d names both a role and a database %q", index, database.Name)
			}
		}
		if strings.Contains(block[1], "\t") {
			t.Fatalf("README example %d contains a tab, which YAML forbids for indentation", index)
		}
	}
}
