package dispatch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// The documented positional argument order of the run-create statement, mirrored
// from the store writer so the dispatch stamping tests can assert the stamped pin
// values by position.
const (
	argSnapshotLSN = 6
	argJournalLow  = 7
)

// fakeLSNReader is a dispatch.LSNReader returning a fixed data-database LSN, standing
// in for a real data-database read with no live Postgres.
type fakeLSNReader struct {
	lsn string
	err error
}

func (f fakeLSNReader) CurrentLSN(context.Context) (string, error) { return f.lsn, f.err }

// fakeJournalHigh is a dispatch.JournalHighWatermark handing out the next value in a
// scripted sequence on each read, so a test can prove the floor comes from the read
// at dispatch and the ceiling from a distinct, later read at the terminal transition.
type fakeJournalHigh struct {
	values []int64
	n      int
}

func (f *fakeJournalHigh) JournalHighID(context.Context) (int64, error) {
	v := f.values[f.n]
	f.n++
	return v, nil
}

// dispatchRecord is the provenance-bearing run record a dispatch supplies before the
// pin values are stamped from the data-database reads.
func dispatchRecord() store.RunRecord {
	return store.RunRecord{
		Pipeline:            "load_orders",
		Cause:               store.CauseLoop,
		DeclarationChecksum: "sha256:decl",
		ArtifactHash:        nil,
	}
}

// TestStampDispatchRecordsSnapshotLSN proves dispatch records the data-database LSN
// into the run's snapshot_lsn at dispatch time: StampDispatch reads the current LSN
// and passes it into the run-create write path, so the created runs row carries it.
func TestStampDispatchRecordsSnapshotLSN(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	lsn := fakeLSNReader{lsn: "0/16A3D2F0"}
	jh := &fakeJournalHigh{values: []int64{81}}

	if err := dispatch.StampDispatch(context.Background(), w, lsn, jh, dispatchRecord()); err != nil {
		t.Fatalf("StampDispatch: %v", err)
	}

	txns := rec.Transactions()
	if len(txns) != 1 || len(txns[0]) != 1 {
		t.Fatalf("StampDispatch committed %v, want one atomic run-create statement", txns)
	}
	if got := txns[0][0].Args[argSnapshotLSN]; got != "0/16A3D2F0" {
		t.Errorf("snapshot_lsn arg = %v, want the LSN read at dispatch %q", got, "0/16A3D2F0")
	}
}

// TestStampJournalWindow proves the run's journal window is stamped at its two
// boundaries: journal_floor is the journal high id read at dispatch and
// journal_ceiling is the journal high id read again at the terminal transition. A
// scripted watermark hands out 81 then 95, so the floor stamped at create and the
// ceiling stamped at terminal come from distinct, ordered reads.
func TestStampJournalWindow(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	lsn := fakeLSNReader{lsn: "0/16A3D2F0"}
	jh := &fakeJournalHigh{values: []int64{81, 95}}

	if err := dispatch.StampDispatch(context.Background(), w, lsn, jh, dispatchRecord()); err != nil {
		t.Fatalf("StampDispatch: %v", err)
	}
	// journal_floor: the journal high id read at dispatch.
	if got := rec.Transactions()[0][0].Args[argJournalLow]; got != int64(81) {
		t.Errorf("journal_floor arg = %v, want the dispatch-time journal high id 81", got)
	}

	if err := dispatch.StampTerminal(context.Background(), w, jh, "42"); err != nil {
		t.Fatalf("StampTerminal: %v", err)
	}
	// journal_ceiling: the journal high id read again at the terminal transition.
	ceiling := findCeilingStmt(t, rec)
	var found bool
	for _, a := range ceiling.Args {
		if a == int64(95) {
			found = true
		}
	}
	if !found {
		t.Errorf("journal_ceiling statement %q did not stamp the terminal journal high id 95 (args %v)",
			ceiling.SQL, ceiling.Args)
	}
}

// findCeilingStmt returns the single statement that stamps journal_ceiling, failing
// the test when the terminal stamp did not issue exactly one such write.
func findCeilingStmt(t *testing.T, rec *storetest.WriteRecorder) storetest.RecordedStatement {
	t.Helper()
	var hits []storetest.RecordedStatement
	for _, s := range rec.Statements() {
		if strings.Contains(s.SQL, "journal_ceiling") {
			hits = append(hits, s)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("found %d journal_ceiling statements, want exactly one terminal stamp", len(hits))
	}
	return hits[0]
}

// TestPinRecordedDispatchTerminal proves that at dispatch a run records the
// data database LSN as snapshot_lsn and the journal high id as journal_floor,
// and at terminal transition records the journal high id again as journal_ceiling.
// The test uses fakes for the data-database reads (LSN and journal watermark)
// and a recording meta writer, exercising the dispatch/store seam with no live
// Postgres: the integration tier for the snapshot pin contract.
func TestPinRecordedDispatchTerminal(t *testing.T) {
	t.Run("pin-recorded-dispatch-terminal", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		lsn := fakeLSNReader{lsn: "0/DEAD BEEF"}
		jh := &fakeJournalHigh{values: []int64{81, 95}}

		if err := dispatch.StampDispatch(context.Background(), w, lsn, jh, dispatchRecord()); err != nil {
			t.Fatalf("StampDispatch: %v", err)
		}
		tx := rec.Transactions()[0][0]
		if got := tx.Args[argSnapshotLSN]; got != "0/DEAD BEEF" {
			t.Errorf("snapshot_lsn = %v, want LSN read at dispatch", got)
		}
		if got := tx.Args[argJournalLow]; got != int64(81) {
			t.Errorf("journal_floor = %v, want journal high id at dispatch", got)
		}

		if err := dispatch.StampTerminal(context.Background(), w, jh, "42"); err != nil {
			t.Fatalf("StampTerminal: %v", err)
		}
		ceiling := findCeilingStmt(t, rec)
		var found bool
		for _, a := range ceiling.Args {
			if a == int64(95) {
				found = true
			}
		}
		if !found {
			t.Errorf("journal_ceiling statement did not receive terminal id 95 (args %v)", ceiling.Args)
		}
	})
}
