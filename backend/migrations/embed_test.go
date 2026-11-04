package migrations

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var fileName = regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.(up|down)\.sql$`)

// Every embedded file is a well-named migration and every version has both directions.
func TestEmbeddedMigrationsArePaired(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]map[string]bool{}
	for _, e := range entries {
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("unexpected embedded file %q", e.Name())
			continue
		}
		if dirs[m[1]] == nil {
			dirs[m[1]] = map[string]bool{}
		}
		dirs[m[1]][m[2]] = true
	}
	if len(dirs) == 0 {
		t.Fatal("no migrations embedded")
	}
	for v, d := range dirs {
		if !d["up"] || !d["down"] {
			t.Errorf("version %s: up=%v down=%v, want both", v, d["up"], d["down"])
		}
	}
}

// The schema forbids CASCADE drops and TRUNCATE in migrations (data-retention invariant 6).
func TestMigrationsAvoidCascadeAndTruncate(t *testing.T) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var code strings.Builder
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			code.WriteString(strings.ToUpper(line))
			code.WriteByte('\n')
		}
		s := code.String()
		if strings.Contains(s, "TRUNCATE") {
			t.Errorf("%s uses TRUNCATE", e.Name())
		}
		if strings.Contains(s, "DROP") && strings.Contains(s, "CASCADE;") {
			t.Errorf("%s uses DROP ... CASCADE", e.Name())
		}
	}
}
