package daemon_test

import (
	"context"
	"os"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// TestPerRunLogLifecycle proves the per-run log lifecycle helpers (specification
// section 2): each run's log is created unrotated under .iris/logs/ keyed by run
// id (run-<id>.log), its reference is the value recorded in runs.log_ref, and it
// is deleted when the run row is pruned. Run rows and pruning proper are E05's;
// this builds and proves the log-file lifecycle mechanics (create / reference /
// delete-on-prune) that the dispatcher and the pruner drive. The store fake
// sources a real run id so the log is keyed exactly as the run row is.
func TestPerRunLogLifecycle(t *testing.T) {
	// spec: S02/per-run-log-lifecycle
	t.Run("S02/per-run-log-lifecycle", func(t *testing.T) {
		ws := t.TempDir()
		s := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{}, config.Layer{})

		// A real run id from the meta-store fake: the per-run log is keyed by it.
		fake := storetest.New()
		run, err := fake.CreateRun(context.Background(), store.RunSpec{Pipeline: "load", Lane: "ingest"})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		w := daemon.NewRunLogWriter(s)

		t.Run("create makes an unrotated run-<id>.log and returns its log_ref", func(t *testing.T) {
			f, ref, err := w.Create(run.ID)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := f.WriteString("run output\n"); err != nil {
				t.Fatalf("write run log: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close run log: %v", err)
			}
			// The reference the run row records (runs.log_ref) is the canonical
			// run-id-keyed path, unrotated, under .iris/logs/.
			if ref != daemon.RunLogPath(s, run.ID) {
				t.Errorf("Create ref = %q, want RunLogPath %q", ref, daemon.RunLogPath(s, run.ID))
			}
			if _, err := os.Stat(daemon.RunLogPath(s, run.ID)); err != nil {
				t.Errorf("run log not created at its keyed path: %v", err)
			}
		})

		t.Run("reference is stable and run-id-keyed", func(t *testing.T) {
			if got := w.Ref(run.ID); got != daemon.RunLogPath(s, run.ID) {
				t.Errorf("Ref(%s) = %q, want %q", run.ID, got, daemon.RunLogPath(s, run.ID))
			}
			// Distinct runs get distinct references (no cross-run clobbering).
			if w.Ref("run-1") == w.Ref("run-2") {
				t.Error("distinct run ids resolved to the same log reference")
			}
		})

		t.Run("delete-on-prune removes the run log when the run row is pruned", func(t *testing.T) {
			// The E05 pruner deletes a run row and calls the delete-on-prune callback
			// for it; model exactly that here over the RunLogPruneFunc seam.
			var prune daemon.RunLogPruneFunc = w.DeleteOnPrune

			path := daemon.RunLogPath(s, run.ID)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("precondition: run log should exist before prune: %v", err)
			}
			if err := prune(run.ID); err != nil {
				t.Fatalf("DeleteOnPrune: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("run log survived prune (stat err=%v)", err)
			}
			// Prune is idempotent: deleting an already-pruned run's log is not an error.
			if err := prune(run.ID); err != nil {
				t.Errorf("second DeleteOnPrune on an absent log errored: %v", err)
			}
		})
	})
}
