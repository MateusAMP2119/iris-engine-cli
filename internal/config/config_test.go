package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// sp and ip return pointers to a string and an int64, the way a configuration
// Layer marks a field as explicitly set. A nil field is unset (it defers to the
// next lower layer); a pointer to the zero value is an explicit zero that still
// overrides.
func sp(s string) *string { return &s }
func ip(i int64) *int64   { return &i }

// TestConfigPrecedenceOrder proves the strict, per-field precedence of
// specification section 8: command flags override IRIS_* env, which override
// iris.toml, which override built-in defaults. It walks every layer relationship
// for a string field (Socket) and an integer field (Retain), proves an unset
// higher layer preserves the lower one, and proves the set-vs-zero distinction:
// a layer that explicitly sets a field to its zero value still overrides the
// layer below, while a layer that leaves the field unset does not.
//
// spec: S08/config-precedence-order
func TestConfigPrecedenceOrder(t *testing.T) {
	// Layers are passed to Resolve lowest-precedence first: defaults, file, env,
	// flags.
	t.Run("socket resolves highest set layer", func(t *testing.T) {
		cases := []struct {
			name                  string
			def, file, env, flags *string
			want                  string
		}{
			{"defaults only", sp("/def"), nil, nil, nil, "/def"},
			{"file over defaults", sp("/def"), sp("/file"), nil, nil, "/file"},
			{"env over file", sp("/def"), sp("/file"), sp("/env"), nil, "/env"},
			{"flags over env", sp("/def"), sp("/file"), sp("/env"), sp("/flag"), "/flag"},
			{"flags over defaults, middle unset", sp("/def"), nil, nil, sp("/flag"), "/flag"},
			{"env over defaults, file unset", sp("/def"), nil, sp("/env"), nil, "/env"},
			{"unset higher layers preserve lower", sp("/def"), nil, nil, nil, "/def"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := config.Resolve(
					config.Layer{Socket: tc.def},
					config.Layer{Socket: tc.file},
					config.Layer{Socket: tc.env},
					config.Layer{Socket: tc.flags},
				)
				if got.Socket != tc.want {
					t.Errorf("Socket = %q, want %q", got.Socket, tc.want)
				}
			})
		}
	})

	t.Run("retain (int) resolves highest set layer", func(t *testing.T) {
		cases := []struct {
			name                  string
			def, file, env, flags *int64
			want                  int64
		}{
			{"defaults only", ip(1000), nil, nil, nil, 1000},
			{"file over defaults", ip(1000), ip(50), nil, nil, 50},
			{"env over file", ip(1000), ip(50), ip(200), nil, 200},
			{"flags over env", ip(1000), ip(50), ip(200), ip(7), 7},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := config.Resolve(
					config.Layer{Retain: tc.def},
					config.Layer{Retain: tc.file},
					config.Layer{Retain: tc.env},
					config.Layer{Retain: tc.flags},
				)
				if got.Retain != tc.want {
					t.Errorf("Retain = %d, want %d", got.Retain, tc.want)
				}
			})
		}
	})

	t.Run("explicit zero overrides, unset does not", func(t *testing.T) {
		// A higher layer that explicitly sets Socket to "" overrides a lower
		// non-empty value; strict precedence is by presence, not by truthiness.
		got := config.Resolve(
			config.Layer{Socket: sp("/def")},
			config.Layer{Socket: sp("/file")},
			config.Layer{Socket: sp("")}, // env explicitly empty
			config.Layer{},               // flags unset
		)
		if got.Socket != "" {
			t.Errorf("explicit-empty env Socket = %q, want empty (explicit zero overrides)", got.Socket)
		}

		// An explicit zero for an int field likewise overrides the layer below.
		gotRetain := config.Resolve(
			config.Layer{Retain: ip(1000)},
			config.Layer{Retain: ip(50)},
			config.Layer{},
			config.Layer{Retain: ip(0)}, // flags explicitly zero
		)
		if gotRetain.Retain != 0 {
			t.Errorf("explicit-zero flags Retain = %d, want 0 (explicit zero overrides)", gotRetain.Retain)
		}
	})

	t.Run("every field is independently layered", func(t *testing.T) {
		// A single higher layer overriding one field must not disturb the others,
		// which fall through to their own highest set layer.
		defaults := config.Defaults("")
		flags := config.Layer{Socket: sp("/flag.sock")}
		got := config.Resolve(defaults, config.Layer{}, config.Layer{}, flags)
		if got.Socket != "/flag.sock" {
			t.Errorf("Socket = %q, want the flag value", got.Socket)
		}
		if got.Retain != config.DefaultRetain {
			t.Errorf("Retain = %d, want the default %d (untouched by the socket flag)", got.Retain, config.DefaultRetain)
		}
		if got.ObjectsPath != filepath.Join(".iris", "objects") {
			t.Errorf("ObjectsPath = %q, want the default (untouched by the socket flag)", got.ObjectsPath)
		}
	})
}

// TestDocumentedEnvVarsRecognized proves every documented IRIS_* environment
// variable of specification section 8 is recognized and supplies the value for
// its corresponding setting: IRIS_SOCKET, IRIS_HOST, IRIS_TOKEN, IRIS_PG_DSN,
// IRIS_RETAIN, IRIS_JOURNAL_PARTITION_ROWS, IRIS_OBJECTS_PATH. It also proves an
// unset (empty) variable contributes nothing (the setting falls back to its
// default), and that the two integer variables parse.
//
// spec: S08/documented-env-vars-recognized
func TestDocumentedEnvVarsRecognized(t *testing.T) {
	// Each documented variable, in isolation, maps to exactly its setting.
	stringVars := []struct {
		env string
		get func(config.Settings) string
	}{
		{config.EnvSocket, func(s config.Settings) string { return s.Socket }},
		{config.EnvHost, func(s config.Settings) string { return s.Host }},
		{config.EnvToken, func(s config.Settings) string { return s.Token }},
		{config.EnvPgDSN, func(s config.Settings) string { return s.PgDSN }},
		{config.EnvObjectsPath, func(s config.Settings) string { return s.ObjectsPath }},
	}
	for _, tc := range stringVars {
		t.Run(tc.env, func(t *testing.T) {
			const val = "from-env-value"
			env := map[string]string{tc.env: val}
			layer, err := config.FromEnv(func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			got := config.Resolve(config.Defaults(""), config.Layer{}, layer, config.Layer{})
			if tc.get(got) != val {
				t.Errorf("%s did not supply its setting: got %q, want %q", tc.env, tc.get(got), val)
			}
		})
	}

	intVars := []struct {
		env string
		get func(config.Settings) int64
	}{
		{config.EnvRetain, func(s config.Settings) int64 { return s.Retain }},
		{config.EnvJournalPartitionRows, func(s config.Settings) int64 { return s.JournalPartitionRows }},
	}
	for _, tc := range intVars {
		t.Run(tc.env, func(t *testing.T) {
			env := map[string]string{tc.env: "4242"}
			layer, err := config.FromEnv(func(k string) string { return env[k] })
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			got := config.Resolve(config.Defaults(""), config.Layer{}, layer, config.Layer{})
			if tc.get(got) != 4242 {
				t.Errorf("%s did not supply its integer setting: got %d, want 4242", tc.env, tc.get(got))
			}
		})
	}

	t.Run("unset variable contributes nothing", func(t *testing.T) {
		layer, err := config.FromEnv(func(string) string { return "" })
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		got := config.Resolve(config.Defaults("/ws"), config.Layer{}, layer, config.Layer{})
		if got.Socket != filepath.Join("/ws", ".iris", "iris.sock") {
			t.Errorf("empty env Socket = %q, want the default socket", got.Socket)
		}
		if got.Retain != config.DefaultRetain {
			t.Errorf("empty env Retain = %d, want the default %d", got.Retain, config.DefaultRetain)
		}
	})

	t.Run("a non-numeric integer variable is a config error", func(t *testing.T) {
		env := map[string]string{config.EnvRetain: "not-a-number"}
		if _, err := config.FromEnv(func(k string) string { return env[k] }); err == nil {
			t.Fatal("FromEnv(IRIS_RETAIN=not-a-number) = nil error, want a parse error")
		}
	})

	t.Run("recognized through the real process environment", func(t *testing.T) {
		// The same recognition holds against os.Getenv, proving the wiring the CLI
		// uses, not just an injected lookup.
		t.Setenv(config.EnvSocket, "/tmp/real.sock")
		t.Setenv(config.EnvRetain, "9")
		layer, err := config.FromEnv(os.Getenv)
		if err != nil {
			t.Fatalf("FromEnv(os.Getenv): %v", err)
		}
		got := config.Resolve(config.Defaults(""), config.Layer{}, layer, config.Layer{})
		if got.Socket != "/tmp/real.sock" {
			t.Errorf("Socket = %q, want /tmp/real.sock from IRIS_SOCKET", got.Socket)
		}
		if got.Retain != 9 {
			t.Errorf("Retain = %d, want 9 from IRIS_RETAIN", got.Retain)
		}
	})
}

// TestZeroConfigDefaults proves the zero-config path of specification section 8:
// with no flags, no environment, and no iris.toml, the CLI defaults to the local
// socket under the workspace .iris directory and the engine defaults to managed
// Postgres (no admin DSN configured -> managed mode). It resolves both the bare
// defaults layer and the full four-layer stack with every other source empty,
// which must agree.
//
// spec: S08/zero-config-defaults
func TestZeroConfigDefaults(t *testing.T) {
	const ws = "/home/dev/project"

	// The full stack with nothing but defaults set.
	got := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{}, config.Layer{})

	wantSocket := filepath.Join(ws, ".iris", "iris.sock")
	if got.Socket != wantSocket {
		t.Errorf("zero-config Socket = %q, want the local socket %q", got.Socket, wantSocket)
	}
	if !got.Managed() {
		t.Errorf("zero-config Managed() = false, want true (managed Postgres when no admin DSN is set); PgDSN = %q", got.PgDSN)
	}
	if got.PgDSN != "" {
		t.Errorf("zero-config PgDSN = %q, want empty (managed mode)", got.PgDSN)
	}
	if want := filepath.Join(ws, ".iris", "objects"); got.ObjectsPath != want {
		t.Errorf("zero-config ObjectsPath = %q, want %q", got.ObjectsPath, want)
	}
	if got.Retain != config.DefaultRetain {
		t.Errorf("zero-config Retain = %d, want %d", got.Retain, config.DefaultRetain)
	}
	if got.JournalPartitionRows != config.DefaultJournalPartitionRows {
		t.Errorf("zero-config JournalPartitionRows = %d, want %d", got.JournalPartitionRows, config.DefaultJournalPartitionRows)
	}

	// The full resolved struct is exactly the documented zero-config defaults.
	// Distinct operands -- an independently built expected value versus the fold --
	// so this can actually fail, and it pins the fields the per-field checks above
	// omit (Host, Token, and the TCP/TLS settings) to empty.
	want := config.Settings{
		Socket:               wantSocket,
		Retain:               config.DefaultRetain,
		JournalPartitionRows: config.DefaultJournalPartitionRows,
		ObjectsPath:          filepath.Join(ws, ".iris", "objects"),
	}
	if got != want {
		t.Errorf("zero-config resolution = %+v, want the documented defaults %+v", got, want)
	}

	// External Postgres is selected the moment an admin DSN appears, so managed is
	// exactly the no-DSN case.
	external := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{PgDSN: sp("postgres://u@h/db")}, config.Layer{})
	if external.Managed() {
		t.Error("Managed() = true with an admin DSN set, want false (external mode)")
	}
}
