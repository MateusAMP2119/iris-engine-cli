package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file proves the engine-home target resolution (issue #169): the engine
// target derives from the fixed per-user engine home (IRIS_HOME, or ~/.iris),
// never from the invoking directory -- one engine per machine, findable from
// any cwd -- and `iris engine install`/`start` refuse with migration guidance
// when the invoking directory holds pre-engine-home state.

// TestResolveTargetIgnoresCwd proves target resolution is cwd-independent: with
// the engine home fixed, a decoy .iris tree under the invoking directory (the
// legacy location, complete with an iris.toml naming a host) contributes
// nothing -- the socket and config resolve from the engine home alone.
func TestResolveTargetIgnoresCwd(t *testing.T) {
	home := clearTargetEnv(t)
	cwd := t.TempDir()
	seedFile(t, filepath.Join(cwd, config.DirName, config.FileName), "host = \"decoy.example:1\"\n")
	t.Chdir(cwd)

	a := newApp(io.Discard, io.Discard)
	settings := a.resolveTarget(a.newRootCommand())

	if want := filepath.Join(home, config.SocketName); settings.Socket != want {
		t.Errorf("Socket = %q, want the engine-home socket %q (never cwd-derived)", settings.Socket, want)
	}
	if settings.Host != "" {
		t.Errorf("Host = %q, want empty: the cwd's legacy iris.toml must contribute nothing", settings.Host)
	}

	// The engine home's own iris.toml IS honored, from any cwd.
	seedFile(t, filepath.Join(home, config.FileName), "host = \"engine.example:8443\"\n")
	settings = a.resolveTarget(a.newRootCommand())
	if settings.Host != "engine.example:8443" {
		t.Errorf("Host = %q, want the engine home iris.toml's host", settings.Host)
	}
}

// TestEngineStartRefusesLegacyWorkspaceState proves the migration guard: `iris
// engine start` (and install) invoked in a directory holding pre-engine-home
// state under <cwd>/.iris fails fast (exit 4) naming the legacy path and the
// engine home to move it to, and never trips when that directory IS the engine
// home.
func TestEngineStartRefusesLegacyWorkspaceState(t *testing.T) {
	for _, argv := range [][]string{{"engine", "start"}, {"engine", "install"}} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			home := clearTargetEnv(t)
			cwd := t.TempDir()
			seedFile(t, filepath.Join(cwd, config.DirName, config.FileName), "pg_dsn = \"postgres://u@h/db\"\n")
			t.Chdir(cwd)

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run(argv)
			if code != exitOpFailed {
				t.Fatalf("exit = %d, want %d (legacy state must refuse)\nstdout: %s\nstderr: %s",
					code, exitOpFailed, out.String(), errb.String())
			}
			combined := out.String() + errb.String()
			if !strings.Contains(combined, filepath.Join(cwd, config.DirName)) || !strings.Contains(combined, home) {
				t.Errorf("refusal should name the legacy path and the engine home:\n%s", combined)
			}
			if !strings.Contains(combined, "older iris") {
				t.Errorf("refusal should say the state is from an older iris:\n%s", combined)
			}
		})
	}

	t.Run("the engine home itself never trips the guard", func(t *testing.T) {
		clearTargetEnv(t)
		cwd := t.TempDir()
		// IRIS_HOME pointing at <cwd>/.iris: the same directory the guard would
		// otherwise flag. Managed Postgres is not installed, so start must get
		// past the guard and fail with install guidance instead.
		t.Setenv(config.EnvHome, filepath.Join(cwd, config.DirName))
		seedFile(t, filepath.Join(cwd, config.DirName, config.FileName), "# engine home config\n")
		t.Chdir(cwd)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "start"})
		if code != exitOpFailed {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitOpFailed, errb.String())
		}
		combined := out.String() + errb.String()
		if strings.Contains(combined, "older iris") {
			t.Errorf("the engine home tripped the legacy guard:\n%s", combined)
		}
		if !strings.Contains(combined, "iris engine install") {
			t.Errorf("start should have failed with install guidance, got:\n%s", combined)
		}
	})

	t.Run("a bare unrelated .iris directory never trips the guard", func(t *testing.T) {
		clearTargetEnv(t)
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cwd, config.DirName), 0o755); err != nil {
			t.Fatalf("mkdir bare .iris: %v", err)
		}
		t.Chdir(cwd)

		var out, errb bytes.Buffer
		code := newApp(&out, &errb).run([]string{"engine", "start"})
		combined := out.String() + errb.String()
		if strings.Contains(combined, "older iris") {
			t.Errorf("a bare .iris directory tripped the legacy guard:\n%s", combined)
		}
		if code != exitOpFailed || !strings.Contains(combined, "iris engine install") {
			t.Errorf("exit = %d, want %d with install guidance:\n%s", code, exitOpFailed, combined)
		}
	})
}
