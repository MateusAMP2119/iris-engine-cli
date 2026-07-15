package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the dispatch-level apply op: it validates a declaration against
// the current registry, then persists it through the single meta writer as one atomic
// registry transaction. Every write rides a real Dispatcher over a recording fake --
// no live Postgres -- so a test asserts the exact write set, its transaction
// grouping, and that a validation failure changes nothing.

// applyHarness wires an Applier over a real Dispatcher (the single-writer path) and
// a recording write connection, plus a seedable registry reader.
type applyHarness struct {
	applier *dispatch.Applier
	rec     *storetest.WriteRecorder
	reg     *storetest.RegistryFake
}

func newApplyHarness(t *testing.T) applyHarness {
	t.Helper()
	rec := storetest.NewWriteRecorder()
	reg := storetest.NewRegistryFake()
	d := dispatch.New(rec)
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	return applyHarness{applier: dispatch.NewApplier(reg, d), rec: rec, reg: reg}
}

// containsToken reports whether any recorded statement's SQL text or any of its
// string-valued args contains token.
func containsToken(stmts []storetest.RecordedStatement, token string) bool {
	for _, s := range stmts {
		if strings.Contains(s.SQL, token) {
			return true
		}
		for _, a := range s.Args {
			if str, ok := a.(string); ok && strings.Contains(str, token) {
				return true
			}
		}
	}
	return false
}

// touchesLanes reports whether any recorded statement references the lanes table.
func touchesLanes(stmts []storetest.RecordedStatement) bool {
	return containsToken(stmts, "lanes")
}

// TestApplyAtomicRegistryTxn proves an apply's registry changes commit in one
// dispatcher meta transaction, all-or-nothing, and that a validation failure
// changes nothing.
func TestApplyAtomicRegistryTxn(t *testing.T) {
	t.Run("apply-atomic-registry-txn", func(t *testing.T) {
		// A valid apply: its pipelines row and depends_on edges commit as one atomic
		// transaction, nothing outside it.
		h := newApplyHarness(t)
		h.reg.Register("extract_orders")
		decl := &declare.Pipeline{Name: "load_orders", Run: []string{"python", "main.py"}, DependsOn: []string{"extract_orders"}}
		if err := h.applier.ApplyPipeline(context.Background(), "pipelines/ingest/load_orders", decl); err != nil {
			t.Fatalf("ApplyPipeline: %v", err)
		}
		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("apply committed %d transactions, want 1 (one atomic registry transaction)", len(txns))
		}
		if len(h.rec.Statements()) != len(txns[0]) {
			t.Errorf("apply wrote %d statements but only %d rode the transaction; every registry change must ride the one transaction", len(h.rec.Statements()), len(txns[0]))
		}
		batch := txns[0]
		if len(stmtsWith(batch, "INSERT INTO pipelines")) != 1 {
			t.Errorf("atomic apply did not upsert the pipelines row: %v", batch)
		}
		if len(stmtsWith(batch, "INSERT INTO dependencies")) != 1 {
			t.Errorf("atomic apply did not write the depends_on edge in the same transaction: %v", batch)
		}

		// A validation failure changes nothing: a depends_on on an unregistered
		// pipeline is rejected and no statement is submitted.
		h2 := newApplyHarness(t)
		bad := &declare.Pipeline{Name: "load_orders", Run: []string{"python", "main.py"}, DependsOn: []string{"never_registered"}}
		if err := h2.applier.ApplyPipeline(context.Background(), "folder", bad); err == nil {
			t.Fatal("ApplyPipeline of a pipeline depending on an unregistered upstream succeeded, want a validation error")
		}
		if n := len(h2.rec.Statements()); n != 0 {
			t.Errorf("a rejected apply left %d statements in meta, want 0 (validation failure changes nothing)", n)
		}
		if n := len(h2.rec.Transactions()); n != 0 {
			t.Errorf("a rejected apply opened %d transactions, want 0", n)
		}
	})
}

// TestApplySingleMemberNoLanesRow proves a single-member lane (a lone pipeline with
// no composer) produces no lanes row: its apply persists the pipeline but writes
// nothing to lanes, so the name stays nominal until a composer promotes it to 2+.
func TestApplySingleMemberNoLanesRow(t *testing.T) {
	t.Run("single-member-no-lanes-row", func(t *testing.T) {
		h := newApplyHarness(t)
		decl := &declare.Pipeline{Name: "solo", Run: []string{"python", "main.py"}}
		if err := h.applier.ApplyPipeline(context.Background(), "pipelines/solo/solo", decl); err != nil {
			t.Fatalf("ApplyPipeline: %v", err)
		}
		if !stmtsAny(h.rec.Statements(), "INSERT INTO pipelines") {
			t.Errorf("the lone pipeline was not registered: %v", h.rec.Statements())
		}
		if touchesLanes(h.rec.Statements()) {
			t.Errorf("a single-member lane produced a lanes statement; its name stays nominal until a composer applies: %v", h.rec.Statements())
		}
	})
}

// TestApplySecretsNeverInMeta proves apply persists a declaration without storing
// resolved env or env_file values: a pipeline whose env and env_file carry
// distinctive secret strings registers with no secret value anywhere in the meta
// write set, and the pipelines row carries no env columns.
func TestApplySecretsNeverInMeta(t *testing.T) {
	t.Run("secrets-never-in-meta", func(t *testing.T) {
		const (
			secretValue = "sk-live-DISTINCTIVE-DO-NOT-PERSIST-0xDEADBEEF"
			interpolate = "${REGION_FROM_DAEMON_ENV}"
			secretFile  = "./super-secret-credentials.env"
		)
		h := newApplyHarness(t)
		decl := &declare.Pipeline{
			Name: "load_orders",
			Run:  []string{"python", "main.py"},
			Env: map[string]string{
				"SECRET_TOKEN": secretValue,
				"REGION":       interpolate,
				"LOG_LEVEL":    "info",
			},
			EnvFile: declare.StringList{secretFile},
		}
		if err := h.applier.ApplyPipeline(context.Background(), "pipelines/ingest/load_orders", decl); err != nil {
			t.Fatalf("ApplyPipeline: %v", err)
		}
		stmts := h.rec.Statements()
		for _, secret := range []string{secretValue, interpolate, secretFile, "SECRET_TOKEN"} {
			if containsToken(stmts, secret) {
				t.Errorf("a resolved secret %q landed in the meta write set: %v", secret, stmts)
			}
		}
		// The pipelines row carries no env/env_file column.
		inserts := stmtsWith(stmts, "INSERT INTO pipelines")
		if len(inserts) != 1 {
			t.Fatalf("apply issued %d pipelines inserts, want 1", len(inserts))
		}
		if strings.Contains(inserts[0].SQL, "env") {
			t.Errorf("the pipelines row carries an env column; env/env_file are never persisted: %q", inserts[0].SQL)
		}
	})
}

// TestApplyComposerAtomicLaneRewrite proves a composer apply rewrites the lane's
// entire order in one atomic all-or-nothing write through the single meta writer,
// regardless of whether the members are registered yet (lanes holds names, not FKs),
// and that an injected transaction failure commits nothing.
func TestApplyComposerAtomicLaneRewrite(t *testing.T) {
	t.Run("composer-apply-atomic-lane-rewrite", func(t *testing.T) {
		// The members are never registered, yet the rewrite writes their names.
		h := newApplyHarness(t)
		composer := &declare.Composer{Lane: "ingest", Order: []string{"never_registered_a", "never_registered_b"}}
		if err := h.applier.ApplyComposer(context.Background(), composer); err != nil {
			t.Fatalf("ApplyComposer: %v", err)
		}
		txns := h.rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("composer apply committed %d transactions, want 1 (one atomic full-lane rewrite)", len(txns))
		}
		if len(stmtsWith(txns[0], "INSERT INTO lanes")) != 2 {
			t.Errorf("composer apply did not write the whole order in one transaction (members need not be registered): %v", txns[0])
		}

		// All-or-nothing: an injected transaction failure commits nothing.
		hf := newApplyHarness(t)
		boom := errors.New("meta transaction aborted")
		hf.rec.FailTx(boom)
		if err := hf.applier.ApplyComposer(context.Background(), composer); !errors.Is(err, boom) {
			t.Errorf("ApplyComposer error = %v, want it to wrap the transaction failure", err)
		}
		if n := len(hf.rec.Statements()); n != 0 {
			t.Errorf("a failed atomic rewrite left %d committed statements, want 0 (all-or-nothing)", n)
		}
	})
}

// stmtsWith returns the recorded statements whose SQL contains sub.
func stmtsWith(stmts []storetest.RecordedStatement, sub string) []storetest.RecordedStatement {
	var out []storetest.RecordedStatement
	for _, s := range stmts {
		if strings.Contains(s.SQL, sub) {
			out = append(out, s)
		}
	}
	return out
}

// stmtsAny reports whether any recorded statement's SQL contains sub.
func stmtsAny(stmts []storetest.RecordedStatement, sub string) bool {
	return len(stmtsWith(stmts, sub)) > 0
}
