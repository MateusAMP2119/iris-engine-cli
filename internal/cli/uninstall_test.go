package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// scratchExecutable writes a throwaway file standing in for the running iris
// binary and returns its path, so `iris uninstall` can remove a real file with no
// risk to the test binary.
func scratchExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "iris")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write scratch executable: %v", err)
	}
	return path
}

// TestUninstallRootVerb proves `iris uninstall` is a top-level lifecycle verb: a
// runnable root leaf, classified daemonless, carrying the destructive --yes/--force
// gate and the inherited global flags, and never the --dry-run of declare.
//
// spec: S08/uninstall-root-verb
func TestUninstallRootVerb(t *testing.T) {
	// spec: S08/uninstall-root-verb
	t.Run("S08/uninstall-root-verb", func(t *testing.T) {
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

// TestUninstallConsentGate proves the destructive consent gate: without --yes and
// with no terminal to prompt, the command refuses with the standard
// consent-required error (exit 4) and removes nothing; a declined interactive
// prompt aborts cleanly (exit 0) with the "Aborted. Nothing removed." line and
// removes nothing.
//
// spec: S08/uninstall-consent-gate
func TestUninstallConsentGate(t *testing.T) {
	clearTargetEnv(t)

	t.Run("S08/uninstall-consent-gate", func(t *testing.T) {
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
				t.Errorf("abort output should be 'Aborted. Nothing removed.': %s", out.String())
			}
		})
	})
}

// TestUninstallDaemonRunningRefused proves the reachable-daemon refusal: with a
// live daemon on the resolved socket, `iris uninstall` refuses (exit 4) with
// guidance to stop and uninstall the engine first, and removes nothing; --force
// overrides the probe and removes the binary.
//
// spec: S08/uninstall-daemon-running-refused
func TestUninstallDaemonRunningRefused(t *testing.T) {
	clearTargetEnv(t)

	t.Run("S08/uninstall-daemon-running-refused", func(t *testing.T) {
		t.Run("reachable daemon refuses without --force", func(t *testing.T) {
			sock := shortSocket(t)
			srv := daemon.NewServer(config.Settings{Socket: sock}, api.NewMux())
			startInProcess(t, srv)

			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(_ string, _ bool) (bool, error) { return true, nil } // consent would pass; probe must win
			code := a.run([]string{"--socket", sock, "uninstall", "--yes"})
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (daemon reachable)\nstderr: %s", code, exitOpFailed, errb.String())
			}
			if !strings.Contains(errb.String(), "engine stop") || !strings.Contains(errb.String(), "engine uninstall") {
				t.Errorf("refusal should guide to `iris engine stop && iris engine uninstall`: %s", errb.String())
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("scratch executable must be untouched on refusal: %v", err)
			}
		})

		t.Run("--force overrides a reachable daemon and removes", func(t *testing.T) {
			sock := shortSocket(t)
			srv := daemon.NewServer(config.Settings{Socket: sock}, api.NewMux())
			startInProcess(t, srv)

			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", sock, "uninstall", "--force"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (--force removes)\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(scratch); !os.IsNotExist(err) {
				t.Errorf("scratch executable should be removed under --force; stat err = %v", err)
			}
		})
	})
}

// TestUninstallRemovesExecutable proves the removal itself: with consent (--yes)
// and no reachable daemon, the resolved executable is deleted and the goodbye
// lines are printed (exit 0); a declined prompt leaves the file untouched; a
// permission failure surfaces sudo/uninstaller guidance (exit 4); and --json
// carries the outcome on one data envelope.
//
// spec: S08/uninstall-removes-executable
func TestUninstallRemovesExecutable(t *testing.T) {
	clearTargetEnv(t)

	t.Run("S08/uninstall-removes-executable", func(t *testing.T) {
		deadSock := shortSocket(t)

		t.Run("--yes removes and says goodbye", func(t *testing.T) {
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
			if !strings.Contains(out.String(), "Uninstalled "+scratch+".") {
				t.Errorf("success output should name the removed path: %s", out.String())
			}
			if !strings.Contains(out.String(), "Goodbye from iris.") {
				t.Errorf("success output should print the goodbye line: %s", out.String())
			}
		})

		t.Run("declined prompt leaves the file untouched", func(t *testing.T) {
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			a.confirm = func(_ string, _ bool) (bool, error) { return false, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Errorf("declined uninstall must leave the file: %v", err)
			}
		})

		t.Run("permission failure guides to sudo", func(t *testing.T) {
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
			if !strings.Contains(errb.String(), "sudo") {
				t.Errorf("permission failure should suggest sudo: %s", errb.String())
			}
		})

		t.Run("--json carries the outcome on one envelope", func(t *testing.T) {
			scratch := scratchExecutable(t)
			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.executablePath = func() (string, error) { return scratch, nil }
			code := a.run([]string{"--socket", deadSock, "uninstall", "--yes", "--json"})
			if code != exitOK {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			var env struct {
				Data struct {
					Status string `json:"status"`
					Path   string `json:"path"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out.String())
			}
			if env.Data.Status != "uninstalled" || env.Data.Path != scratch {
				t.Errorf("json envelope = %+v, want status=uninstalled path=%s", env.Data, scratch)
			}
		})
	})
}
