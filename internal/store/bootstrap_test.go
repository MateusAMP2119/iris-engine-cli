package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// countPrefix counts the statements beginning with prefix (after trimming leading
// whitespace).
func countPrefix(stmts []string, prefix string) int {
	n := 0
	for _, s := range stmts {
		if strings.HasPrefix(strings.TrimSpace(s), prefix) {
			n++
		}
	}
	return n
}

// TestBootstrapCreatesDedicatedMetaDatabase proves the engine bootstrap creates
// the dedicated meta database and, in its public schema, exactly the eighteen
// control tables with no warehouse schemas.
func TestBootstrapCreatesDedicatedMetaDatabase(t *testing.T) {
	ctx := context.Background()
	rec := storetest.NewRecorder()

	// One recorder stands in for both the admin/cluster connection (CREATE
	// DATABASE) and the freshly-created meta connection (the tables), capturing the
	// whole bootstrap inventory in order.
	if err := store.Bootstrap(ctx, rec, rec); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	stmts := rec.Statements()

	if n := countPrefix(stmts, "CREATE DATABASE "+store.MetaDatabase); n != 1 {
		t.Errorf("bootstrap issued %d CREATE DATABASE %s statements, want exactly 1", n, store.MetaDatabase)
	}
	if n := countPrefix(stmts, "CREATE TABLE"); n != len(metaRoster) {
		t.Errorf("bootstrap created %d tables, want %d (the meta control-table roster)", n, len(metaRoster))
	}
	// No warehouse schemas: bootstrap creates nothing in analytics/raw and issues no
	// CREATE SCHEMA at all (the tables live in the default public schema).
	if n := countPrefix(stmts, "CREATE SCHEMA"); n != 0 {
		t.Errorf("bootstrap issued %d CREATE SCHEMA statements, want none (no warehouse schemas)", n)
	}

	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "meta_bootstrap.sql"))
}

// TestMetaDDLCreateIfMissing proves the embedded meta DDL is applied
// create-if-missing and can be re-checked at each leader election: EnsureSchema
// emits IF NOT EXISTS everywhere, is byte-identical on a second run (idempotent),
// and carries no self-migration ledger, version gate, ALTER, or DROP.
func TestMetaDDLCreateIfMissing(t *testing.T) {
	ctx := context.Background()

	first := storetest.NewRecorder()
	if err := store.EnsureSchema(ctx, first); err != nil {
		t.Fatalf("EnsureSchema (bootstrap): %v", err)
	}
	second := storetest.NewRecorder()
	if err := store.EnsureSchema(ctx, second); err != nil {
		t.Fatalf("EnsureSchema (leader re-check): %v", err)
	}

	// Re-checked at each leader election: the second ensure emits the identical
	// idempotent statements, byte-for-byte.
	if a, b := first.Dump(), second.Dump(); string(a) != string(b) {
		t.Fatalf("EnsureSchema is not idempotent across re-checks:\nfirst:\n%s\nsecond:\n%s", a, b)
	}

	stmts := first.Statements()
	if len(stmts) == 0 {
		t.Fatal("EnsureSchema issued no statements")
	}
	for _, s := range stmts {
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "CREATE TABLE") && !strings.Contains(s, "IF NOT EXISTS") {
			t.Errorf("meta DDL is not create-if-missing:\n%s", s)
		}
		if strings.HasPrefix(trimmed, "CREATE INDEX") && !strings.Contains(s, "IF NOT EXISTS") {
			t.Errorf("meta index DDL is not create-if-missing:\n%s", s)
		}
		// No self-migration ledger or version gate: the re-check ensures tables, it
		// never alters, drops, or consults an applied-version.
		for _, banned := range []string{"ALTER TABLE", "DROP TABLE", "DROP DATABASE"} {
			if strings.Contains(strings.ToUpper(s), banned) {
				t.Errorf("EnsureSchema issued a %s statement; the re-check is create-if-missing, not migration-managed:\n%s", banned, s)
			}
		}
	}

	// The leader-election re-check ensures tables only; the one-time CREATE DATABASE
	// belongs to Bootstrap, never to the re-check.
	if n := countPrefix(stmts, "CREATE DATABASE"); n != 0 {
		t.Errorf("EnsureSchema issued %d CREATE DATABASE statements; database creation is a bootstrap-only step", n)
	}
}

// TestBootstrapNoLocalStateStore proves all engine state lives in Postgres behind
// the single DSN seam: the bootstrap path issues only Postgres DDL through the
// Execer and creates no local SQLite storage or .iris/state.db on disk.
func TestBootstrapNoLocalStateStore(t *testing.T) {
	ctx := context.Background()

	// Run bootstrap with the working directory rooted in an empty temp tree, so any
	// stray local state file (.iris/state.db, a SQLite database) created relative to
	// cwd would land here and be caught.
	tmp := t.TempDir()
	t.Chdir(tmp)

	rec := storetest.NewRecorder()
	if err := store.Bootstrap(ctx, rec, rec); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Bootstrap wrote nothing to the local filesystem.
	var created []string
	err := filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			created = append(created, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk temp tree: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("bootstrap created local files, want none (state lives in Postgres): %v", created)
	}

	// The only sink is the Postgres statement seam, and no statement references a
	// local state store.
	for _, s := range rec.Statements() {
		if !strings.HasPrefix(strings.TrimSpace(s), "CREATE") {
			t.Errorf("bootstrap issued a non-DDL statement, want only Postgres CREATE statements:\n%s", s)
		}
		lower := strings.ToLower(s)
		for _, token := range []string{"sqlite", "state.db", ".iris/state"} {
			if strings.Contains(lower, token) {
				t.Errorf("bootstrap statement references a local state store %q:\n%s", token, s)
			}
		}
	}
}
