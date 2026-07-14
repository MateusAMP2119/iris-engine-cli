package pg_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg/pgtest"
)

// This file proves the data-database grant surface: the field-level GRANT DDL
// rendering, and reconciliation of a role's live grants against the meta access
// ledger -- the ledger is authoritative, the reconcile emits GRANT DDL to make
// Postgres match it additively, and strays beyond the ledger are reported, never
// silently fixed. The generated DDL is captured through the recording pg fake
// and diffed byte-for-byte against golden files, with no live Postgres;
// live-grant reads are a faked data-database seam.

// fakeLiveGrants is a recording-free LiveGrantReader: it returns a fixed set of
// live field grants (or an injected error), standing in for a pg_catalog read of
// what Postgres currently grants a role, so reconcile is provable with no live
// Postgres.
type fakeLiveGrants struct {
	grants []declare.FieldGrant
	err    error
}

func (f fakeLiveGrants) ReadFieldGrants(_ context.Context, _ string) ([]declare.FieldGrant, error) {
	return f.grants, f.err
}

var _ pg.LiveGrantReader = fakeLiveGrants{}

// TestApplyGrantsExactDeclared proves apply issues grants on the pipeline's
// Postgres role for exactly the declared tables and fields, nothing more: the
// declared reads/writes expand to one field grant each, and each renders to a
// column-level GRANT diffed against a golden -- reads as SELECT, writes as
// INSERT/UPDATE. Three declared fields (two read, one write) yield exactly three
// GRANT statements, in declaration order.
func TestApplyGrantsExactDeclared(t *testing.T) {
	t.Run("apply-grants-exact-declared", func(t *testing.T) {
		ctx := context.Background()
		reads := []declare.Access{{Table: "analytics.orders", Fields: []string{"id", "amount"}}}
		writes := []declare.Access{{Table: "raw.orders_staging", Fields: []string{"id"}}}

		grants, err := declare.GrantsFromAccess(reads, writes)
		if err != nil {
			t.Fatalf("GrantsFromAccess: %v", err)
		}

		rec := pgtest.New()
		for _, g := range grants {
			ddl, err := pg.RenderGrant("iris_load_orders", g.Schema, g.Table, g.Field, g.Access)
			if err != nil {
				t.Fatalf("RenderGrant(%+v): %v", g, err)
			}
			if err := rec.Exec(ctx, ddl); err != nil {
				t.Fatalf("record GRANT: %v", err)
			}
		}

		if got := len(rec.Statements()); got != 3 {
			t.Fatalf("apply issued %d grant statements, want exactly 3 (the declared fields, nothing more)", got)
		}
		golden.Assert(t, rec.Dump(), filepath.Join("testdata", "apply_orders_grants.sql"))
	})
}

// TestLedgerTruthReconciled proves the meta access ledger is authoritative and
// reconciliation emits grant DDL onto the data database to match it: with the role
// holding no live grants, reconcile issues exactly one GRANT per ledger field, in
// ledger order, and reports no stray.
func TestLedgerTruthReconciled(t *testing.T) {
	t.Run("ledger-truth-reconciled", func(t *testing.T) {
		ctx := context.Background()
		ledger := []declare.FieldGrant{
			{Schema: "analytics", Table: "orders", Field: "id", Access: declare.AccessRead},
			{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
		}
		live := fakeLiveGrants{} // Postgres holds nothing yet.

		rec := pgtest.New()
		plan, err := pg.Reconcile(ctx, rec, live, "iris_pat_orders", ledger)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(plan.Strays) != 0 {
			t.Errorf("reconcile reported %d strays over a clean role, want 0", len(plan.Strays))
		}
		if got := len(rec.Statements()); got != 2 {
			t.Fatalf("reconcile issued %d statements, want 2 (one GRANT per ledger field)", got)
		}
		golden.Assert(t, rec.Dump(), filepath.Join("testdata", "reconcile_ledger_truth.sql"))
	})
}

// TestGrantDriftReconcile proves grant drift diffs live Postgres grants against the
// meta ledger and reconciles so Postgres matches the ledger: a ledger field the
// role lacks is granted (additive), while a live grant beyond the ledger is
// reported as a non-additive stray and never revoked. The recording fake captures
// only the additive GRANT -- no REVOKE is ever issued.
func TestGrantDriftReconcile(t *testing.T) {
	t.Run("grant-drift-reconcile", func(t *testing.T) {
		ctx := context.Background()
		ledger := []declare.FieldGrant{
			{Schema: "analytics", Table: "orders", Field: "id", Access: declare.AccessRead},
			{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
		}
		live := fakeLiveGrants{grants: []declare.FieldGrant{
			{Schema: "analytics", Table: "orders", Field: "id", Access: declare.AccessRead},     // already granted
			{Schema: "analytics", Table: "orders", Field: "secret", Access: declare.AccessRead}, // stray beyond the ledger
		}}

		rec := pgtest.New()
		plan, err := pg.Reconcile(ctx, rec, live, "iris_pat_orders", ledger)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}

		// The one missing ledger field (amount) is granted; id (already live) is not
		// re-granted; the stray (secret) is never touched.
		stmts := rec.Statements()
		if len(stmts) != 1 {
			t.Fatalf("reconcile issued %d statements, want 1 (the single missing GRANT): %v", len(stmts), stmts)
		}
		for _, s := range stmts {
			if strings.Contains(s, "REVOKE") {
				t.Errorf("reconcile issued a REVOKE; strays are reported, never silently fixed: %q", s)
			}
			if !strings.Contains(s, `"amount"`) {
				t.Errorf("reconcile granted %q, want the missing field amount", s)
			}
		}
		golden.Assert(t, rec.Dump(), filepath.Join("testdata", "reconcile_grant_drift.sql"))

		// The stray rides the report as a non-additive, reported drift.
		if len(plan.Strays) != 1 {
			t.Fatalf("reconcile reported %d strays, want 1 (the secret field beyond the ledger)", len(plan.Strays))
		}
		if plan.Strays[0].Kind != declare.DriftNonAdditive || plan.Strays[0].Action != declare.ActionReport {
			t.Errorf("stray drift = %+v, want non_additive/report", plan.Strays[0])
		}
		if !strings.Contains(plan.Strays[0].Name, "secret") {
			t.Errorf("stray drift names %q, want it to concern the secret field", plan.Strays[0].Name)
		}
	})
}

// TestDataPATNoPostMintColumns proves grant reconciliation for a data PAT never
// grants a column added to the table after mint: the diff is computed against the
// ledger's fixed per-field grant set, not the table's current columns. A bare
// schema.table minted when the table had [id, amount] fixes that two-field ledger;
// after the table later gains a status column, reconcile grants only id and amount
// and never status -- even though re-expanding the bare grant now would include it.
func TestDataPATNoPostMintColumns(t *testing.T) {
	t.Run("data-pat-no-post-mint-columns", func(t *testing.T) {
		// Mint time: the table has exactly [id, amount].
		mintFields := map[string][]string{"analytics.orders": {"id", "amount"}}
		ledger, err := declare.ExpandDataPATGrants(
			[]declare.DataPATRead{{Table: "analytics.orders"}}, mintFields, nil)
		if err != nil {
			t.Fatalf("ExpandDataPATGrants(mint): %v", err)
		}
		if len(ledger) != 2 {
			t.Fatalf("mint ledger has %d grants, want 2 (id, amount)", len(ledger))
		}

		// Reconcile against the FIXED ledger with an empty live set: the additive
		// GRANTs cover exactly the recorded fields.
		plan, err := pg.ReconcileGrants("iris_pat_orders", ledger, nil)
		if err != nil {
			t.Fatalf("ReconcileGrants: %v", err)
		}
		joined := strings.Join(plan.Grants, "\n")
		for _, want := range []string{`"id"`, `"amount"`} {
			if !strings.Contains(joined, want) {
				t.Errorf("reconcile did not grant the ledger field %s: %v", want, plan.Grants)
			}
		}
		if strings.Contains(joined, "status") {
			t.Errorf("reconcile granted a post-mint column (status); it must diff against the fixed ledger, not current columns: %v", plan.Grants)
		}
		if len(plan.Grants) != 2 {
			t.Fatalf("reconcile issued %d GRANTs, want exactly 2 (the fixed ledger fields)", len(plan.Grants))
		}

		// Prove the fixity matters: the table has since grown a status column, so a
		// fresh bare expansion now WOULD include status -- but the ledger, fixed at
		// mint, does not, so reconcile above never granted it.
		grownFields := map[string][]string{"analytics.orders": {"id", "amount", "status"}}
		reExpanded, err := declare.ExpandDataPATGrants(
			[]declare.DataPATRead{{Table: "analytics.orders"}}, grownFields, nil)
		if err != nil {
			t.Fatalf("ExpandDataPATGrants(grown): %v", err)
		}
		var sawStatus bool
		for _, g := range reExpanded {
			if g.Field == "status" {
				sawStatus = true
			}
		}
		if !sawStatus {
			t.Fatal("re-expanding the bare grant over the grown table did not include status; the fixity guard proves nothing")
		}
	})
}
