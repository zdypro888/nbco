package store

import (
	"io/fs"
	"regexp"
	"testing"
)

func TestMigrationNumericPrefixesUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^([0-9]{4})_`)
	seen := map[string]string{}
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			t.Fatalf("migration %s must start with a four digit prefix", e.Name())
		}
		if prev := seen[m[1]]; prev != "" {
			t.Fatalf("duplicate migration prefix %s: %s and %s", m[1], prev, e.Name())
		}
		seen[m[1]] = e.Name()
	}
}
