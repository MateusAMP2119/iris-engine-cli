package pgtest_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg/pgtest"
)

// canonicalDDL is a representative CREATE / ALTER / GRANT / trigger DDL sequence
// -- the exact shapes E03/E04 will issue through the pg seam -- built from the
// golden analytics.orders table of specification section 5. The recording fake
// must capture these statements verbatim and in order.
var canonicalDDL = []string{
	`CREATE TABLE analytics.orders (
    id          uuid        PRIMARY KEY,
    customer_id uuid        NOT NULL,
    amount      numeric,
    created_at  timestamptz DEFAULT now()
);`,
	`ALTER TABLE analytics.orders ADD COLUMN status text DEFAULT 'pending';`,
	`GRANT SELECT ON analytics.orders TO iris_read_analytics_orders;`,
	`CREATE TRIGGER iris_capture_analytics_orders
    AFTER INSERT OR UPDATE OR DELETE ON analytics.orders
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();`,
}

// TestRecorderSatisfiesDB proves the recording pg fake stands behind the data
// database seam: it implements pg.DB, so DDL/grant reconcile code runs against
// it with no live Postgres.
//
// spec: S16/integration-fakes-interfaces
func TestRecorderSatisfiesDB(t *testing.T) {
	var db pg.DB = pgtest.New()
	if db == nil {
		t.Fatal("pgtest.New() does not satisfy pg.DB")
	}
}

// TestRecorderCapturesDDLGolden issues the canonical CREATE/ALTER/GRANT/trigger
// sequence through the pg seam and asserts the recording -- the exact statements,
// in order -- byte-for-byte against a checked-in golden. This is the seam by
// which E03/E04 prove their generated DDL without a live database: a golden diff
// is a contract diff.
//
// spec: S16/integration-fakes-interfaces
func TestRecorderCapturesDDLGolden(t *testing.T) {
	ctx := context.Background()
	rec := pgtest.New()

	for _, stmt := range canonicalDDL {
		if err := rec.Exec(ctx, stmt); err != nil {
			t.Fatalf("Exec: %v", err)
		}
	}

	// The recording preserves every statement, in issue order.
	got := rec.Statements()
	if len(got) != len(canonicalDDL) {
		t.Fatalf("recorded %d statements, want %d", len(got), len(canonicalDDL))
	}
	for i := range got {
		if got[i] != canonicalDDL[i] {
			t.Errorf("statement %d not captured verbatim:\n got %q\nwant %q", i, got[i], canonicalDDL[i])
		}
	}

	// The dumped recording is diffed byte-for-byte against the golden.
	golden.Assert(t, rec.Dump(), filepath.Join("testdata", "orders_ddl.recording.sql"))
}

// TestRecorderErrorInjection proves the fake can model a failing data database:
// an injected error surfaces from Exec, and the offending statement is still
// recorded so a test can assert what was attempted before the failure.
//
// spec: S16/integration-fakes-interfaces
func TestRecorderErrorInjection(t *testing.T) {
	ctx := context.Background()
	rec := pgtest.New()

	boom := errors.New("permission denied for schema analytics")
	rec.FailWith(boom)

	err := rec.Exec(ctx, `CREATE TABLE analytics.orders (id uuid);`)
	if !errors.Is(err, boom) {
		t.Errorf("Exec err = %v, want the injected error", err)
	}
	if got := rec.Statements(); len(got) != 1 {
		t.Errorf("attempted statement not recorded on failure: %v", got)
	}
}
