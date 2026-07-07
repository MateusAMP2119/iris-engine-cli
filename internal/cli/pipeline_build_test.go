package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// fakeBuildHandler is a canned api.BuildHandler: the daemon-side build outcome the
// CLI command under test renders.
type fakeBuildHandler struct {
	res api.PipelineBuildResult
	err error
}

func (f fakeBuildHandler) BuildPipeline(_ context.Context, req api.PipelineBuildRequest) (api.PipelineBuildResult, error) {
	if f.err != nil {
		return api.PipelineBuildResult{}, f.err
	}
	res := f.res
	res.Pipeline = req.Pipeline
	return res, nil
}

// TestPipelineBuildCommand proves `iris pipeline build <name>` is the explicit
// build entry point (specification sections 1 and 8): the command POSTs the build
// mutation to the daemon's /pipeline/build route and reports the recorded content
// hash on success (exit 0), so the executed bytes are identifiable from the
// command's own output; a daemon-side build failure is operation-failed (exit 4),
// never a silent success.
//
// spec: S01/build-single-binary-content-hash
func TestPipelineBuildCommand(t *testing.T) {
	const hash = "0beec7b5ea3f0fdbc95d0dd47f3c5bc275da8a33f0fdbc95d0dd47f3c5bc275"

	t.Run("success reports the recorded content hash", func(t *testing.T) {
		sock := shortSocket(t)
		role := api.NewRoleState()
		role.SetLeader()
		mux := api.NewMux(api.WithRole(role), api.WithBuild(fakeBuildHandler{
			res: api.PipelineBuildResult{Hash: hash, SizeBytes: 42},
		}))
		srv := daemon.NewServer(config.Settings{Socket: sock}, mux)
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start in-process daemon: %v", err)
		}
		t.Cleanup(func() { _ = srv.Shutdown() })

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "build", "etl"})
		if code != 0 {
			t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), hash) {
			t.Errorf("stdout does not report the recorded content hash %q:\n%s", hash, out.String())
		}
		if !strings.Contains(out.String(), "etl") {
			t.Errorf("stdout does not name the built pipeline:\n%s", out.String())
		}
	})

	t.Run("daemon-side build failure is operation-failed", func(t *testing.T) {
		sock := shortSocket(t)
		role := api.NewRoleState()
		role.SetLeader()
		mux := api.NewMux(api.WithRole(role), api.WithBuild(fakeBuildHandler{
			err: errors.New("unsupported runtime \"ruby\": no pinned build recipe"),
		}))
		srv := daemon.NewServer(config.Settings{Socket: sock}, mux)
		if err := srv.Start(context.Background()); err != nil {
			t.Fatalf("start in-process daemon: %v", err)
		}
		t.Cleanup(func() { _ = srv.Shutdown() })

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"--socket", sock, "pipeline", "build", "etl"})
		if code != exitOpFailed {
			t.Fatalf("exit = %d, want %d (operation failed)\nstdout: %s\nstderr: %s",
				code, exitOpFailed, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "unsupported runtime") {
			t.Errorf("stderr does not carry the daemon's failure reason:\n%s", errb.String())
		}
	})
}
