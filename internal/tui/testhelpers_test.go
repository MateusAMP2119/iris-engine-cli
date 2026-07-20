package tui

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// TestMain pins the basic 16-color palette so SGR golden/string assertions stay
// stable regardless of the developer's COLORTERM. Individual tests may call
// applyPalette(true) and restore.
func TestMain(m *testing.M) {
	applyPalette(false)
	os.Exit(m.Run())
}

// psFixture is the canned readout shared by tui tests.
func psFixture() api.PsPayload {
	return api.PsPayload{
		Engine: api.PsEngine{
			Version: "dev", Role: "leader", PID: 4242, Uptime: "1h2m3s",
			QueuedRuns: 1, RunningRuns: 1,
			Load: &api.PsLoad{CPUPercent: 2.5, RSSBytes: 150 << 20},
		},
		Runs: []api.PsRun{
			{ID: "8", Pipeline: "load_orders", Lane: "ingest", State: "queued"},
			{ID: "7", Pipeline: "extract", Lane: "ingest", State: "running",
				Load: &api.PsLoad{CPUPercent: 51.0, RSSBytes: 24 << 20}},
		},
	}
}

// shortSocket returns a short temp unix socket path for tests.
func shortSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "iris.sock")
}

// unixClient builds a Client dialing a unix socket (same shape as the CLI).
func unixClient(sock string) *Client {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return NewClient(hc, "http://iris", "", false, "unix://"+sock)
}

// psFunc adapts a function to api.PsHandler.
type psFunc func(ctx context.Context, all, history bool) (api.PsPayload, error)

func (f psFunc) Ps(ctx context.Context, all, history bool) (api.PsPayload, error) {
	return f(ctx, all, history)
}
