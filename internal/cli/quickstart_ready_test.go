package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// scriptedRole adapts a function to the api.RoleReporter interface, so the
// integration-tier daemon fakes can serve a scripted leadership role on
// GET /healthz (the probe waitEngineReady polls).
type scriptedRole struct{ fn func() api.Role }

func (s scriptedRole) Role() api.Role     { return s.fn() }
func (s scriptedRole) LeaderHint() string { return "" }

// startRoleDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the scripted role reporter, so the tour's readiness poll
// reads the same /healthz document a live daemon serves.
func startRoleDaemon(t *testing.T, sock string, role api.RoleReporter) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(api.WithRole(role)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestQuickstartEngineActWaitsForRole proves the ENGINE act closes only when
// the daemon reports a leadership role: the tour polls the /healthz probe after
// the act's steps, holds while the role is unknown, proceeds when it flips,
// and a daemon that never reports a role is a clear fault (exit 4) that keeps
// THE PIPELINE act shut.
func TestQuickstartEngineActWaitsForRole(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	t.Run("quickstart-act-structure", func(t *testing.T) {
		t.Run("the act holds through unknown AND standby, proceeding only on leader", func(t *testing.T) {
			chdirWorkspace(t)
			sock := shortSocket(t)
			var roleCalls atomic.Int64
			startRoleDaemon(t, sock, scriptedRole{fn: func() api.Role {
				// The real election shape: unknown, then a contending standby
				// (`engine start -d` returns on socket-up while the fresh workspace
				// engine is still winning its own election), then leader.
				n := roleCalls.Add(1)
				switch {
				case n >= 4:
					return api.RoleLeader
				case n >= 2:
					return api.RoleStandby
				default:
					return api.RoleUnknown
				}
			}})

			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)
			a.readyEvery = 10 * time.Millisecond
			a.readyBudget = 2 * time.Second
			a.waitForReady = a.waitEngineReady // the real poll, against the fake /healthz

			code := a.run([]string{"quickstart", "--socket=" + sock})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if n := roleCalls.Load(); n < 4 {
				t.Errorf("/healthz polled %d times; the act closed before leadership (a standby readout must hold it)", n)
			}
			// The tour proceeded past the readiness hold: the shop pick was asked.
			if picks := pickEvents(*events); len(picks) != 1 {
				t.Errorf("act picks = %q, want exactly the shop pick", picks)
			}
			if steps := stepEvents(*events); len(steps) == 0 || !strings.HasPrefix(steps[len(steps)-1], "ps") {
				t.Errorf("engine steps did not run to the readout: %q", steps)
			}
		})

		t.Run("a daemon that never reports leadership is a clear fault, exit 4, PIPELINE shut", func(t *testing.T) {
			chdirWorkspace(t)
			sock := shortSocket(t)
			startRoleDaemon(t, sock, scriptedRole{fn: func() api.Role {
				// A standby forever: reachable, healthy, never the leader -- a
				// mutation would exit 6, so the act must not close on it.
				return api.RoleStandby
			}})

			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)
			a.readyEvery = 10 * time.Millisecond
			a.readyBudget = 150 * time.Millisecond
			a.waitForReady = a.waitEngineReady

			code := a.run([]string{"quickstart", "--socket=" + sock})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (a clear fault)\nstdout: %s\nstderr: %s", code, exitOpFailed, out.String(), errb.String())
			}
			if picks := pickEvents(*events); len(picks) != 0 {
				t.Errorf("an unready ENGINE act still offered the next act's pick: %q", picks)
			}
			for _, step := range stepEvents(*events) {
				if strings.HasPrefix(step, "declare apply") {
					t.Errorf("PIPELINE steps ran on an unready engine: %q", stepEvents(*events))
				}
			}
			if e := errb.String(); !strings.Contains(e, "role") {
				t.Errorf("fault does not explain the missing leadership role: %q", e)
			}
		})
	})
}
