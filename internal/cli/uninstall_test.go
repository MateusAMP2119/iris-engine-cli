package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// scratchExecutable writes a throwaway file standing in for the running iris binary, so removal never touches the test binary.
func scratchExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "iris")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write scratch executable: %v", err)
	}
	return path
}

// plantEngineState places one engine-owned artifact (the service unit) under the socket's engine dir so EngineArtifactsPresent reports state.
func plantEngineState(t *testing.T, sock string) string {
	t.Helper()
	unit := daemon.ServiceUnitPath(config.Settings{Socket: sock})
	if err := os.WriteFile(unit, []byte("unit\n"), 0o644); err != nil {
		t.Fatalf("write service unit: %v", err)
	}
	return unit
}

// containsQuote reports whether out carries one of the built-in farewell quotes with its attribution.
func containsQuote(out string) bool {
	for _, q := range farewellQuotes {
		if strings.Contains(out, q.text) && strings.Contains(out, q.author) {
			return true
		}
	}
	return false
}

// TestUninstallRootVerb proves `iris uninstall` is a top-level lifecycle verb: a
// runnable root leaf, classified daemonless, carrying the destructive --yes/--force
// gate and the inherited global flags, and never the --dry-run of declare.
func TestUninstallRootVerb(t *testing.T) {
	t.Run("uninstall-root-verb", func(t *testing.T) {
		root := testRoot()

		cmd := find(root, "uninstall")
		if cmd == nil {
			t.Fatal("root verb `uninstall` missing from the tree")
		}
		if !isLeafCommand(cmd) {
			t.Errorf("`uninstall` should be a runnable leaf, not a group node")
		}
		if life := cmd.Annotations[lifecycleAnnotation]; life != lifecycleDaemonless {
			t.Errorf("`uninstall` lifecycle = %q, want %q", life, lifecycleDaemonless)
		}
		for _, flag := range []string{"yes", "force"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("`uninstall` missing destructive gate flag --%s", flag)
			}
		}
		for _, flag := range []string{"json", "socket", "host", "token"} {
			if !acceptsFlag(cmd, flag) {
				t.Errorf("`uninstall` does not accept global flag --%s", flag)
			}
		}
		if cmd.Flags().Lookup("dry-run") != nil {
			t.Errorf("`uninstall` registers --dry-run, which belongs only to declare")
		}
	})
}

// TestUninstallConsentGate proves the per-step consent gate: no terminal and no
// --yes refuses with the standard consent-required error (exit 4) and removes
// nothing; a declined interactive prompt aborts the remainder cleanly (exit 0)
// and removes nothing.
func TestUninstallConsentGate(t *testing.T) {
	clearTargetEnv(t)

	t.Run("uninstall-consent-gate", func(t *testing.T) {
		deadSock := shortSocket(t) // nothing listening: no daemon reachable

		t.Run("non-interactive without --yes refuses", func(t *testing.T) {
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return "/opt/iris/bin/iris", nil }
			// A confirm seam that reports it cannot prompt (no terminal).
			a.confirm = func(_ string, _ bool) (bool, error) {
				return false, errors.New("not a terminal")
			}
			code := a.run([]string{"--socket", deadSock, "uninstall"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (consent required)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "--yes") {
				t.Errorf("consent-required message should name --yes: %s", errb.String())
			}
		})

		t.Run("declined interactive prompt aborts exit 0", func(t *testing.T) {
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return "/opt/iris/bin/iris", nil }
			a.confirm = func(_ string, _ bool) (bool, error) { return false, nil } // user typed N
			code := a.run([]string{"--socket", deadSock, "uninstall"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "Aborted. Nothing removed.") {
				t.Errorf("abort output should say 'Aborted. Nothing removed.': %s", out.String())
			}
		})
	})
}

// TestUninstallStopsEngine proves step 1: a recorded detached daemon is stopped
// (SIGTERM by pidfile, pidfile reaped); a reachable daemon with no pidfile fails
// the step (exit 4) naming it, while --force leaves it running and proceeds; and
// no running engine passes clean.
func TestUninstallStopsEngine(t *testing.T) {
	clearTargetEnv(t)

	t.Run("uninstall-stops-engine", func(t *testing.T) {
		t.Run("recorded detached daemon is stopped by pidfile", func(t *testing.T) {
			sock := shortSocket(t)
			settings := config.Settings{Socket: sock}
			child := exec.Command("sleep", "60")
			if err := child.Start(); err != nil {
				t.Fatalf("start stand-in daemon: %v", err)
			}
			go func() { _ = child.Wait() }() // reap the child once signalled, so the stop's liveness poll sees it gone
			t.Cleanup(func() { _ = child.Process.Kill() })
			if err := daemon.WritePIDFile(settings, child.Process.Pid); err != nil {
				t.Fatalf("write pidfile: %v", err)
			}

			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", sock, "uninstall", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "Iris engine stopped successfully.") {
				t.Errorf("step 1 should report the stop: %s", out.String())
			}
			if err := child.Process.Signal(syscall.Signal(0)); err == nil {
				t.Errorf("stand-in daemon should be gone after the stop")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(sock), "iris.pid")); !os.IsNotExist(err) {
				t.Errorf("pidfile should be reaped after the stop; stat err = %v", err)
			}
		})

		t.Run("reachable undetached daemon fails step 1", func(t *testing.T) {
			sock := shortSocket(t)
			srv := daemon.NewServer(config.Settings{Socket: sock}, api.NewMux())
			startInProcess(t, srv)

			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", sock, "uninstall", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (unstoppable daemon)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "step 1/3") || !strings.Contains(errb.String(), "--force") {
				t.Errorf("failure should name step 1/3 and offer --force: %s", errb.String())
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("scratch executable must be untouched when step 1 fails: %v", err)
			}
		})

		t.Run("--force leaves a reachable daemon running and proceeds", func(t *testing.T) {
			sock := shortSocket(t)
			srv := daemon.NewServer(config.Settings{Socket: sock}, api.NewMux())
			startInProcess(t, srv)

			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", sock, "uninstall", "--force"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (--force proceeds)\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "Daemon left running (--force).") {
				t.Errorf("step 1 should report the daemon was left running: %s", out.String())
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Errorf("scratch executable should be removed under --force; stat err = %v", err)
			}
		})

		t.Run("no running engine passes clean", func(t *testing.T) {
			deadSock := shortSocket(t)
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "No running engine; nothing to stop.") {
				t.Errorf("step 1 should report nothing to stop: %s", out.String())
			}
		})
	})
}

// TestUninstallEngineState proves step 2: on-disk engine state is removed behind
// its own y/N (or --yes), absent state skips the step without a prompt, and a
// decline aborts the remainder keeping the binary.
func TestUninstallEngineState(t *testing.T) {
	clearTargetEnv(t)

	t.Run("uninstall-engine-state", func(t *testing.T) {
		t.Run("--yes removes the engine state", func(t *testing.T) {
			sock := shortSocket(t)
			unit := plantEngineState(t, sock)
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", sock, "uninstall", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(unit); !os.IsNotExist(err) {
				t.Errorf("service unit should be removed; stat err = %v", err)
			}
			if !strings.Contains(out.String(), "Engine state removed.") {
				t.Errorf("step 2 should report the removal: %s", out.String())
			}
		})

		t.Run("absent engine state skips step 2 without a prompt", func(t *testing.T) {
			deadSock := shortSocket(t)
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			var asked []string
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(q string, _ bool) (bool, error) { asked = append(asked, q); return true, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "No engine state on disk; nothing to remove.") {
				t.Errorf("step 2 should report nothing to remove: %s", out.String())
			}
			if len(asked) != 1 || !strings.Contains(asked[0], "Uninstall cli") {
				t.Errorf("only step 3 should prompt when no engine state exists; asked = %q", asked)
			}
		})

		t.Run("declined engine state aborts keeping state and binary", func(t *testing.T) {
			sock := shortSocket(t)
			unit := plantEngineState(t, sock)
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(q string, _ bool) (bool, error) {
				return !strings.Contains(q, "Remove engine state"), nil // N to step 2
			}
			code := a.run([]string{"--socket", sock, "uninstall"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(unit); err != nil {
				t.Errorf("declined step 2 must keep the engine state: %v", err)
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("aborted sequence must keep the binary: %v", err)
			}
			if !strings.Contains(out.String(), "Aborted. Nothing removed.") {
				t.Errorf("abort should report nothing removed: %s", out.String())
			}
		})

		t.Run("declined step 3 reports the engine state already removed", func(t *testing.T) {
			sock := shortSocket(t)
			unit := plantEngineState(t, sock)
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(q string, _ bool) (bool, error) {
				return strings.Contains(q, "Remove engine state"), nil // y to step 2, N to step 3
			}
			code := a.run([]string{"--socket", sock, "uninstall"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (clean abort)\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(unit); !os.IsNotExist(err) {
				t.Errorf("step 2 removal should have run; stat err = %v", err)
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("declined step 3 must keep the binary: %v", err)
			}
			if !strings.Contains(out.String(), "Engine state removed; the iris binary stays at "+scratch) {
				t.Errorf("abort should report what was and was not removed: %s", out.String())
			}
		})
	})
}

// TestUninstallRemovesExecutable proves step 3 and the outcomes: with consent the
// resolved executable is deleted and the sequence closes with the version header,
// the step lines, and a farewell quote from the built-in pool (exit 0); a
// permission failure surfaces sudo/uninstaller guidance naming the step (exit 4);
// and --json carries per-step statuses plus the final outcome on one envelope.
func TestUninstallRemovesExecutable(t *testing.T) {
	clearTargetEnv(t)

	t.Run("uninstall-removes-executable", func(t *testing.T) {
		deadSock := shortSocket(t)

		t.Run("--yes removes and closes with a farewell quote", func(t *testing.T) {
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--yes"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Errorf("executable should be removed; stat err = %v", err)
			}
			for _, want := range []string{"[IRIS UNINSTALL v", "Step 1/3", "Step 2/3", "Step 3/3", "Binary removed", "Traces erased"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("staged output missing %q: %s", want, out.String())
				}
			}
			if !containsQuote(out.String()) {
				t.Errorf("success output should close with a quote from the built-in pool: %s", out.String())
			}
		})

		t.Run("permission failure guides to sudo naming the step", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("running as root: directory permissions do not block removal")
			}
			dir := t.TempDir()
			scratch := filepath.Join(dir, "iris")
			if err := os.WriteFile(scratch, []byte("x"), 0o755); err != nil {
				t.Fatalf("write scratch: %v", err)
			}
			// Remove write permission on the parent so os.Remove of the entry fails.
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatalf("chmod dir: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (permission denied)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "sudo") || !strings.Contains(errb.String(), "step 3/3") {
				t.Errorf("permission failure should suggest sudo and name step 3/3: %s", errb.String())
			}
		})

		t.Run("--json carries per-step statuses on one envelope", func(t *testing.T) {
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--yes", "--json"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			var env struct {
				Data uninstallCmdResult `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out.String())
			}
			if env.Data.Status != "uninstalled" || env.Data.Path != scratch {
				t.Errorf("json envelope = %+v, want status=uninstalled path=%s", env.Data, scratch)
			}
			wantSteps := map[string]string{stepStopEngine: "nothing_to_stop", stepEngineState: "nothing_to_remove", stepBinary: "removed"}
			if len(env.Data.Steps) != 3 {
				t.Fatalf("steps = %+v, want 3 entries", env.Data.Steps)
			}
			for _, s := range env.Data.Steps {
				if wantSteps[s.Name] != s.Status {
					t.Errorf("step %s status = %q, want %q", s.Name, s.Status, wantSteps[s.Name])
				}
			}
			if strings.Contains(out.String(), "█") || containsQuote(out.String()) {
				t.Errorf("--json output must carry no banner or quote: %s", out.String())
			}
		})

		t.Run("--json aborted marks declined and skipped steps", func(t *testing.T) {
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(_ string, _ bool) (bool, error) { return false, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--json"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			var env struct {
				Data uninstallCmdResult `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out.String())
			}
			if env.Data.Status != "aborted" {
				t.Errorf("json status = %q, want aborted", env.Data.Status)
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("aborted run must keep the binary: %v", err)
			}
		})
	})
}
