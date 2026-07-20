package arch_test

import (
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/arch"
)

// TestDependencyAllowlist proves the dependency discipline as static analysis
// over go.mod and go.sum: the direct-dependency allowlist (an upper bound: pgx,
// cobra plus its flag-set library pflag, goccy/go-yaml, an argon2id provider,
// embedded-postgres) and the forbidden-anywhere ban on ORMs, migration
// frameworks, schedulers, parquet libraries, cloud object-store clients, and
// SQLite drivers, anywhere in the module graph. It runs on synthetic go.mod
// snippets that plant violations, and then over the repo's real go.mod + go.sum,
// which must be clean.
func TestDependencyAllowlist(t *testing.T) {
	t.Run("allowlist membership", func(t *testing.T) {
		allowed := []string{
			"github.com/jackc/pgx/v5",
			"github.com/jackc/pgx/v5/pgxpool",
			"github.com/spf13/cobra",
			"github.com/spf13/pflag",
			"github.com/spf13/viper",
			"github.com/goccy/go-yaml",
			"github.com/alexedwards/argon2id",
			"golang.org/x/crypto",
			"github.com/fergusstrange/embedded-postgres",
			"golang.org/x/term",
			"github.com/charmbracelet/bubbletea",
			"github.com/charmbracelet/lipgloss",
			"github.com/charmbracelet/bubbles",
			"github.com/charmbracelet/huh",
		}
		for _, p := range allowed {
			if !arch.AllowedDirectDependency(p) {
				t.Errorf("AllowedDirectDependency(%q) = false, want true (on the allowlist)", p)
			}
		}
		denied := []string{
			"gorm.io/gorm",
			"github.com/golang-migrate/migrate/v4",
			"github.com/robfig/cron/v3",
			"github.com/mattn/go-sqlite3",
			"github.com/aws/aws-sdk-go-v2",
			"github.com/some/random-dep",
		}
		for _, p := range denied {
			if arch.AllowedDirectDependency(p) {
				t.Errorf("AllowedDirectDependency(%q) = true, want false (off the allowlist)", p)
			}
		}
	})

	t.Run("direct require off allowlist is caught", func(t *testing.T) {
		const src = `module example.com/m
go 1.26
require (
	github.com/goccy/go-yaml v1.19.2
	gorm.io/gorm v1.25.0
	golang.org/x/sys v0.20.0 // indirect
)
`
		g, err := arch.ParseGoMod([]byte(src))
		if err != nil {
			t.Fatalf("ParseGoMod: %v", err)
		}
		vs := arch.CheckDirectAllowlist(g)
		if len(vs) != 1 {
			t.Fatalf("CheckDirectAllowlist found %d violations, want 1: %v", len(vs), vs)
		}
		if vs[0].Subject != "gorm.io/gorm" {
			t.Errorf("violation subject = %q, want gorm.io/gorm", vs[0].Subject)
		}
		// The indirect golang.org/x/sys is not a direct require, so it is not an
		// allowlist violation (indirect deps are unbounded except by the ban).
		for _, v := range vs {
			if v.Subject == "golang.org/x/sys" {
				t.Errorf("indirect dep %q was flagged by the direct allowlist", v.Subject)
			}
		}
	})

	t.Run("all-allowlisted direct requires are clean", func(t *testing.T) {
		const src = `module example.com/m
go 1.26
require (
	github.com/jackc/pgx/v5 v5.6.0
	github.com/spf13/cobra v1.8.1
	github.com/goccy/go-yaml v1.19.2
	github.com/alexedwards/argon2id v1.0.0
	github.com/fergusstrange/embedded-postgres v1.27.0
	golang.org/x/sys v0.20.0 // indirect
)
`
		g, err := arch.ParseGoMod([]byte(src))
		if err != nil {
			t.Fatalf("ParseGoMod: %v", err)
		}
		if vs := arch.CheckDirectAllowlist(g); len(vs) != 0 {
			t.Errorf("CheckDirectAllowlist = %v, want none", vs)
		}
	})

	t.Run("forbidden classes caught anywhere in the graph", func(t *testing.T) {
		cases := map[string]string{
			"github.com/mattn/go-sqlite3":             "sqlite",
			"modernc.org/sqlite":                      "sqlite",
			"gorm.io/gorm":                            "orm",
			"entgo.io/ent":                            "orm",
			"github.com/golang-migrate/migrate/v4":    "migration",
			"github.com/pressly/goose/v3":             "migration",
			"github.com/robfig/cron/v3":               "scheduler",
			"github.com/xitongsys/parquet-go":         "parquet",
			"github.com/aws/aws-sdk-go-v2/service/s3": "cloud-object-store",
			"cloud.google.com/go/storage":             "cloud-object-store",
			"github.com/Azure/azure-sdk-for-go":       "cloud-object-store",
		}
		for path, wantCat := range cases {
			cat, forbidden := arch.ForbiddenModule(path)
			if !forbidden {
				t.Errorf("ForbiddenModule(%q) = not forbidden, want class %q", path, wantCat)
				continue
			}
			if cat != wantCat {
				t.Errorf("ForbiddenModule(%q) class = %q, want %q", path, cat, wantCat)
			}
		}
	})

	t.Run("allowlisted deps are never forbidden", func(t *testing.T) {
		for _, p := range []string{
			"github.com/jackc/pgx/v5",
			"github.com/spf13/cobra",
			"github.com/spf13/pflag",
			"github.com/goccy/go-yaml",
			"github.com/alexedwards/argon2id",
			"golang.org/x/crypto",
			"github.com/fergusstrange/embedded-postgres",
			"golang.org/x/sys",
		} {
			if cat, forbidden := arch.ForbiddenModule(p); forbidden {
				t.Errorf("ForbiddenModule(%q) = forbidden (%s); an allowed dep must never match a ban", p, cat)
			}
		}
	})

	t.Run("CheckForbiddenModules reports each banned path", func(t *testing.T) {
		paths := []string{
			"github.com/goccy/go-yaml",
			"github.com/mattn/go-sqlite3",
			"gorm.io/gorm",
			"golang.org/x/sys",
		}
		vs := arch.CheckForbiddenModules(paths)
		if len(vs) != 2 {
			t.Fatalf("CheckForbiddenModules found %d, want 2 (sqlite + orm): %v", len(vs), vs)
		}
		got := map[string]bool{}
		for _, v := range vs {
			got[v.Subject] = true
			if v.Kind != arch.KindForbiddenModule {
				t.Errorf("violation kind = %q, want %q", v.Kind, arch.KindForbiddenModule)
			}
		}
		for _, want := range []string{"github.com/mattn/go-sqlite3", "gorm.io/gorm"} {
			if !got[want] {
				t.Errorf("CheckForbiddenModules missed %q", want)
			}
		}
	})

	t.Run("real repo go.mod + go.sum are clean", func(t *testing.T) {
		root := filepath.Join("..", "..")
		g, err := arch.LoadGoMod(filepath.Join(root, "go.mod"))
		if err != nil {
			t.Fatalf("LoadGoMod: %v", err)
		}
		if vs := arch.CheckDirectAllowlist(g); len(vs) != 0 {
			t.Errorf("real go.mod has off-allowlist direct deps: %v", vs)
		}
		sumPaths, err := arch.LoadGoSum(filepath.Join(root, "go.sum"))
		if err != nil {
			t.Fatalf("LoadGoSum: %v", err)
		}
		all := append(g.AllRequirePaths(), sumPaths...)
		if vs := arch.CheckForbiddenModules(all); len(vs) != 0 {
			t.Errorf("real module graph contains a forbidden dependency: %v", vs)
		}
	})
}
