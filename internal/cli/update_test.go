package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/buildinfo"
	"github.com/MateusAMP2119/iris-lakehouse/internal/update"
)

// TestUpdateDevBuildRefuses proves `iris update` refuses to self-replace an
// unstamped "dev" build -- the build that was not installed from a release --
// with exit 4 (operation failed) and installer guidance on stderr, and makes no
// network or filesystem attempt (the dev guard returns before any I/O). The test
// build is itself unstamped, so buildinfo.Version is "dev" and the real handler
// drives the real refusal path with no injection.
func TestUpdateDevBuildRefuses(t *testing.T) {
	if buildinfo.Version != "dev" {
		t.Skipf("build is stamped %q, not the unstamped dev default this path exercises", buildinfo.Version)
	}
	var out, errb bytes.Buffer
	code := newApp(&out, &errb).run([]string{"update"})
	if code != exitOpFailed {
		t.Fatalf("exit = %d, want %d (dev build refused)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "install") {
		t.Errorf("dev-build refusal missing installer guidance on stderr:\n%s", errb.String())
	}
}

// TestUpdateUpToDate proves `iris update` reports already-up-to-date and exits 0
// when the resolved latest tag equals the running version, without replacing
// anything. The update engine is injected so the decision is exercised with no
// network or filesystem I/O.
func TestUpdateUpToDate(t *testing.T) {
	var out, errb bytes.Buffer
	a := newApp(&out, &errb)
	a.runUpdate = func(_ context.Context, current string, _ bool) (update.Result, error) {
		return update.Result{Status: update.StatusUpToDate, From: current, To: "v1.2.3"}, nil
	}
	code := a.run([]string{"update"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (already up to date)\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
	}
	if s := out.String(); !strings.Contains(s, "up to date") {
		t.Errorf("up-to-date message missing from stdout:\n%s", s)
	}
}

// TestUpdateSnapshotFlag proves `iris update --snapshot` selects the snapshot
// channel: the flag reaches the update engine as snapshot=true (plain `iris
// update` passes false), and the outcome renders the snapshot build it
// installed.
func TestUpdateSnapshotFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"update", "--snapshot"}, true},
		{[]string{"update"}, false},
	} {
		var out, errb bytes.Buffer
		a := newApp(&out, &errb)
		var got bool
		a.runUpdate = func(_ context.Context, current string, snapshot bool) (update.Result, error) {
			got = snapshot
			return update.Result{Status: update.StatusUpdated, From: current, To: "v1.2.4-snapshot.20260715.0a1b2c3d4e5f"}, nil
		}
		if code := a.run(tc.args); code != exitOK {
			t.Fatalf("%v: exit = %d, want %d\nstdout: %s\nstderr: %s", tc.args, code, exitOK, out.String(), errb.String())
		}
		if got != tc.want {
			t.Errorf("%v: engine saw snapshot=%v, want %v", tc.args, got, tc.want)
		}
		if tc.want && !strings.Contains(out.String(), "v1.2.4-snapshot.20260715.0a1b2c3d4e5f") {
			t.Errorf("outcome does not name the installed snapshot build:\n%s", out.String())
		}
	}
}
