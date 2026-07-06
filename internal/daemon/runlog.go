package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file holds the per-run log lifecycle helpers (specification section 2:
// "per-run logs unrotated (bounded output), run-id-keyed under .iris/logs/
// (runs.log_ref), deleted when run row pruned"). A run's log is created unrotated
// under .iris/logs/ keyed by run id (run-<id>.log), its path is the value recorded
// in runs.log_ref, and it is deleted when the run row is pruned. Run rows and
// pruning proper are E05's; this builds the mechanics both drive: the dispatcher
// creates the log and records Ref in runs.log_ref when a run starts, and the
// pruner calls DeleteOnPrune (a RunLogPruneFunc) for each run row it removes.

// RunLogPruneFunc is the delete-on-prune callback the run-record pruner (E05)
// invokes for each run it removes, so a run's per-run log dies with its row without
// the pruner reaching into the daemon's log layout. RunLogWriter.DeleteOnPrune
// satisfies it.
type RunLogPruneFunc func(runID string) error

// RunLogWriter manages per-run log files under the workspace .iris/logs directory,
// keyed by run id. Per-run logs are unrotated (a run's output is bounded); their
// path is what runs.log_ref records and what DeleteOnPrune removes on prune.
type RunLogWriter struct {
	settings config.Settings
}

// NewRunLogWriter builds a RunLogWriter rooted at the settings' workspace .iris
// tree.
func NewRunLogWriter(s config.Settings) *RunLogWriter {
	return &RunLogWriter{settings: s}
}

// Ref returns the per-run log reference for runID: the run-id-keyed path under
// .iris/logs/ recorded in runs.log_ref. It is RunLogPath, the naming convention
// the whole engine shares, so distinct runs never share a log.
func (w *RunLogWriter) Ref(runID string) string {
	return RunLogPath(w.settings, runID)
}

// Create ensures the logs directory and creates (truncating) the unrotated per-run
// log for runID, returning the open file for the dispatcher to stream run output
// into and the reference to record in runs.log_ref. The caller closes the file.
func (w *RunLogWriter) Create(runID string) (*os.File, string, error) {
	if err := EnsureLogsDir(w.settings); err != nil {
		return nil, "", err
	}
	path := w.Ref(runID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, logFilePerm) //nolint:gosec // G304: path is the engine-owned run log under the workspace .iris tree, keyed by an engine-assigned run id, not user or network input.
	if err != nil {
		return nil, "", fmt.Errorf("daemon: create run log %s: %w", path, err)
	}
	return f, path, nil
}

// DeleteOnPrune removes the per-run log for runID. It is the callback the run-record
// pruner invokes when it prunes the run row (a per-run log does not outlive its
// run). An already-absent log is not an error, so a prune is idempotent.
func (w *RunLogWriter) DeleteOnPrune(runID string) error {
	path := w.Ref(runID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: delete pruned run log %s: %w", path, err)
	}
	return nil
}
