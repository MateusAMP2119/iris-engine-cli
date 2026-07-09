package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// fakePromoteHandler is a canned api.PromoteHandler: the daemon-side promote
// outcome the CLI command under test renders.
type fakePromoteHandler struct {
	res api.PipelinePromoteResult
	err error
}

func (f fakePromoteHandler) PromotePipeline(_ context.Context, req api.PipelinePromoteRequest) (api.PipelinePromoteResult, error) {
	if f.err != nil {
		return api.PipelinePromoteResult{}, f.err
	}
	res := f.res
	res.Pipeline = req.Pipeline
	return res, nil
}

// startPromoteDaemon serves a leader mux wired to h on a unix socket.
func startPromoteDaemon(t *testing.T, h api.PromoteHandler) string {
	t.Helper()
	sock := shortSocket(t)
	role := api.NewRoleState()
	role.SetLeader()
	mux := api.NewMux(api.WithRole(role), api.WithPromote(h))
	srv := daemon.NewServer(config.Settings{Socket: sock}, mux)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start in-process daemon: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
	return sock
}

// TestPipelinePromoteCommand proves `iris pipeline promote <name>` marks the
// pipeline's data permanent only when the pipeline is built (specification
// sections 1 and 8): a successful promote reports the permanent data mode and
// exits 0; the daemon-side built-gate refusal for a source-only pipeline is
// operation-failed (exit 4) carrying the refusal, never a silent success.
//
// spec: S01/promote-requires-built
func TestPipelinePromoteCommand(t *testing.T) {
	t.Run("success reports the permanent data mode", func(t *testing.T) {
		sock := startPromoteDaemon(t, fakePromoteHandler{
			res: api.PipelinePromoteResult{DataMode: "permanent"},
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "promote", "etl"})
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "etl") || !strings.Contains(out.String(), "permanent") {
			t.Errorf("stdout does not report the pipeline's permanent data mode:\n%s", out.String())
		}
	})

	t.Run("built-gate refusal is operation-failed", func(t *testing.T) {
		sock := startPromoteDaemon(t, fakePromoteHandler{
			err: errors.New(`dispatch: promote "etl" refused: pipeline is not in built state; permanent data requires a built artifact`),
		})
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "promote", "etl"})
		if code != exitOpFailed {
			t.Fatalf("exit = %d, want %d (operation failed)\nstdout: %s\nstderr: %s",
				code, exitOpFailed, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "built") {
			t.Errorf("stderr does not carry the built-gate refusal:\n%s", errb.String())
		}
	})
}

// TestPipelinePromoteRepeatsWarning proves the promote command surfaces the
// repeated cross-mode read warning while an upstream read dependency is still
// disposable (specification section 5): human output prints the advisory to
// stderr alongside the success, and under --json the warning rides the data
// envelope -- the warning accompanies the outcome, never blocks it.
//
// spec: S05/promote-repeats-cross-mode-warning
func TestPipelinePromoteRepeatsWarning(t *testing.T) {
	handler := fakePromoteHandler{
		res: api.PipelinePromoteResult{
			DataMode: "permanent",
			Warnings: []declare.Warning{{
				Kind:    declare.WarnCrossModeRead,
				Table:   "raw_orders",
				Message: "permanent-data pipeline reads disposable-mode upstream raw_orders",
			}},
		},
	}

	t.Run("human output prints the warning to stderr", func(t *testing.T) {
		sock := startPromoteDaemon(t, handler)
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "promote", "etl"})
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (a warning never blocks the promote)\nstderr: %s", code, errb.String())
		}
		if !strings.Contains(errb.String(), "warning") || !strings.Contains(errb.String(), "raw_orders") {
			t.Errorf("stderr does not carry the cross-mode read warning:\n%s", errb.String())
		}
		if !strings.Contains(out.String(), "permanent") {
			t.Errorf("stdout does not report the promote outcome:\n%s", out.String())
		}
	})

	t.Run("the warning rides the --json envelope", func(t *testing.T) {
		sock := startPromoteDaemon(t, handler)
		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "--json", "pipeline", "promote", "etl"})
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
		}
		var doc struct {
			Data api.PipelinePromoteResult `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("stdout is not one JSON data envelope: %v\n%s", err, out.String())
		}
		if doc.Data.DataMode != "permanent" {
			t.Errorf("envelope data mode = %q, want permanent", doc.Data.DataMode)
		}
		if len(doc.Data.Warnings) != 1 || doc.Data.Warnings[0].Kind != declare.WarnCrossModeRead {
			t.Errorf("envelope does not carry the cross-mode read warning: %+v", doc.Data.Warnings)
		}
	})
}
