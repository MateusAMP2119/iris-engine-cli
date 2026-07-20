package arch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// This file holds the dependency-allowlist and forbidden-anywhere checks. The
// allowlist is an upper bound on direct dependencies; the forbidden classes are
// banned anywhere in the module graph, direct or indirect.

// LoadGoMod reads and parses the go.mod at path.
func LoadGoMod(path string) (*GoMod, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the repo-controlled go.mod the structural gate reads, never user or network input.
	if err != nil {
		return nil, fmt.Errorf("arch: read go.mod %s: %w", path, err)
	}
	return ParseGoMod(data)
}

// LoadGoSum reads the go.sum at path and returns the distinct module paths it
// records: the fuller, transitive view of the module graph the forbidden ban
// sweeps alongside go.mod's own requires. A missing go.sum (a module with no
// dependencies yet) yields no paths and no error.
func LoadGoSum(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the repo-controlled go.sum the structural gate reads, never user or network input.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("arch: read go.sum %s: %w", path, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(string(data), "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		p := fields[0]
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// AllowedDirectDependency reports whether a direct-dependency module path is on
// the allowlist: the pgx driver, cobra plus its own flag-set library spf13/pflag
// (admitted for flag-set introspection, never as an independent CLI framework),
// goccy/go-yaml, an argon2id provider (github.com/alexedwards/argon2id or
// golang.org/x/crypto), the embedded-postgres supervisor, spf13/viper for
// engine config resolution, and the Charm stack for interactive surfaces
// (bubbletea, lipgloss, bubbles, huh). Everything else is off the allowlist;
// digests and signatures use only stdlib hashing and crypto/ed25519, which are
// never go.mod requires.
func AllowedDirectDependency(path string) bool {
	switch path {
	case "github.com/jackc/pgx",
		"github.com/spf13/cobra",
		"github.com/spf13/pflag",
		"github.com/spf13/viper",
		"github.com/goccy/go-yaml",
		"github.com/alexedwards/argon2id",
		"golang.org/x/crypto",
		"github.com/fergusstrange/embedded-postgres",
		// Raw terminal mode (still used by legacy paths and tests).
		"golang.org/x/term",
		// Charm interactive stack: live view + install/uninstall prompts.
		"github.com/charmbracelet/bubbletea",
		"github.com/charmbracelet/lipgloss",
		"github.com/charmbracelet/bubbles",
		"github.com/charmbracelet/huh":
		return true
	}
	// pgx is versioned as a subpath module (github.com/jackc/pgx/v5, .../v5/pgxpool).
	return strings.HasPrefix(path, "github.com/jackc/pgx/")
}

// CheckDirectAllowlist returns a violation for every direct require whose module
// path is not on the allowlist.
func CheckDirectAllowlist(g *GoMod) []Violation {
	var vs []Violation
	for _, r := range g.DirectRequires() {
		if !AllowedDirectDependency(r.Path) {
			vs = append(vs, Violation{
				Kind:    KindDependencyAllowlist,
				Subject: r.Path,
				Detail:  "direct dependency is not on the allowlist",
			})
		}
	}
	return vs
}

// forbiddenPrefixes maps a module-path prefix to the banned class it belongs to.
// A path matches when it equals the prefix or sits under it (prefix + "/"),
// mirroring how the go tool scopes a module path. SQLite and parquet are matched
// separately by substring, since their drivers and libraries are spread across
// many unrelated module roots.
var forbiddenPrefixes = []struct{ prefix, category string }{
	// ORMs.
	{"gorm.io", "orm"},
	{"github.com/jinzhu/gorm", "orm"},
	{"entgo.io/ent", "orm"},
	{"github.com/volatiletech/sqlboiler", "orm"},
	{"github.com/uptrace/bun", "orm"},
	{"xorm.io", "orm"},
	// Migration frameworks.
	{"github.com/golang-migrate/migrate", "migration"},
	{"github.com/pressly/goose", "migration"},
	{"github.com/rubenv/sql-migrate", "migration"},
	{"github.com/amacneil/dbmate", "migration"},
	// Scheduler libraries.
	{"github.com/robfig/cron", "scheduler"},
	{"github.com/go-co-op/gocron", "scheduler"},
	{"github.com/madflojo/tasks", "scheduler"},
	// Cloud object-store clients.
	{"github.com/aws/aws-sdk-go", "cloud-object-store"},
	{"github.com/aws/aws-sdk-go-v2", "cloud-object-store"},
	{"cloud.google.com/go/storage", "cloud-object-store"},
	{"github.com/Azure/azure-sdk-for-go", "cloud-object-store"},
	{"github.com/Azure/azure-storage-blob-go", "cloud-object-store"},
	{"gocloud.dev", "cloud-object-store"},
}

// ForbiddenModule reports whether a module path belongs to a class the engine
// bans anywhere in its graph -- SQLite driver, ORM, migration framework,
// scheduler, parquet library, or cloud object-store client -- and names the
// class.
func ForbiddenModule(path string) (category string, forbidden bool) {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "sqlite") {
		return "sqlite", true
	}
	if strings.Contains(lower, "parquet") {
		return "parquet", true
	}
	for _, f := range forbiddenPrefixes {
		if path == f.prefix || strings.HasPrefix(path, f.prefix+"/") {
			return f.category, true
		}
	}
	return "", false
}

// CheckForbiddenModules returns a violation for every distinct module path of a
// forbidden class.
func CheckForbiddenModules(paths []string) []Violation {
	var vs []Violation
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if cat, forbidden := ForbiddenModule(p); forbidden {
			vs = append(vs, Violation{
				Kind:    KindForbiddenModule,
				Subject: p,
				Detail:  fmt.Sprintf("%s dependency is banned anywhere in the module graph", cat),
			})
		}
	}
	return vs
}
