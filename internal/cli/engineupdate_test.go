package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/buildinfo"
	"github.com/MateusAMP2119/iris-engine-cli/internal/update"
)

// TestEngineUpdateDevBuildRefuses proves `iris engine update` refuses to self-
// replace an unstamped "dev" build -- the build that was not installed from a
// release -- with exit 4 (operation failed) and installer guidance on stderr, and
// makes no network or filesystem attempt (the dev guard returns before any I/O).
// The test build is itself unstamped, so buildinfo.Version is "dev" and the real
// handler drives the real refusal path with no injection.
//
// spec: S08/update-dev-build-refuses
func TestEngineUpdateDevBuildRefuses(t *testing.T) {
	if buildinfo.Version != "dev" {
		t.Skipf("build is stamped %q, not the unstamped dev default this path exercises", buildinfo.Version)
	}
	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"engine", "update"})
	if code != exitOpFailed {
		t.Fatalf("exit = %d, want %d (dev build refused)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "install") {
		t.Errorf("dev-build refusal missing installer guidance on stderr:\n%s", errb.String())
	}
}

// TestEngineUpdateUpToDate proves `iris engine update` reports already-up-to-date
// and exits 0 when the resolved latest tag equals the running version, without
// replacing anything. The update engine is injected so the decision is exercised
// with no network or filesystem I/O.
//
// spec: S08/update-tag-equals-up-to-date
func TestEngineUpdateUpToDate(t *testing.T) {
	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	a.runUpdate = func(_ context.Context, current string) (update.Result, error) {
		return update.Result{Status: update.StatusUpToDate, From: current, To: "v1.2.3"}, nil
	}
	code := a.run([]string{"engine", "update"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (already up to date)\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
	}
	if s := out.String(); !strings.Contains(s, "up to date") {
		t.Errorf("up-to-date message missing from stdout:\n%s", s)
	}
}
