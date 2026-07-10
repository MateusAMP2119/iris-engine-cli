package daemon

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// dbURLFromEnv returns the IRIS_DB_URL value the child env carries, or "" if absent.
func dbURLFromEnv(env []string) (string, bool) {
	prefix := dispatch.DBConnEnvVar + "="
	for i := len(env) - 1; i >= 0; i-- { // last write wins, as the child sees it
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}

// TestManualRunScopedConnectionInjected proves the manual-run plane injects the
// run-scoped data-database connection into the subprocess environment exactly as the
// lane path does: the run id rides the base scoped DSN as the iris.run_id GUC
// (pg.InjectRunID), so a manually-run pipeline's captured writes attribute to its own
// run. The injection is a function of the base DSN plus the run id, identical whether
// the run's cause is loop or manual (specification section 7).
//
// spec: S07/pipeline-scoped-connection-injected
func TestManualRunScopedConnectionInjected(t *testing.T) {
	t.Run("S07/pipeline-scoped-connection-injected", func(t *testing.T) {
		const base = "postgres://writer:pw@localhost:5432/data?sslmode=disable"

		t.Run("the run id rides the base scoped DSN", func(t *testing.T) {
			m := &manualExec{dbURL: base}
			got, ok := dbURLFromEnv(m.childEnv(42))
			if !ok {
				t.Fatal("manual child env carries no IRIS_DB_URL")
			}
			want := pg.InjectRunID(base, 42)
			if got != want {
				t.Errorf("injected DSN = %q, want the base DSN carrying run 42 %q", got, want)
			}
			// The injected connection carries the run id as the iris.run_id GUC the
			// capture trigger reads: the same mechanism the run-attribution conformance
			// leg proves end-to-end.
			if !strings.Contains(got, "iris.run_id") {
				t.Errorf("injected DSN %q does not carry the iris.run_id setting", got)
			}
		})

		t.Run("distinct runs get distinct injected connections", func(t *testing.T) {
			m := &manualExec{dbURL: base}
			a, _ := dbURLFromEnv(m.childEnv(1))
			b, _ := dbURLFromEnv(m.childEnv(2))
			if a == b {
				t.Errorf("runs 1 and 2 got the same injected DSN %q; each run must attribute to itself", a)
			}
		})

		t.Run("no base scoped DSN yields an empty connection, never a malformed one", func(t *testing.T) {
			// Until the scoped connection is provisioned (E04.4) the base is empty; the
			// injection stays empty rather than producing an options-only DSN.
			m := &manualExec{}
			got, ok := dbURLFromEnv(m.childEnv(42))
			if !ok {
				t.Fatal("manual child env carries no IRIS_DB_URL key")
			}
			if got != "" {
				t.Errorf("empty base injected %q, want an empty IRIS_DB_URL", got)
			}
		})
	})
}
