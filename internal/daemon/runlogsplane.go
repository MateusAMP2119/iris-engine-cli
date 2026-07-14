package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// This file is the daemon's run-logs plane: the api.RunLogsHandler behind GET
// /runs/{id}/logs and `iris run logs`. A run's captured stdout/stderr lives in
// the run-id-keyed file under this node's workspace .iris/logs (the sink the run
// paths stream into and runs.log_ref records), so the plane serves the local
// file -- a read, served on any role. A run that executed on another host keeps
// its log on that host; this node then answers honestly that it holds no
// captured output for the run.

// runLogsPlane implements api.RunLogsHandler over the per-run log writer's
// naming convention.
type runLogsPlane struct {
	logs *RunLogWriter
}

// compile-time proof the plane satisfies the mux's run-logs seam.
var _ api.RunLogsHandler = runLogsPlane{}

// NewRunLogsPlane wires the run-logs handler over the per-run log writer.
func NewRunLogsPlane(logs *RunLogWriter) api.RunLogsHandler {
	return runLogsPlane{logs: logs}
}

// Logs opens the run's captured output for streaming.
func (p runLogsPlane) Logs(_ context.Context, id string) (io.ReadCloser, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, fmt.Errorf("run logs: %q is not a run id", id)
	}
	f, err := os.Open(p.logs.Ref(id)) //nolint:gosec // G304: the path is the engine-owned run log under the workspace .iris tree, keyed by a validated numeric run id.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("run logs: no captured output for run %s on this node (the run may predate output capture, its log was pruned with the run, or it executed on another host)", id)
		}
		return nil, fmt.Errorf("run logs: open captured output for run %s: %w", id, err)
	}
	return f, nil
}
