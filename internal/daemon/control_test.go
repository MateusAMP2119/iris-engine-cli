package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the leader-side control orchestrator's schema-provisioning and
// composer-destroy interlock paths with fakes and a temp workspace -- no live
// Postgres. It drives the unexported provision and laneMembers directly (internal
// test), so the reserved-schema guard, the unconditional capture-function repair,
// and the DB-sourced member count are each pinned.

// controlDataFake is a dataPlane: it records EnsureCaptureFunction calls and serves a
// fixed live view, so a test can assert what provisioning did without a database.
type controlDataFake struct {
	live        pg.LiveView
	ensureCalls int
	ensureErr   error
	execErr     error
}

func (f *controlDataFake) Exec(context.Context, string) error                { return f.execErr }
func (f *controlDataFake) ReadLiveView(context.Context) (pg.LiveView, error) { return f.live, nil }
func (f *controlDataFake) EnsureCaptureFunction(context.Context) error {
	f.ensureCalls++
	return f.ensureErr
}
func (f *controlDataFake) ExecuteWipe(context.Context, pg.WipeTarget) (pg.WipeResult, error) {
	return pg.WipeResult{}, nil
}
func (f *controlDataFake) OpenUndoRunIDs(context.Context) ([]int64, error) { return nil, nil }
func (f *controlDataFake) ReadFieldGrants(context.Context, string) ([]declare.FieldGrant, error) {
	return nil, nil
}

// controlHeadsFake is a store.AppliedHeadReader over a fixed head map.
type controlHeadsFake struct{ heads map[string]string }

func (f controlHeadsFake) AppliedHeads(context.Context) (map[string]string, error) {
	return f.heads, nil
}

// controlLedgerFake is a pg.LedgerRecorder that records nothing (or a fixed error).
type controlLedgerFake struct{ err error }

func (f controlLedgerFake) RecordMigrationHead(context.Context, pg.MigrationHead) error {
	return f.err
}

// provEmptyLive is a live view where nothing exists yet.
func provEmptyLive() pg.LiveView {
	return pg.LiveView{Schemas: map[string]bool{}, Tables: map[string]bool{}, CaptureTriggers: map[string]bool{}}
}

// writeSchemaTable writes a schemas/<schema>/<table>/table.yaml under ws.
func writeSchemaTable(t *testing.T, ws, schema, table, body string) {
	t.Helper()
	dir := filepath.Join(ws, "schemas", schema, table)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir schema tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "table.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write table.yaml: %v", err)
	}
}

// idOnlyTable renders a minimal valid table.yaml with a single primary-key column.
func idOnlyTable(schema, table string) string {
	return "schema: " + schema + "\ntable: " + table + "\ncolumns:\n  - name: id\n    type: uuid\n    primary_key: true\n"
}

// TestProvisionRejectsPublicSchemaFolder proves provisioning refuses a schemas/public/
// folder: public is engine-reserved, so a declared public schema must be rejected
// before any DDL, not silently merged with the engine's own public objects. Even a
// dry run (which otherwise plans then writes nothing) must reject.
func TestProvisionRejectsPublicSchemaFolder(t *testing.T) {
	t.Run("public-schema-folder-rejected", func(t *testing.T) {
		ws := t.TempDir()
		writeSchemaTable(t, ws, "public", "orders", idOnlyTable("public", "orders"))

		o := newControlOrchestrator(ws, nil, nil, storetest.NewRegistryFake(),
			&controlDataFake{live: provEmptyLive()}, controlLedgerFake{},
			controlHeadsFake{heads: map[string]string{}}, destructiveGate{}, nil)

		err := o.provision(context.Background(), true) // dry run: the reserved guard must fire first.
		if err == nil {
			t.Fatal("provision accepted a schemas/public/ folder; want rejection (public is engine-reserved)")
		}
		if !strings.Contains(err.Error(), "public") || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("rejection %v does not name the reserved public schema", err)
		}
	})
}

// TestProvisionEnsuresCaptureOnEmptyPlan proves provisioning re-ensures iris.capture()
// on every non-dry-run apply, even when the plan is empty: a dropped capture function
// (all tables and triggers already present) must be re-created so the triggers keep
// binding, rather than staying silently broken until something else makes the plan
// non-empty.
func TestProvisionEnsuresCaptureOnEmptyPlan(t *testing.T) {
	t.Run("provision-ensures-capture", func(t *testing.T) {
		ws := t.TempDir()
		writeSchemaTable(t, ws, "sales", "orders", idOnlyTable("sales", "orders"))

		// Live already reflects the declared world, so the plan is empty.
		live := pg.LiveView{
			Schemas:         map[string]bool{"sales": true},
			Tables:          map[string]bool{"sales.orders": true},
			CaptureTriggers: map[string]bool{"sales.orders": true},
			HasJournal:      true,
		}
		data := &controlDataFake{live: live}
		o := newControlOrchestrator(ws, nil, nil, storetest.NewRegistryFake(), data,
			controlLedgerFake{}, controlHeadsFake{heads: map[string]string{"sales.orders": "0001"}}, destructiveGate{}, nil)

		if err := o.provision(context.Background(), false); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if data.ensureCalls == 0 {
			t.Error("provision skipped EnsureCaptureFunction on an empty plan; a dropped iris.capture() would stay unrepaired")
		}
	})
}

// TestComposerInterlockCountsRegisteredFromDB proves the composer-destroy interlock
// counts a lane's registered members from the lanes table in meta, not the workspace
// disk: a member still registered but whose declaration file was deleted from disk
// must still be counted, so the composer is not destroyed while registered members
// remain. The DB-sourced count then drives the interlock to block the destroy.
func TestComposerInterlockCountsRegisteredFromDB(t *testing.T) {
	t.Run("composer-destroy-interlock", func(t *testing.T) {
		ctx := context.Background()
		ws := t.TempDir() // no pipeline declaration files on disk (all deleted).

		reg := storetest.NewRegistryFake()
		reg.Register("extract").Register("transform").Register("load")
		reg.SeedLane("etl", "extract", "transform", "load")

		o := newControlOrchestrator(ws, nil, nil, reg, &controlDataFake{live: provEmptyLive()},
			controlLedgerFake{}, controlHeadsFake{heads: map[string]string{}}, destructiveGate{}, nil)

		members, err := o.laneMembers(ctx, "etl")
		if err != nil {
			t.Fatalf("laneMembers: %v", err)
		}
		if len(members) != 3 {
			t.Fatalf("laneMembers(etl) = %v (%d), want the 3 registered members counted from the lanes table, not the empty disk walk", members, len(members))
		}

		// End-to-end: the DB-sourced members drive the interlock to block the destroy
		// and write nothing to meta.
		rec := storetest.NewWriteRecorder()
		d := dispatch.New(rec)
		d.Start(ctx)
		defer d.Stop()
		destroyer := dispatch.NewDestroyer(reg, d)
		if err := destroyer.DestroyComposer(ctx, "etl", members); err == nil {
			t.Error("composer destroy proceeded with 3 registered members still in the lane; the interlock must block")
		} else if !strings.Contains(err.Error(), "etl") {
			t.Errorf("interlock refusal %v does not name the lane", err)
		}
		if n := len(rec.Statements()); n != 0 {
			t.Errorf("a blocked composer destroy wrote %d meta statements, want 0", n)
		}
	})
}
