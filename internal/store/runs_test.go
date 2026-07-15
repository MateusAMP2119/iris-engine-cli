package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// runsStrPtr returns a pointer to s, for the nullable *string run-record fields.
func runsStrPtr(s string) *string { return &s }

// TestClassifyEnding proves the single dead-lettered terminal: every non-success
// ending -- a run that failed, one that was stopped/cancelled, and one dead-lettered
// because an upstream was -- maps to the one dead_lettered state and parks in the
// dead-letter worklist under its own reason. A three-way non-success set collapsing
// to one terminal state is exactly the "single non-success terminal state" invariant.
func TestClassifyEnding(t *testing.T) {
	cases := []struct {
		name       string
		ending     store.RunEnding
		wantReason store.DeadLetterReason
	}{
		{"failed", store.EndingFailed, store.ReasonFailed},
		{"stopped or cancelled", store.EndingCancelled, store.ReasonStopped},
		{"upstream dead-lettered", store.EndingUpstreamDeadLettered, store.ReasonUpstreamDeadLettered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason, err := store.ClassifyEnding(tc.ending)
			if err != nil {
				t.Fatalf("ClassifyEnding(%q) returned error: %v", tc.ending, err)
			}
			if state != store.RunDeadLettered {
				t.Errorf("ClassifyEnding(%q) state = %q, want the single terminal %q",
					tc.ending, state, store.RunDeadLettered)
			}
			if reason != tc.wantReason {
				t.Errorf("ClassifyEnding(%q) reason = %q, want %q", tc.ending, reason, tc.wantReason)
			}
		})
	}
}

// TestClassifyEndingRejectsUnknown proves the classifier is closed: an ending that
// is not one of the three non-success kinds is rejected loudly rather than silently
// mapped to some default terminal state.
func TestClassifyEndingRejectsUnknown(t *testing.T) {
	if _, _, err := store.ClassifyEnding(store.RunEnding("succeeded")); err == nil {
		t.Fatal("ClassifyEnding of an out-of-set ending = nil error, want a rejection")
	}
}

// createRun run-record fields shared by the write-path tests: a fully-populated
// provenance record whose only variable is the artifact hash (present for a built
// run, nil for a dev run).
func sampleRunRecord(artifactHash *string, upstreams []int64) store.RunRecord {
	return store.RunRecord{
		Pipeline:               "load_orders",
		Cause:                  store.CauseLoop,
		DeclarationChecksum:    "sha256:decl",
		ArtifactHash:           artifactHash,
		SnapshotLSN:            "0/16A3D2F0",
		JournalFloor:           81,
		ConsumedUpstreamRunIDs: upstreams,
	}
}

// createStmt returns the single run-create statement the write path committed,
// failing the test unless CreateRun issued exactly one atomic transaction of one
// statement (the runs insert and its run_inputs rows are one CTE, never split).
func createStmt(t *testing.T, rec *storetest.WriteRecorder) storetest.RecordedStatement {
	t.Helper()
	txns := rec.Transactions()
	if len(txns) != 1 {
		t.Fatalf("CreateRun committed %d transactions, want exactly one atomic batch", len(txns))
	}
	if len(txns[0]) != 1 {
		t.Fatalf("run-create transaction has %d statements, want one CTE statement", len(txns[0]))
	}
	return txns[0][0]
}

// The documented positional argument order of the run-create statement. Both the
// test and the writer pin this order; changing it changes both.
const (
	argPipeline    = 0
	argState       = 1
	argCause       = 2
	argReplayed    = 3
	argArtifact    = 4
	argDeclaration = 5
	argSnapshotLSN = 6
	argJournalLow  = 7
	argUpstreams   = 8
)

// TestCreateRunRecordsHashes proves every run row records the declaration checksum,
// the binary hash (artifact_hash), and the consumed upstream run ids: the write path
// stamps all three in one atomic run-create transaction -- the runs row carrying the
// declaration checksum and artifact hash, and one run_inputs row per consumed
// upstream run id, committed together with the runs insert.
func TestCreateRunRecordsHashes(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	upstreams := []int64{39, 40}
	if err := w.CreateRun(context.Background(), sampleRunRecord(runsStrPtr("hashXYZ"), upstreams)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	stmt := createStmt(t, rec)
	if !strings.Contains(stmt.SQL, "INSERT INTO runs") {
		t.Errorf("run-create statement does not insert the runs row:\n%s", stmt.SQL)
	}
	if !strings.Contains(stmt.SQL, "INSERT INTO run_inputs") {
		t.Errorf("run-create statement does not insert run_inputs rows:\n%s", stmt.SQL)
	}
	if !strings.Contains(stmt.SQL, "declaration_checksum") {
		t.Errorf("run-create statement omits declaration_checksum:\n%s", stmt.SQL)
	}
	if got := stmt.Args[argDeclaration]; got != "sha256:decl" {
		t.Errorf("declaration_checksum arg = %v, want %q", got, "sha256:decl")
	}
	if got := stmt.Args[argArtifact]; got != "hashXYZ" {
		t.Errorf("artifact_hash arg = %v, want %q", got, "hashXYZ")
	}
	if got := stmt.Args[argUpstreams]; !reflect.DeepEqual(got, upstreams) {
		t.Errorf("consumed upstream run ids arg = %v, want %v", got, upstreams)
	}
}

// TestCreateRunDevRunNullArtifactHash proves a dev run's runs row records
// artifact_hash as SQL NULL: with no built artifact, the write path binds an untyped
// nil (not a typed nil pointer, which pgx would reject), so the column is NULL rather
// than an empty or zero hash.
func TestCreateRunDevRunNullArtifactHash(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.CreateRun(context.Background(), sampleRunRecord(nil, nil)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	stmt := createStmt(t, rec)
	if got := stmt.Args[argArtifact]; got != nil {
		t.Errorf("dev-run artifact_hash arg = %#v, want a nil binding (SQL NULL)", got)
	}
}

// TestRunsTableShape proves the runs table model's shape column-for-column and
// constraint-for-constraint, and that the run-create write path stamps only
// columns the model declares. The model is the single source the
// DDL renders from, so a shape assertion here is a DDL-shape assertion.
func TestRunsTableShape(t *testing.T) {
	runs := tableByName(t, store.MetaSchema(), "runs")

	// id: bigint identity, and the sole primary key.
	id := columnByName(t, runs, "id")
	if id.Type != "bigint" || !id.Identity {
		t.Errorf("runs.id = {type:%q identity:%v}, want a bigint identity", id.Type, id.Identity)
	}
	if !reflect.DeepEqual(runs.PrimaryKey, []string{"id"}) {
		t.Errorf("runs primary key = %v, want [id]", runs.PrimaryKey)
	}

	// The nullable columns: everything but the identity, pipeline, state, cause,
	// declaration_checksum, and recorded_at is nullable.
	nullable := map[string]bool{
		"replayed_from": true, "exit_code": true, "handle": true,
		"artifact_hash": true, "log_ref": true, "snapshot_lsn": true,
		"journal_floor": true, "journal_ceiling": true,
	}
	required := map[string]bool{
		"pipeline": true, "state": true, "cause": true,
		"declaration_checksum": true, "recorded_at": true,
	}
	for name, wantNullable := range nullable {
		if col := columnByName(t, runs, name); col.Nullable != wantNullable {
			t.Errorf("runs.%s nullable = %v, want %v", name, col.Nullable, wantNullable)
		}
	}
	for name := range required {
		if col := columnByName(t, runs, name); col.Nullable {
			t.Errorf("runs.%s is nullable, want NOT NULL", name)
		}
	}

	// The value-set CHECK constraints: state and cause are closed enums.
	wantChecks := map[string][]string{
		"state": {"queued", "running", "succeeded", "dead_lettered"},
		"cause": {"manual", "loop", "replay", "propagated"},
	}
	gotChecks := map[string][]string{}
	for _, c := range runs.Checks {
		gotChecks[c.Column] = c.Values
	}
	for col, want := range wantChecks {
		if got := gotChecks[col]; !reflect.DeepEqual(got, want) {
			t.Errorf("runs CHECK on %s = %v, want %v", col, got, want)
		}
	}

	// The foreign keys: pipeline, the replayed_from self-FK, and artifact_hash.
	wantFKs := map[string]string{
		"pipeline":      "pipelines.name",
		"replayed_from": "runs.id",
		"artifact_hash": "artifacts.hash",
	}
	gotFKs := map[string]string{}
	for _, fk := range runs.ForeignKeys {
		gotFKs[fk.Column] = fk.RefTable + "." + fk.RefColumn
	}
	for col, want := range wantFKs {
		if got := gotFKs[col]; got != want {
			t.Errorf("runs FK on %s -> %s, want %s", col, got, want)
		}
	}

	// The (pipeline, id) secondary index.
	var haveIndex bool
	for _, idx := range runs.Indexes {
		if reflect.DeepEqual(idx.Columns, []string{"pipeline", "id"}) {
			haveIndex = true
		}
	}
	if !haveIndex {
		t.Errorf("runs is missing the (pipeline, id) index; indexes = %v", runs.Indexes)
	}

	// Write-path conformance: CreateRun stamps only declared runs columns.
	declared := map[string]bool{}
	for _, c := range runs.Columns {
		declared[c.Name] = true
	}
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)
	if err := w.CreateRun(context.Background(), sampleRunRecord(runsStrPtr("h"), []int64{7})); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stmt := createStmt(t, rec)
	for _, col := range []string{"pipeline", "state", "cause", "declaration_checksum", "artifact_hash", "snapshot_lsn", "journal_floor", "recorded_at"} {
		if !declared[col] {
			t.Errorf("write path stamps column %q not declared on the runs model", col)
		}
		if !strings.Contains(stmt.SQL, col) {
			t.Errorf("run-create statement does not stamp declared column %q:\n%s", col, stmt.SQL)
		}
	}
}
