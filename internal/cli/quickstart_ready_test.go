package cli

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
)

// infoFunc adapts a function to the api.InfoHandler interface for the
// integration-tier daemon fakes (startInfoDaemon lives beside the engine-info
// readout tests).
type infoFunc func(ctx context.Context) (api.InfoPayload, error)

func (f infoFunc) Info(ctx context.Context) (api.InfoPayload, error) { return f(ctx) }

// TestQuickstartEngineActWaitsForRole proves the ENGINE act closes only when
// the daemon reports a leadership role: the tour polls the /info readout after
// the act's steps, holds while the role is unknown, proceeds when it flips,
// and a daemon that never reports a role is a clear fault (exit 4) that keeps
// THE PIPELINE act shut.
func TestQuickstartEngineActWaitsForRole(t *testing.T) {
	clearTargetEnv(t)
	unsetNoColor(t)
	// spec: S08/quickstart-act-structure
	t.Run("S08/quickstart-act-structure", func(t *testing.T) {
		t.Run("the act holds through unknown AND standby, proceeding only on leader", func(t *testing.T) {
			chdirWorkspace(t)
			sock := shortSocket(t)
			var infoCalls atomic.Int64
			startInfoDaemon(t, sock, infoFunc(func(context.Context) (api.InfoPayload, error) {
				// The real election shape: unknown, then a contending standby
				// (`engine start -d` returns on socket-up while the fresh workspace
				// engine is still winning its own election), then leader.
				n := infoCalls.Add(1)
				role := "unknown"
				switch {
				case n >= 4:
					role = "leader"
				case n >= 2:
					role = "standby"
				}
				return api.InfoPayload{Role: role, Socket: sock, Uptime: "1s"}, nil
			}))

			var out, errb bytes.Buffer
			a := tourApp(&out, &errb, true)
			events := scriptTour(a, proceeds(1), nil)
			a.readyEvery = 10 * time.Millisecond
			a.readyBudget = 2 * time.Second
			a.waitForReady = a.waitEngineReady // the real poll, against the fake /info

			code := a.run([]string{"quickstart", "--socket=" + sock})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, out.String(), errb.String())
			}
			if n := infoCalls.Load(); n < 4 {
				t.Errorf("/info polled %d times; the act closed before leadership (a standby readout must hold it)", n)
			}
			// The tour proceeded: THE PIPELINE's gate was asked and its steps ran.
			if prompts := promptEvents(*events); len(prompts) != 1 {
				t.Errorf("act gates = %q, want exactly the PIPELINE gate", prompts)
			}
			if steps := stepEvents(*events); len(steps) == 0 || !strings.HasPrefix(steps[len(steps)-1], "data provenance") {
				t.Errorf("PIPELINE steps did not run to the finale: %q", steps)
			}
		})

		t.Run("a daemon that never reports leadership is a clear fault, exit 4, PIPELINE shut", func(t *testing.T) {
			chdirWorkspace(t)
			sock := shortSocket(t)
			startInfoDaemon(t, sock, infoFunc(func(context.Context) (api.InfoPayload, error) {
				// A standby forever: reachable, healthy, never the leader -- a
				// mutation would exit 6, so the act must not close on it.
				return api.InfoPayload{Role: "standby", Socket: sock, Uptime: "1s"}, nil
			}))

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
			if prompts := promptEvents(*events); len(prompts) != 0 {
				t.Errorf("an unready ENGINE act still offered the next act's gate: %q", prompts)
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
