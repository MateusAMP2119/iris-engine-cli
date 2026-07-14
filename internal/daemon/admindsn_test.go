package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// ptr returns a pointer to s, for building config layers.
func ptr(s string) *string { return &s }

// settingsWith folds the given per-source pg_dsn layers through the real config
// precedence (defaults < iris.toml < IRIS_* env < flags) into resolved Settings,
// so the admin-DSN resolution is exercised over the documented chain rather than a
// hand-set field. An empty string for a layer means that source did not set it.
func settingsWith(tomlDSN, envDSN, flagDSN string) config.Settings {
	toml, env, flags := config.Layer{}, config.Layer{}, config.Layer{}
	if tomlDSN != "" {
		toml.PgDSN = ptr(tomlDSN)
	}
	if envDSN != "" {
		env.PgDSN = ptr(envDSN)
	}
	if flagDSN != "" {
		flags.PgDSN = ptr(flagDSN)
	}
	return config.Resolve(config.Defaults(""), toml, env, flags)
}

// TestAdminDSNPrecedence proves the admin DSN resolves at startup as --pg-dsn over
// IRIS_PG_DSN over iris.toml pg_dsn, and that with none present resolution fails
// fast with no default.
func TestAdminDSNPrecedence(t *testing.T) {
	cases := []struct {
		name             string
		toml, env, flags string
		want             string
	}{
		{"flag over env over toml", "postgres://toml/db", "postgres://env/db", "postgres://flag/db", "postgres://flag/db"},
		{"env over toml", "postgres://toml/db", "postgres://env/db", "", "postgres://env/db"},
		{"toml only", "postgres://toml/db", "", "", "postgres://toml/db"},
		{"flag over toml (no env)", "postgres://toml/db", "", "postgres://flag/db", "postgres://flag/db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin, err := daemon.Resolve(settingsWith(tc.toml, tc.env, tc.flags))
			if err != nil {
				t.Fatalf("Resolve(%+v) = error %v, want nil", tc, err)
			}
			if got := admin.Source().ConnString(); got != tc.want {
				t.Errorf("resolved admin DSN = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("none present fails fast with no default", func(t *testing.T) {
		admin, err := daemon.Resolve(settingsWith("", "", ""))
		if !errors.Is(err, daemon.ErrNoAdminDSN) {
			t.Fatalf("Resolve(none) error = %v, want ErrNoAdminDSN", err)
		}
		// Fail fast means no default value is substituted: the returned AdminDSN is
		// inert, not some fallback string.
		if got := admin.Source().ConnString(); got != "" {
			t.Errorf("failed Resolve yielded a non-empty admin DSN %q, want no default", got)
		}
	})
}

// TestAdminDSNMemoryOnly proves the admin DSN is held only in daemon memory: it
// redacts under every fmt verb (so it never leaks into a log line), it cannot be
// serialized into meta or a CLI response (no exported field, not a Marshaler), and
// resolving it — the same chain a daemonless lifecycle command runs — writes it to
// no local file (never in meta, CLI never sees it, daemonless commands resolve the
// same chain and never store it).
func TestAdminDSNMemoryOnly(t *testing.T) {
	// A password marker embedded in the DSN. If any leak vector surfaces the raw
	// string, this marker appears in the output.
	const marker = "unguessablemarker9x7"
	const fixtureDSN = "postgres://admin:" + marker + "@db.internal:5432/meta?sslmode=require"

	admin, err := daemon.Resolve(config.Settings{PgDSN: fixtureDSN})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	leaks := func(s string) bool { return strings.Contains(s, marker) || strings.Contains(s, fixtureDSN) }

	t.Run("redacts under fmt", func(t *testing.T) {
		rendered := []string{admin.String(), admin.GoString(), admin.Source().String(), admin.Source().GoString()}
		// A variable verb exercises each formatting path at runtime (a literal "%s"
		// would just be String(), already covered).
		for _, verb := range []string{"%v", "%s", "%#v", "%q"} {
			rendered = append(rendered, fmt.Sprintf(verb, admin), fmt.Sprintf(verb, admin.Source()))
		}
		for _, s := range rendered {
			if leaks(s) {
				t.Errorf("admin DSN formatting leaked the secret: %q", s)
			}
		}
		// A positive check that the redaction sentinel is what a caller sees.
		if got := admin.String(); !strings.Contains(strings.ToUpper(got), "REDACTED") {
			t.Errorf("admin DSN String() = %q, want a REDACTED sentinel", got)
		}
	})

	t.Run("never serialized into meta or a CLI response", func(t *testing.T) {
		// A meta row or a --json CLI response would carry the admin DSN as a field;
		// encoding that response must never surface the raw string, because the
		// DSN's only field is unexported (no reflection-based encoder reaches it).
		type response struct {
			Admin  daemon.AdminDSN         `json:"admin"`
			Source daemon.ConnectionSource `json:"source"`
		}
		b, err := json.Marshal(response{Admin: admin, Source: admin.Source()})
		if err != nil {
			t.Fatalf("json.Marshal(response): %v", err)
		}
		if leaks(string(b)) {
			t.Errorf("admin DSN survived JSON encoding: %s", b)
		}
		// It is not a Marshaler that could re-expose the raw string through a custom
		// encoding path.
		if _, ok := any(admin).(json.Marshaler); ok {
			t.Error("AdminDSN implements json.Marshaler; it must not expose a custom encoding of the DSN")
		}
	})

	t.Run("resolving the chain writes no local state", func(t *testing.T) {
		// The daemonless lifecycle commands resolve the same chain and must never
		// store the DSN. Resolve in an empty temp cwd and assert nothing was written
		// (the no-local-state rule: the admin DSN is held in memory only).
		tmp := t.TempDir()
		t.Chdir(tmp)

		if _, err := daemon.Resolve(config.Settings{PgDSN: fixtureDSN}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		var created []string
		if err := filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				created = append(created, path)
			}
			return nil
		}); err != nil {
			t.Fatalf("walk temp tree: %v", err)
		}
		if len(created) != 0 {
			t.Errorf("resolving the admin DSN wrote local files, want none (memory-only): %v", created)
		}
	})
}

// TestAdminDSNRedactsEveryVerb proves the memory-only guarantee holds under every
// fmt verb, not just the string-like ones: String/GoString cover only %v/%s/%q and
// %#v, so a numeric verb (%d, %o, %b) would otherwise fall through to struct
// reflection and print the unexported connection string verbatim. AdminDSN and
// ConnectionSource each implement fmt.Formatter, which preempts every formatting
// path, so no verb can leak the DSN.
func TestAdminDSNRedactsEveryVerb(t *testing.T) {
	const marker = "unguessablemarker9x7"
	const fixtureDSN = "postgres://admin:" + marker + "@db.internal:5432/meta?sslmode=require"

	admin, err := daemon.Resolve(config.Settings{PgDSN: fixtureDSN})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	values := map[string]any{"AdminDSN": admin, "ConnectionSource": admin.Source()}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%o", "%b"}
	for typeName, val := range values {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, val)
			if strings.Contains(out, marker) || strings.Contains(out, fixtureDSN) {
				t.Errorf("%s formatted with %s leaked the DSN: %q", typeName, verb, out)
			}
			// Every verb still renders the redaction sentinel, never a fmt error
			// verb (%!) or a raw struct dump.
			if !strings.Contains(strings.ToUpper(out), "REDACTED") {
				t.Errorf("%s formatted with %s = %q, want the REDACTED sentinel", typeName, verb, out)
			}
		}
	}
}

// recordingDialer records the connection string of every Dial it receives without
// opening a live connection. Its Dial signature satisfies both store.Dialer and
// pg.Dialer, so one recorder can stand in for either seam.
type recordingDialer struct{ dialed []string }

func (r *recordingDialer) Dial(_ context.Context, connString string) error {
	r.dialed = append(r.dialed, connString)
	return nil
}

// TestConnectionsDeriveAdminDSN proves every Postgres connection the engine opens
// derives from the single daemon-owned admin DSN: the daemon dials meta (store) and
// data (pg) only from the admin DSN's derived source, and the source type the
// store/pg entry points require can carry a meaningful DSN only when built from the
// admin DSN (a forged zero-value source is inert).
func TestConnectionsDeriveAdminDSN(t *testing.T) {
	ctx := context.Background()
	const adminDSN = "postgres://admin@cluster:5432/postgres"

	admin, err := daemon.Resolve(config.Settings{PgDSN: adminDSN})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	t.Run("engine dials meta and data from the admin DSN", func(t *testing.T) {
		meta, data := &recordingDialer{}, &recordingDialer{}
		if err := admin.Connect(ctx, meta, data); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		for name, rec := range map[string]*recordingDialer{"meta": meta, "data": data} {
			if len(rec.dialed) != 1 {
				t.Fatalf("%s opened %d connections, want 1", name, len(rec.dialed))
			}
			if rec.dialed[0] != adminDSN {
				t.Errorf("%s connection derived from %q, want the admin DSN %q", name, rec.dialed[0], adminDSN)
			}
		}
	})

	t.Run("store and pg entry points dial only the source they are given", func(t *testing.T) {
		src := admin.Source()
		meta, data := &recordingDialer{}, &recordingDialer{}
		if err := store.Open(ctx, src, meta); err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		if err := pg.Open(ctx, src, data); err != nil {
			t.Fatalf("pg.Open: %v", err)
		}
		if len(meta.dialed) != 1 || meta.dialed[0] != adminDSN {
			t.Errorf("store.Open dialed %v, want [%q]", meta.dialed, adminDSN)
		}
		if len(data.dialed) != 1 || data.dialed[0] != adminDSN {
			t.Errorf("pg.Open dialed %v, want [%q]", data.dialed, adminDSN)
		}
	})

	t.Run("a source not built from the admin DSN is inert", func(t *testing.T) {
		// The only ConnectionSource reachable outside the daemon's derivation is the
		// zero value, and it carries no DSN: connections cannot originate from a
		// forged source.
		var forged daemon.ConnectionSource
		if got := forged.ConnString(); got != "" {
			t.Errorf("zero-value ConnectionSource yielded %q, want empty (only the admin DSN derivation carries a string)", got)
		}
	})
}
