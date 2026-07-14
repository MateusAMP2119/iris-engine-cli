package pg_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/golden"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg/pgtest"
)

// captureInstallDDL is every statement the engine emits to install write capture
// for a plan: the iris schema, the iris.capture() function, and each declared
// table's capture triggers. It is the DDL the "adds no columns" and "stamp never a
// user column" contracts assert never touches a user table's shape.
func captureInstallDDL(plan pg.ProvisionPlan) []string {
	out := []string{pg.CaptureSchemaDDL(), pg.CaptureFunctionDDL()}
	for _, tp := range plan.Tables {
		out = append(out, tp.CaptureTriggers...)
	}
	return out
}

// TestCaptureStatementLevelOneInsert proves the capture triggers are
// statement-level with transition tables and the iris.capture() body issues exactly
// one INSERT...SELECT per fired statement -- a 10M-row load fires one trigger, not
// 10M -- and only ever inserts on the hot write path: nothing is partitioned,
// sealed, or archived inline.
func TestCaptureStatementLevelOneInsert(t *testing.T) {
	// The trigger bindings are FOR EACH STATEMENT (never FOR EACH ROW) and carry a
	// REFERENCING transition-table clause, the set-based capture seam.
	for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
		if !strings.Contains(trig, "FOR EACH STATEMENT") {
			t.Errorf("capture trigger is not statement-level:\n%s", trig)
		}
		if strings.Contains(trig, "FOR EACH ROW") {
			t.Errorf("capture trigger is row-level; capture is statement-level:\n%s", trig)
		}
		if !strings.Contains(trig, "REFERENCING") {
			t.Errorf("capture trigger has no transition table:\n%s", trig)
		}
	}

	body := pg.CaptureFunctionDDL()

	// Exactly one INSERT...SELECT into the journal per statement: the transition
	// table holds every changed row, so one set-based insert stamps them all.
	if n := strings.Count(body, "INSERT INTO public.data_journal"); n != 1 {
		t.Errorf("iris.capture() issues %d INSERT INTO public.data_journal, want exactly 1 per statement", n)
	}
	for _, want := range []string{
		"SELECT",                         // set-based, not a per-row assignment
		"FROM %s",                        // reads the transition table
		"current_setting('iris.run_id')", // run id read in-transaction from the injected session
	} {
		if !strings.Contains(body, want) {
			t.Errorf("iris.capture() body missing %q:\n%s", want, body)
		}
	}

	// The hot write path only inserts a stamp: no per-row loop, and no inline
	// partitioning, sealing, archiving, or DDL against any table.
	upper := strings.ToUpper(body)
	for _, forbidden := range []string{"FOR EACH ROW", "LOOP", "PARTITION", "SEAL", "ARCHIVE", "CHECKPOINT", "ALTER TABLE", "CREATE TABLE", "DROP TABLE"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("iris.capture() body does hot-path %q; capture only inserts a stamp:\n%s", forbidden, body)
		}
	}

	golden.Assert(t, []byte(body+"\n"), filepath.Join("testdata", "capture_function.sql"))
}

// TestCaptureTriggersAlwaysOn proves provisioning installs the full capture-trigger
// set on every declared table unconditionally: every table in the plan carries its
// three per-operation triggers, and there is no declaration field a table could set
// to opt out.
func TestCaptureTriggersAlwaysOn(t *testing.T) {
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	if len(plan.Tables) != len(tables) {
		t.Fatalf("plan has %d tables, want %d (every declared table planned)", len(plan.Tables), len(tables))
	}
	for _, tp := range plan.Tables {
		// The full three-operation trigger set (INSERT, UPDATE, DELETE), always.
		if len(tp.CaptureTriggers) != 3 {
			t.Errorf("%s.%s got %d capture triggers, want the full unconditional set of 3", tp.Schema, tp.Table, len(tp.CaptureTriggers))
		}
	}

	// No declaration field can disable capture: the declared table type carries no
	// capture/trigger opt-out knob, so a table.yaml cannot turn capture off.
	assertNoCaptureField(t, reflect.TypeOf(tables[0].Spec).Elem(), "declare table spec")
}

// TestCaptureNoOptOut proves capture is always on for every pipeline and role: no
// configuration setting disables it, and provisioning installs the triggers on a
// declared table regardless of any input.
func TestCaptureNoOptOut(t *testing.T) {
	// No configuration knob disables capture: neither the resolved settings nor any
	// configuration layer carries a capture toggle.
	assertNoCaptureField(t, reflect.TypeOf(config.Settings{}), "config.Settings")
	assertNoCaptureField(t, reflect.TypeOf(config.Layer{}), "config.Layer")

	// Provisioning still installs capture triggers unconditionally: a declared table
	// gets its full set with no gate to skip it.
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	for _, tp := range plan.Tables {
		if len(tp.CaptureTriggers) == 0 {
			t.Errorf("%s.%s has no capture triggers; capture cannot be opted out", tp.Schema, tp.Table)
		}
	}
}

// assertNoCaptureField fails when any field of the struct type names capture,
// proving no opt-out toggle exists on that surface.
func assertNoCaptureField(t *testing.T, typ reflect.Type, what string) {
	t.Helper()
	if typ.Kind() != reflect.Struct {
		t.Fatalf("%s is not a struct type: %s", what, typ.Kind())
	}
	for i := 0; i < typ.NumField(); i++ {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "capture") {
			t.Errorf("%s has a %q field; capture must have no opt-out", what, typ.Field(i).Name)
		}
	}
}

// TestCaptureAddsNoColumns proves installing the capture triggers adds no columns to
// a declared table: the capture-install DDL (the iris schema, the capture function,
// and the per-table triggers) issues no ALTER TABLE / ADD COLUMN, so table.yaml
// remains the sole authority for the table's shape.
func TestCaptureAddsNoColumns(t *testing.T) {
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}

	for _, stmt := range captureInstallDDL(plan) {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ALTER TABLE") || strings.Contains(up, "ADD COLUMN") {
			t.Errorf("capture install alters a table's columns; capture adds no columns:\n%s", stmt)
		}
	}

	// The only CREATE TABLE for a declared table is its own table.yaml head, which
	// carries exactly the declared columns -- capture injects none.
	for _, tp := range plan.Tables {
		create, ok := tp.Branch.(pg.CreateFromHead)
		if !ok {
			t.Fatalf("%s.%s branch = %T, want CreateFromHead", tp.Schema, tp.Table, tp.Branch)
		}
		for _, stamp := range []string{"pg_role", "run_id", "row_pk", "pre_image", "recorded_at"} {
			if strings.Contains(create.DDL, stamp) {
				t.Errorf("%s.%s create DDL carries the journal column %q; the engine adds no columns to user tables:\n%s",
					tp.Schema, tp.Table, stamp, create.DDL)
			}
		}
	}
}

// TestStampNeverUserColumn proves run attribution lives only in data_journal: the
// pg_role and run_id stamp columns exist in the journal DDL but the engine-emitted
// DDL for a declared table (its create plus its capture triggers) never adds them.
func TestStampNeverUserColumn(t *testing.T) {
	ctx := context.Background()

	// The stamp columns live in the journal.
	journal := pgtest.New()
	if err := pg.EnsureJournal(ctx, journal); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	journalDDL := string(journal.Dump())
	for _, stamp := range []string{"pg_role", "run_id"} {
		if !strings.Contains(journalDDL, stamp) {
			t.Errorf("data_journal DDL missing the %q stamp column; attribution lives in the journal", stamp)
		}
	}

	// No user-table DDL the engine emits carries a stamp column: not the table
	// create, not the capture triggers, not the capture function.
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	var userTableDDL []string
	for _, tp := range plan.Tables {
		create, ok := tp.Branch.(pg.CreateFromHead)
		if !ok {
			t.Fatalf("%s.%s branch = %T, want CreateFromHead", tp.Schema, tp.Table, tp.Branch)
		}
		userTableDDL = append(userTableDDL, create.DDL)
		userTableDDL = append(userTableDDL, tp.CaptureTriggers...)
	}
	for _, stmt := range userTableDDL {
		up := strings.ToUpper(stmt)
		if strings.Contains(up, "ADD COLUMN") {
			t.Errorf("engine-emitted user-table DDL adds a column; the stamp is never a user column:\n%s", stmt)
		}
		// The trigger and create DDL must not introduce a pg_role / run_id column on
		// the user table (the trigger references the function, not these columns).
		if strings.Contains(stmt, "ADD COLUMN") && (strings.Contains(stmt, "pg_role") || strings.Contains(stmt, "run_id")) {
			t.Errorf("engine-emitted DDL adds a stamp column to a user table:\n%s", stmt)
		}
	}
}

// TestProvisionEnsuresCaptureFull proves both provisioning ends by ensuring capture:
// applying a plan that must create the journal emits the partitioned journal exactly
// once (parent, its open tail partition, and the select grant) and a capture trigger
// on every declared table.
func TestProvisionEnsuresCaptureFull(t *testing.T) {
	ctx := context.Background()
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	if !plan.EnsureJournal {
		t.Fatal("plan does not ensure the journal against an empty live view")
	}

	rec := pgtest.New()
	if err := plan.Apply(ctx, rec, &recordingLedger{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	stmts := rec.Statements()

	// The partitioned journal is ensured exactly once per data database: one parent
	// CREATE (PARTITION BY RANGE), its open tail partition (PARTITION OF), and the
	// select grant that opens read to every role.
	if n := countContains(stmts, "PARTITION BY RANGE (id)"); n != 1 {
		t.Errorf("journal parent created %d times, want exactly once per data database", n)
	}
	if countContains(stmts, "PARTITION OF public.data_journal") == 0 {
		t.Error("Apply did not create the journal's open tail partition; the journal is not writable")
	}
	if countContains(stmts, "GRANT SELECT ON public.data_journal TO PUBLIC") == 0 {
		t.Error("Apply did not grant SELECT on the journal; not every role can read it")
	}

	// A capture trigger on every declared table.
	for _, dt := range tables {
		want := "ON " + `"` + dt.Schema + `"."` + dt.Table + `"`
		found := false
		for _, s := range stmts {
			if strings.Contains(s, "CREATE TRIGGER") && strings.Contains(s, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no capture trigger installed on %s.%s; capture must cover every declared table", dt.Schema, dt.Table)
		}
	}
}

// countContains counts the statements that contain sub.
func countContains(stmts []string, sub string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}
