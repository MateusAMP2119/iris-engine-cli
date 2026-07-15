package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the engine-managed pipeline-role teardown rides declare destroy
// and engine uninstall, never a standalone operation. The proof is two-sided:
//   - behavioral: RetirePipeline (the destroy path) retires the role/grants/
//     credentials rows in its one atomic transaction, and engine uninstall drops the
//     whole meta database (DropMetaDatabaseDDL), taking the access ledger with it;
//   - structural: the role-teardown SQL lives in exactly one source file
//     (teardown.go), and its only caller is the destroy op, so no standalone verb or
//     entry point tears a role down outside those two confirmation-gated paths.

// roleTeardownSQL is the full set of access-ledger deletes a pipeline-role
// retirement issues: its owner row, its grants, and its credential. The behavioral
// check below asserts all three ride RetirePipeline's one transaction.
var roleTeardownSQL = []string{
	"DELETE FROM roles",
	"DELETE FROM grants",
	"DELETE FROM credentials",
}

// teardownExclusiveSQL are the deletes that mean "tear this role down" and nothing
// else: removing the role's owner row (roles) and its login secret (credentials).
// DELETE FROM grants is deliberately absent -- ReplaceGrants clears a role's grants
// to REPLACE them on a routine apply, and the role survives that, so a grants delete
// is not a teardown signature. Only roles and credentials rows are removed solely at
// teardown (credential rotation is an upsert, never a delete), so these two pin the
// teardown to its single source file.
var teardownExclusiveSQL = []string{
	"DELETE FROM roles",
	"DELETE FROM credentials",
}

// TestRoleTeardownRidesDestroyAndUninstall proves the role teardown is reachable
// only through declare destroy and engine uninstall.
func TestRoleTeardownRidesDestroyAndUninstall(t *testing.T) {
	t.Run("role-teardown-rides-destroy-uninstall", func(t *testing.T) {
		// Destroy: RetirePipeline retires the role/grants/credentials rows in its one
		// atomic meta transaction (the destroy op rides its own confirmation gate).
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.RetirePipeline(context.Background(), "load_orders"); err != nil {
			t.Fatalf("RetirePipeline: %v", err)
		}
		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("RetirePipeline committed %d transactions, want 1 (atomic teardown)", len(txns))
		}
		batch := txns[0]
		for _, want := range roleTeardownSQL {
			if !stmtInBatch(batch, want) {
				t.Errorf("destroy teardown does not retire the role ledger: missing %q", want)
			}
		}
		// grants and credentials (children) are retired before roles (parent), so the
		// batch never trips the FK mid-flight.
		if pos(batch, "DELETE FROM grants") > pos(batch, "DELETE FROM roles") {
			t.Errorf("grants must be retired before roles (FK order): %v", batch)
		}
		if pos(batch, "DELETE FROM credentials") > pos(batch, "DELETE FROM roles") {
			t.Errorf("credentials must be retired before roles (FK order): %v", batch)
		}

		// Uninstall: dropping meta removes the whole access ledger (roles, grants,
		// credentials go with the database). It is the uninstall teardown's meta half.
		drop := store.DropMetaDatabaseDDL()
		if !strings.Contains(drop, "DROP DATABASE") || !strings.Contains(drop, store.MetaDatabase) {
			t.Errorf("uninstall meta teardown = %q, want a DROP DATABASE %s", drop, store.MetaDatabase)
		}
	})

	// Structural: the role-teardown SQL is constructed in exactly one file.
	t.Run("role-teardown SQL lives only in the destroy teardown", func(t *testing.T) {
		files := moduleGoFiles(t)
		for _, frag := range teardownExclusiveSQL {
			var owners []string
			for path, src := range files {
				if strings.Contains(src, frag) {
					owners = append(owners, path)
				}
			}
			if len(owners) != 1 || !strings.HasSuffix(owners[0], "internal/store/teardown.go") {
				t.Errorf("statement %q is constructed in %v, want only internal/store/teardown.go (no standalone role teardown)", frag, owners)
			}
		}
	})

	// Structural: RetirePipeline's only call site is the destroy op. Uninstall never
	// calls it (it drops meta wholesale), so the teardown rides destroy and uninstall
	// and nothing else.
	t.Run("RetirePipeline is called only by the destroy op", func(t *testing.T) {
		files := moduleGoFiles(t)
		var callers []string
		for path, src := range files {
			if strings.Contains(src, ".RetirePipeline(") {
				callers = append(callers, path)
			}
		}
		if len(callers) != 1 || !strings.HasSuffix(callers[0], "internal/dispatch/destroy.go") {
			t.Errorf("RetirePipeline is called from %v, want only internal/dispatch/destroy.go", callers)
		}
	})
}

// stmtInBatch reports whether any statement in the batch contains sub.
func stmtInBatch(batch []storetest.RecordedStatement, sub string) bool {
	for _, s := range batch {
		if strings.Contains(s.SQL, sub) {
			return true
		}
	}
	return false
}

// pos returns the index of the first statement in the batch whose SQL contains sub,
// or len(batch) when none does.
func pos(batch []storetest.RecordedStatement, sub string) int {
	for i, s := range batch {
		if strings.Contains(s.SQL, sub) {
			return i
		}
	}
	return len(batch)
}

// moduleGoFiles reads every non-test .go file under the module's internal/ and
// cmd/ trees (testdata skipped), keyed by slash-separated repo-relative path. It
// is the source-scan basis for the structural teardown-only checks: a call-graph
// proof over the real tree, mirroring the arch package's own source walk.
func moduleGoFiles(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	out := map[string]string{}
	for _, base := range []string{"internal", "cmd"} {
		walkRoot := filepath.Join(root, base)
		err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // G304: a repo source file under the module root, not user or network input.
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(src)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("moduleGoFiles found no source files; check the repo-root path")
	}
	return out
}
