package pg_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// This file proves the fault-recovery idempotency of the provisioning DDL
// (specification section 5, "a failure between them is reconciled by
// re-provisioning, which is idempotent"): re-applying a plan against a database a
// partial prior run already touched must be a no-op, never an "already exists"
// abort. It exercises the real rendered ADD COLUMN and capture-trigger DDL through
// a stateful fake that models the Postgres object-existence errors the findings
// turn on -- no live Postgres (S16/integration-fakes-interfaces).

// quotedIdent matches a double-quoted SQL identifier, so the fake can key an ADD
// COLUMN or trigger statement by the objects it names.
var quotedIdent = regexp.MustCompile(`"([^"]*)"`)

// fakePG is a pg.DB that models the Postgres existence semantics the idempotency
// findings depend on: a plain ADD COLUMN (no IF NOT EXISTS) of a column that
// already exists, and a plain CREATE TRIGGER of a trigger that already exists, both
// fail exactly as a partial prior provisioning run would make them fail. The IF NOT
// EXISTS ADD COLUMN and the DROP TRIGGER IF EXISTS + CREATE TRIGGER forms are
// idempotent no-ops on an existing object. Every other statement (CREATE SCHEMA,
// CREATE TABLE, journal DDL) is accepted. A compound statement (the simple-protocol
// "DROP ...; CREATE ...;" a no-arg Exec runs) is split and each part applied, like
// pgx's no-argument Exec against a real cluster.
type fakePG struct {
	columns  map[string]bool
	triggers map[string]bool
}

func newFakePG() *fakePG {
	return &fakePG{columns: map[string]bool{}, triggers: map[string]bool{}}
}

var _ pg.DB = (*fakePG)(nil)

// Exec applies one (possibly compound) statement, modeling the object-existence
// errors a re-apply against a partially provisioned database would hit.
func (f *fakePG) Exec(_ context.Context, sql string) error {
	for _, stmt := range strings.Split(sql, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := f.execOne(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakePG) execOne(stmt string) error {
	idents := quotedIdent.FindAllStringSubmatch(stmt, -1)
	names := make([]string, 0, len(idents))
	for _, m := range idents {
		names = append(names, m[1])
	}
	switch {
	case strings.Contains(stmt, "DROP TRIGGER IF EXISTS"):
		if len(names) > 0 {
			delete(f.triggers, names[0]) // safe whether or not it exists.
		}
		return nil
	case strings.Contains(stmt, "CREATE TRIGGER"):
		if len(names) == 0 {
			return errors.New("fakePG: CREATE TRIGGER with no identifier")
		}
		name := names[0]
		if f.triggers[name] {
			return errors.New(`fakePG: trigger "` + name + `" already exists`)
		}
		f.triggers[name] = true
		return nil
	case strings.Contains(stmt, "ADD COLUMN"):
		// idents = schema, table, column (IF NOT EXISTS carries no quoted token).
		if len(names) < 3 {
			return errors.New("fakePG: ADD COLUMN with too few identifiers: " + stmt)
		}
		key := names[0] + "." + names[1] + "." + names[2]
		ifNotExists := strings.Contains(stmt, "IF NOT EXISTS")
		if f.columns[key] && !ifNotExists {
			return errors.New(`fakePG: column "` + key + `" already exists`)
		}
		f.columns[key] = true
		return nil
	default:
		return nil // CREATE SCHEMA / CREATE TABLE / journal DDL: accepted.
	}
}

// TestProvisionReplayIdempotentAfterHeadRecordFailure proves the ADD COLUMN replay
// is re-appliable after a head-record failure: the data ALTER runs before the meta
// head is recorded, so a RecordMigrationHead failure leaves the column present with
// the head unrecorded. The next apply replays the same migration; without an
// idempotent ADD COLUMN it fails "column already exists" and the state is
// unrecoverable. The replay must instead be a no-op.
//
// spec: S05/provision-idempotent
func TestProvisionReplayIdempotentAfterHeadRecordFailure(t *testing.T) {
	ctx := context.Background()
	alter, err := pg.RenderAddColumn("analytics", "orders", declare.MigrationColumn{Name: "status", Type: "text", Default: "'pending'"})
	if err != nil {
		t.Fatalf("RenderAddColumn: %v", err)
	}
	plan := pg.ProvisionPlan{Tables: []pg.TableProvision{{
		Schema: "analytics", Table: "orders",
		Branch: pg.ReplayPending{Migrations: []pg.PendingMigration{{
			Alter: alter,
			Head:  pg.MigrationHead{Schema: "analytics", Table: "orders", MigrationID: "0002", Parent: "0001", Checksum: "deadbeef"},
		}}},
	}}}

	db := newFakePG()
	// First apply: the column lands, then the meta head-record fails -- the exact
	// split-failure the finding describes (data written, head not recorded).
	boom := errors.New("meta head-record failed")
	if err := plan.Apply(ctx, db, &recordingLedger{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("first apply error = %v, want the injected head-record failure", err)
	}
	// Re-apply the identical plan (head still unrecorded): the ALTER replays against a
	// database that already has the column, and must be a no-op rather than aborting.
	if err := plan.Apply(ctx, db, &recordingLedger{}); err != nil {
		t.Fatalf("re-apply after a head-record failure errored (non-idempotent ADD COLUMN): %v", err)
	}
}

// TestProvisionCaptureTriggersReapplyIdempotent proves re-applying the capture
// triggers is idempotent: a partial prior run leaves one or more of the three
// triggers installed, so a plain CREATE TRIGGER re-apply fails "trigger already
// exists". The rendered DDL must drop-if-exists then create so a re-apply always
// makes progress.
//
// spec: S05/provision-idempotent
func TestProvisionCaptureTriggersReapplyIdempotent(t *testing.T) {
	ctx := context.Background()
	plan := pg.ProvisionPlan{Tables: []pg.TableProvision{{
		Schema:          "analytics",
		Table:           "orders",
		Branch:          pg.ReplayPending{}, // existing table, nothing pending.
		CaptureTriggers: pg.RenderCaptureTriggers("analytics", "orders"),
	}}}

	db := newFakePG()
	if err := plan.Apply(ctx, db, &recordingLedger{}); err != nil {
		t.Fatalf("first capture-trigger install: %v", err)
	}
	// Re-apply: the triggers are already present; re-installing must not fail.
	if err := plan.Apply(ctx, db, &recordingLedger{}); err != nil {
		t.Fatalf("re-apply of capture triggers errored (non-idempotent CREATE TRIGGER): %v", err)
	}
}
