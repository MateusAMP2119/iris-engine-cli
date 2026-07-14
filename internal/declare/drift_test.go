package declare_test

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// tbl builds a declared table head from (schema, table) and name:type column
// pairs, for the drift-classification fixtures. Only name and type matter to the
// classifier, so the modifiers stay at their zero values.
func tbl(schema, table string, cols ...[2]string) *declare.Table {
	t := &declare.Table{Schema: schema, Table: table}
	for _, c := range cols {
		t.Columns = append(t.Columns, declare.Column{Name: c[0], Type: c[1]})
	}
	return t
}

// live builds a live-Postgres table view from (schema, table), a capture-trigger
// presence flag, and name:type column pairs (types in canonical Postgres form).
func live(schema, table string, hasTrigger bool, cols ...[2]string) declare.LiveTable {
	lt := declare.LiveTable{Schema: schema, Table: table, HasCaptureTrigger: hasTrigger}
	for _, c := range cols {
		lt.Columns = append(lt.Columns, declare.LiveColumn{Name: c[0], Type: c[1]})
	}
	return lt
}

// findDrift returns the first drift matching subject and a column short name
// (matched against the trailing schema.table.column segment of the drift's
// qualified Name), and whether one was found.
func findDrift(ds []declare.Drift, subject declare.DriftSubject, name string) (declare.Drift, bool) {
	for _, d := range ds {
		if d.Subject != subject {
			continue
		}
		if d.Name == name || (name != "" && strings.HasSuffix(d.Name, "."+name)) {
			return d, true
		}
	}
	return declare.Drift{}, false
}

// TestDriftAdditiveOnlyAutofix proves the additive-only doctrine holds across all
// three drift comparisons at once: every additive gap
// carries the autofix action (the engine auto-resolves it), and every
// non-additive discrepancy carries no automatic action -- it is refused or
// reported, never autofixed. The invariant is checked over a mixed report drawn
// from schema, ledger, and grant drift together, so no single domain can satisfy
// it vacuously.
func TestDriftAdditiveOnlyAutofix(t *testing.T) {
	t.Run("drift-additive-only-autofix", func(t *testing.T) {
		// Schema drift: one missing column (additive) and one extra column
		// (non-additive) on the same table, capture trigger present.
		schemaRep, err := declare.ClassifySchemaDrift(
			tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}, [2]string{"status", "text"}),
			live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}, [2]string{"legacy_id", "integer"}),
		)
		if err != nil {
			t.Fatalf("ClassifySchemaDrift: %v", err)
		}

		// Ledger drift: one column added in table.yaml but absent from the ledger
		// head (additive gap) and one column removed from table.yaml (non-additive).
		ledgerRep, err := declare.ClassifyLedgerDrift(
			tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"status", "text"}),
			declare.LedgerState{Columns: []declare.LedgerColumn{{Name: "id", Type: "uuid"}, {Name: "amount", Type: "numeric"}}},
		)
		if err != nil {
			t.Fatalf("ClassifyLedgerDrift: %v", err)
		}

		// Grant drift: one grant the ledger asserts but Postgres lacks (additive
		// gap) and several stray grants Postgres holds beyond the ledger bounds
		// (non-additive), supplied out of name order to exercise the report's
		// deterministic ordering.
		grantRep := declare.ClassifyGrantDrift(declare.GrantView{
			Bounds: []declare.Grant{{Role: "reader", Schema: "public", Object: "orders", Privilege: "SELECT"}},
			Live: []declare.Grant{
				{Role: "reader", Schema: "meta", Object: "meta", Privilege: "CONNECT"},
				{Role: "reader", Schema: "public", Object: "zzz", Privilege: "SELECT"},
				{Role: "reader", Schema: "public", Object: "aaa", Privilege: "SELECT"},
			},
		})

		// Stray grants ride the report in deterministic sorted order, like the
		// schema/ledger extras the DriftReport doc promises.
		var strayNames []string
		for _, d := range grantRep.NonAdditive() {
			strayNames = append(strayNames, d.Name)
		}
		if !sort.StringsAreSorted(strayNames) {
			t.Errorf("stray grant drifts are not in deterministic sorted order: %v", strayNames)
		}

		for _, dom := range []struct {
			name string
			rep  declare.DriftReport
		}{
			{"schema", schemaRep},
			{"ledger", ledgerRep},
			{"grant", grantRep},
		} {
			var sawAdditiveAutofix, sawNonAdditiveReported bool
			for _, d := range dom.rep.Drifts {
				switch d.Kind {
				case declare.DriftAdditive:
					if d.Action != declare.ActionAutofix {
						t.Errorf("%s: additive drift %q action = %q, want autofix (only additive gaps auto-resolve)", dom.name, d.Name, d.Action)
					}
					sawAdditiveAutofix = true
				case declare.DriftNonAdditive:
					if d.Action == declare.ActionAutofix {
						t.Errorf("%s: non-additive drift %q was autofixed; non-additive changes take no automatic action", dom.name, d.Name)
					}
					sawNonAdditiveReported = true
				default:
					t.Errorf("%s: drift %q has unknown kind %q", dom.name, d.Name, d.Kind)
				}
			}
			if !sawAdditiveAutofix {
				t.Errorf("%s: no additive-autofix drift found; the doctrine must span this domain", dom.name)
			}
			if !sawNonAdditiveReported {
				t.Errorf("%s: no non-additive drift found; the doctrine must span this domain", dom.name)
			}
		}
	})
}

// TestLedgerDriftRemovalRefused proves a column removed from table.yaml relative
// to the migrations-ledger head is classified non-additive and refused, never
// dropped. The ledger records amount; table.yaml no longer declares it.
func TestLedgerDriftRemovalRefused(t *testing.T) {
	t.Run("ledger-drift-removal-refused", func(t *testing.T) {
		rep, err := declare.ClassifyLedgerDrift(
			tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"customer_id", "uuid"}),
			declare.LedgerState{Columns: []declare.LedgerColumn{
				{Name: "id", Type: "uuid"}, {Name: "customer_id", Type: "uuid"}, {Name: "amount", Type: "numeric"},
			}},
		)
		if err != nil {
			t.Fatalf("ClassifyLedgerDrift: %v", err)
		}

		// A nil declared head is a sentinel error, consistent with ClassifySchemaDrift
		// (both classifiers refuse a nil head identically, never a silent empty report).
		if _, nerr := declare.ClassifyLedgerDrift(nil, declare.LedgerState{}); !errors.Is(nerr, declare.ErrNilDeclaredTable) {
			t.Errorf("ClassifyLedgerDrift(nil) err = %v, want ErrNilDeclaredTable", nerr)
		}

		d, ok := findDrift(rep.Drifts, declare.SubjectColumn, "amount")
		if !ok {
			t.Fatalf("no ledger drift for the removed column amount; drifts = %+v", rep.Drifts)
		}
		if d.Kind != declare.DriftNonAdditive {
			t.Errorf("removed column amount kind = %q, want non_additive", d.Kind)
		}
		if d.Action != declare.ActionRefuse {
			t.Errorf("removed column amount action = %q, want refuse (never dropped)", d.Action)
		}
		if d.Domain != declare.DomainLedger {
			t.Errorf("removed column amount domain = %q, want ledger", d.Domain)
		}
		if !rep.Refused() {
			t.Error("Refused() = false, want true: a ledger removal refuses apply")
		}
		// The removal is never resolved by an automatic drop: no autofix names amount.
		for _, d := range rep.Autofixes() {
			if d.Name == "amount" {
				t.Errorf("removed column amount has an autofix %+v; a removal is refused, never auto-dropped", d)
			}
		}
	})
}

// TestSchemaDriftExcludesEngineOwned proves engine-owned surfaces are outside the
// schema-drift comparison: the journal table is never
// flagged even when present in the live view, a present capture trigger is never
// flagged, and a missing capture trigger is classified additive/autofix -- the
// classification vocabulary the engine's trigger-emission step later executes.
func TestSchemaDriftExcludesEngineOwned(t *testing.T) {
	t.Run("schema-drift-excludes-engine-owned", func(t *testing.T) {
		// The journal is engine-owned; a declared user table is not.
		if !declare.IsEngineOwnedTable("public", "data_journal") {
			t.Error("IsEngineOwnedTable(public, data_journal) = false, want true (the journal is engine-owned)")
		}
		if declare.IsEngineOwnedTable("analytics", "orders") {
			t.Error("IsEngineOwnedTable(analytics, orders) = true, want false (a declared user table is not engine-owned)")
		}

		declared := []*declare.Table{tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"name", "text"})}

		// Live view carries the journal (engine-owned) alongside the user table,
		// whose capture trigger is installed.
		liveWithJournalAndTrigger := []declare.LiveTable{
			live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"name", "text"}),
			live("public", "data_journal", false, [2]string{"id", "bigint"}, [2]string{"schema", "text"}, [2]string{"row_pk", "text"}),
		}
		rep, err := declare.ClassifySchema(declared, liveWithJournalAndTrigger)
		if err != nil {
			t.Fatalf("ClassifySchema: %v", err)
		}
		for _, d := range rep.Drifts {
			if strings.Contains(d.Name, "data_journal") {
				t.Errorf("journal flagged as drift: %+v; the journal is excluded from comparison", d)
			}
			if d.Subject == declare.SubjectCaptureTrigger {
				t.Errorf("present capture trigger flagged as drift: %+v; an installed trigger is never flagged", d)
			}
		}

		// A live table whose capture trigger is missing: classified additive/autofix,
		// exactly like a missing column.
		liveMissingTrigger := []declare.LiveTable{
			live("analytics", "orders", false, [2]string{"id", "uuid"}, [2]string{"name", "text"}),
		}
		rep, err = declare.ClassifySchema(declared, liveMissingTrigger)
		if err != nil {
			t.Fatalf("ClassifySchema (missing trigger): %v", err)
		}
		var trigDrifts []declare.Drift
		for _, d := range rep.Drifts {
			if d.Subject == declare.SubjectCaptureTrigger {
				trigDrifts = append(trigDrifts, d)
			}
		}
		if len(trigDrifts) != 1 {
			t.Fatalf("missing capture trigger produced %d trigger drifts, want exactly 1; got %+v", len(trigDrifts), rep.Drifts)
		}
		if trigDrifts[0].Kind != declare.DriftAdditive || trigDrifts[0].Action != declare.ActionAutofix {
			t.Errorf("missing capture trigger classified %q/%q, want additive/autofix", trigDrifts[0].Kind, trigDrifts[0].Action)
		}
	})
}

// TestSchemaDriftNonAdditiveRefused proves schema drift flags an extra, renamed,
// or retyped live column as non-additive and refuses apply, never auto-dropping.
// A rename manifests in a pure name diff as an extra
// (old-name) live column, which is the refusing discrepancy.
func TestSchemaDriftNonAdditiveRefused(t *testing.T) {
	t.Run("schema-drift-nonadditive-refused", func(t *testing.T) {
		// A nil declared head is a sentinel error, consistent with ClassifyLedgerDrift.
		if _, err := declare.ClassifySchemaDrift(nil, declare.LiveTable{}); !errors.Is(err, declare.ErrNilDeclaredTable) {
			t.Errorf("ClassifySchemaDrift(nil) err = %v, want ErrNilDeclaredTable", err)
		}

		cases := []struct {
			name       string
			declared   *declare.Table
			live       declare.LiveTable
			refusedCol string
		}{
			{
				name:       "extra live column",
				declared:   tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}),
				live:       live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}, [2]string{"legacy_id", "integer"}),
				refusedCol: "legacy_id",
			},
			{
				name:       "retyped live column",
				declared:   tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}),
				live:       live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"amount", "text"}),
				refusedCol: "amount",
			},
			{
				name:       "renamed live column (old name lingers)",
				declared:   tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"total", "numeric"}),
				live:       live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"amount", "numeric"}),
				refusedCol: "amount",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rep, err := declare.ClassifySchemaDrift(tc.declared, tc.live)
				if err != nil {
					t.Fatalf("ClassifySchemaDrift: %v", err)
				}
				d, ok := findDrift(rep.Drifts, declare.SubjectColumn, tc.refusedCol)
				if !ok {
					t.Fatalf("no drift for %q; drifts = %+v", tc.refusedCol, rep.Drifts)
				}
				if d.Kind != declare.DriftNonAdditive {
					t.Errorf("%q kind = %q, want non_additive", tc.refusedCol, d.Kind)
				}
				if d.Action != declare.ActionRefuse {
					t.Errorf("%q action = %q, want refuse", tc.refusedCol, d.Action)
				}
				if !rep.Refused() {
					t.Errorf("Refused() = false, want true for %s", tc.name)
				}
				// Never auto-drop: no autofix targets the refused column.
				for _, af := range rep.Autofixes() {
					if af.Name == tc.refusedCol {
						t.Errorf("%q has an autofix %+v; apply never auto-drops a non-additive column", tc.refusedCol, af)
					}
				}
			})
		}
	})
}

// TestNonAdditiveRefusedOutright proves non-additive schema changes are refused
// outright with no confirmation gate ever offered. The
// no-gate guarantee is asserted structurally: neither the Drift value nor the
// DriftReport result carries any confirmation/gate field, so no code path can
// offer one. The behavioral half asserts a non-additive schema drift resolves to
// the refuse action.
func TestNonAdditiveRefusedOutright(t *testing.T) {
	t.Run("non-additive-refused-outright", func(t *testing.T) {
		// Structural: no gate hook exists on the result types.
		forbidden := []string{"confirm", "gate", "prompt", "approve"}
		for _, typ := range []reflect.Type{reflect.TypeOf(declare.Drift{}), reflect.TypeOf(declare.DriftReport{})} {
			for i := 0; i < typ.NumField(); i++ {
				fn := strings.ToLower(typ.Field(i).Name)
				for _, bad := range forbidden {
					if strings.Contains(fn, bad) {
						t.Errorf("%s carries a confirmation-gate field %q; non-additive changes are refused outright, never gated", typ.Name(), typ.Field(i).Name)
					}
				}
			}
		}

		// Behavioral: a non-additive schema drift resolves to refuse, no gate.
		rep, err := declare.ClassifySchemaDrift(
			tbl("analytics", "orders", [2]string{"id", "uuid"}),
			live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"legacy_id", "integer"}),
		)
		if err != nil {
			t.Fatalf("ClassifySchemaDrift: %v", err)
		}
		na := rep.NonAdditive()
		if len(na) == 0 {
			t.Fatal("no non-additive drift produced for an extra live column")
		}
		for _, d := range na {
			if d.Action != declare.ActionRefuse {
				t.Errorf("non-additive drift %q action = %q, want refuse (refused outright)", d.Name, d.Action)
			}
		}
	})
}

// TestEngineColumnRefusedAsDrift proves an engine-added column on a user table is
// classified as non-additive drift and refused, keeping table.yaml authoritative.
// The exclusion of engine-owned surfaces is at
// the object level (the journal table, the capture trigger) -- never at the column
// level: an extra live column is refused regardless of an engine-ish name, while
// the capture trigger on the same table stays excluded.
func TestEngineColumnRefusedAsDrift(t *testing.T) {
	t.Run("engine-column-refused-as-drift", func(t *testing.T) {
		rep, err := declare.ClassifySchemaDrift(
			tbl("analytics", "orders", [2]string{"id", "uuid"}, [2]string{"name", "text"}),
			// An engine-ish extra column plus an installed capture trigger.
			live("analytics", "orders", true, [2]string{"id", "uuid"}, [2]string{"name", "text"}, [2]string{"_iris_run_id", "bigint"}),
		)
		if err != nil {
			t.Fatalf("ClassifySchemaDrift: %v", err)
		}

		d, ok := findDrift(rep.Drifts, declare.SubjectColumn, "_iris_run_id")
		if !ok {
			t.Fatalf("engine-added column _iris_run_id was not flagged; drifts = %+v", rep.Drifts)
		}
		if d.Kind != declare.DriftNonAdditive || d.Action != declare.ActionRefuse {
			t.Errorf("_iris_run_id classified %q/%q, want non_additive/refuse (table.yaml authoritative)", d.Kind, d.Action)
		}
		if !rep.Refused() {
			t.Error("Refused() = false, want true: an engine-added column refuses apply")
		}
		// The engine-ish column is never excluded as engine-owned; only the trigger is.
		for _, d := range rep.Drifts {
			if d.Subject == declare.SubjectCaptureTrigger {
				t.Errorf("the installed capture trigger was flagged: %+v; an engine-owned object is excluded, unlike an engine-added column", d)
			}
		}
	})
}

// TestCrossModeReadWarns proves apply warns but never refuses when a
// permanent-data pipeline declares reads on a disposable-mode pipeline's table:
// the legitimate mid-promotion state. The check returns
// warnings only -- there is no error path -- and stays silent when the reader is
// disposable or every upstream is already permanent.
func TestCrossModeReadWarns(t *testing.T) {
	t.Run("cross-mode-read-warns", func(t *testing.T) {
		// Permanent reader over a disposable upstream: exactly one warning, no refusal.
		warns := declare.CheckCrossModeReads(declare.DataPermanent, []declare.UpstreamRead{
			{Table: "raw.orders_staging", Mode: declare.DataDisposable},
		})
		if len(warns) != 1 {
			t.Fatalf("permanent reader over disposable upstream: %d warnings, want 1", len(warns))
		}
		if warns[0].Kind != declare.WarnCrossModeRead {
			t.Errorf("warning kind = %q, want cross_mode_read", warns[0].Kind)
		}
		if warns[0].Table != "raw.orders_staging" {
			t.Errorf("warning table = %q, want raw.orders_staging", warns[0].Table)
		}
		if warns[0].Message == "" {
			t.Error("cross-mode warning has no message")
		}

		// A disposable reader is never mid-promotion: no warning.
		if w := declare.CheckCrossModeReads(declare.DataDisposable, []declare.UpstreamRead{
			{Table: "raw.orders_staging", Mode: declare.DataDisposable},
		}); len(w) != 0 {
			t.Errorf("disposable reader: %d warnings, want 0", len(w))
		}

		// A permanent reader over a permanent upstream: no warning.
		if w := declare.CheckCrossModeReads(declare.DataPermanent, []declare.UpstreamRead{
			{Table: "analytics.orders", Mode: declare.DataPermanent},
		}); len(w) != 0 {
			t.Errorf("permanent reader over permanent upstream: %d warnings, want 0", len(w))
		}

		// Mixed upstreams: a warning for each disposable one only, in order.
		mixed := declare.CheckCrossModeReads(declare.DataPermanent, []declare.UpstreamRead{
			{Table: "a.one", Mode: declare.DataDisposable},
			{Table: "b.two", Mode: declare.DataPermanent},
			{Table: "c.three", Mode: declare.DataDisposable},
		})
		if len(mixed) != 2 {
			t.Fatalf("mixed upstreams: %d warnings, want 2", len(mixed))
		}
		if mixed[0].Table != "a.one" || mixed[1].Table != "c.three" {
			t.Errorf("mixed warnings name %q, %q; want a.one, c.three", mixed[0].Table, mixed[1].Table)
		}
	})
}
