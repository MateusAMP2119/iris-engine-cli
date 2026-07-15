package daemon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg/pgtest"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// seedFile writes body to path, first creating the file's parent directory, so
// each fixture write is self-contained rather than relying on a sibling write to
// have created the shared parent.
func seedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("seed dir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// teardownWorkspace seeds a throwaway workspace with the on-disk engine artifacts
// uninstall must delete: an object store under objects_path holding artifact bytes
// and an archived journal partition, a control socket file, and a service unit
// file. It returns the resolved settings the teardown reads its paths from.
func teardownWorkspace(t *testing.T) config.Settings {
	t.Helper()
	ws := t.TempDir()
	s := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{}, config.Layer{})

	// The object store: content-addressed artifact bytes plus an archived sealed
	// partition, the two payload kinds uninstall removes with the store.
	seedFile(t, filepath.Join(s.ObjectsPath, "deadbeef.artifact"), "built binary bytes")
	seedFile(t, filepath.Join(s.ObjectsPath, "c0ffee.partition.seal"), "archived journal partition")
	seedFile(t, s.Socket, "socket")
	seedFile(t, daemon.ServiceUnitPath(s), "unit")
	return s
}

// runUninstall drives UninstallEngine over recording database fakes and the seeded
// workspace, with the live-candidate check defaulting to proceed, and returns the
// recorded cluster/data statements, the report, and the settings.
func runUninstall(t *testing.T) (cluster *storetest.Recorder, data *pgtest.Recorder, rep daemon.UninstallReport, s config.Settings) {
	t.Helper()
	s = teardownWorkspace(t)
	cluster = storetest.NewRecorder()
	data = pgtest.New()
	rep, err := daemon.UninstallEngine(context.Background(), daemon.UninstallDeps{
		LiveCandidate: daemon.ProceedWithoutLiveCheck(),
		Cluster:       cluster,
		Data:          data,
		Settings:      s,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("UninstallEngine: %v", err)
	}
	return cluster, data, rep, s
}

// TestUninstallEngineFullTeardown proves `iris engine uninstall` is a full engine
// teardown: it drops the meta database (all captured provenance with it), drops the
// data journal on the data connection, deletes the object store under objects_path,
// and removes the socket and service unit -- leaving nothing behind.
func TestUninstallEngineFullTeardown(t *testing.T) {
	cluster, data, rep, s := runUninstall(t)

	// meta is dropped on the cluster/maintenance connection (you cannot drop the
	// database you are connected to).
	if got := cluster.Statements(); len(got) != 1 || got[0] != store.DropMetaDatabaseDDL() {
		t.Errorf("cluster statements = %v, want exactly [%q]", got, store.DropMetaDatabaseDDL())
	}
	if !rep.MetaDropped {
		t.Error("report does not record the meta drop")
	}

	// the journal (and its dependent objects) is dropped on the data connection.
	if got := data.Statements(); len(got) != len(pg.JournalTeardownDDL()) {
		t.Fatalf("data statements = %v, want the journal teardown %v", got, pg.JournalTeardownDDL())
	}
	for i, want := range pg.JournalTeardownDDL() {
		if data.Statements()[i] != want {
			t.Errorf("data statement %d = %q, want %q", i, data.Statements()[i], want)
		}
	}
	if !rep.JournalDropped {
		t.Error("report does not record the journal drop")
	}

	// the object store, socket, and service unit are gone from disk.
	for _, path := range []string{s.ObjectsPath, s.Socket, daemon.ServiceUnitPath(s)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still present after uninstall (stat err = %v)", path, err)
		}
	}
	// and the report names each removed path.
	for _, path := range []string{s.ObjectsPath, s.Socket, daemon.ServiceUnitPath(s)} {
		if !containsString(rep.Removed, path) {
			t.Errorf("report Removed %v does not name %s", rep.Removed, path)
		}
	}
}

// TestUninstallDropsEndpointsWithMeta proves endpoints do not outlive the engine:
// they are meta rows (the endpoints and endpoint_filters control tables), so the
// single DROP DATABASE meta the teardown issues removes them with everything else
// in meta -- there is no separate endpoint teardown and none can survive.
func TestUninstallDropsEndpointsWithMeta(t *testing.T) {
	cluster, _, _, _ := runUninstall(t)

	drop := store.DropMetaDatabaseDDL()
	if got := cluster.Statements(); len(got) != 1 || got[0] != drop {
		t.Fatalf("uninstall did not drop meta with one statement: %v", got)
	}
	if !strings.Contains(drop, store.MetaDatabase) {
		t.Errorf("meta drop %q does not target the meta database %q", drop, store.MetaDatabase)
	}

	// The endpoints live in meta: dropping the database drops them. If the roster
	// ever moved endpoints out of meta this guarantee would break, so assert they
	// are meta tables.
	roster := map[string]bool{}
	for _, tbl := range store.MetaSchema().Tables {
		roster[tbl.Name] = true
	}
	for _, endpointTable := range []string{"endpoints", "endpoint_filters"} {
		if !roster[endpointTable] {
			t.Errorf("%s is not a meta table; dropping meta would not drop endpoints", endpointTable)
		}
	}
}

// TestUninstallDropsEngineState proves the teardown drops the engine's state in
// full: meta, the journal and its dependent triggers (the CASCADE), and the object
// store under objects_path with both payload kinds -- artifact bytes and archived
// partitions.
func TestUninstallDropsEngineState(t *testing.T) {
	_, data, _, s := runUninstall(t)

	// The journal teardown cascades, so the journal's own triggers and partitions
	// go with it rather than being orphaned.
	joined := strings.Join(data.Statements(), "\n")
	if !strings.Contains(joined, pg.JournalName) {
		t.Errorf("journal teardown does not name %s: %q", pg.JournalName, joined)
	}
	if !strings.Contains(strings.ToUpper(joined), "CASCADE") {
		t.Errorf("journal teardown is not a cascade drop (its triggers/partitions would be orphaned): %q", joined)
	}

	// The object store directory is removed with its contents: the artifact bytes
	// and the archived partition are both gone, not just the directory entry.
	for _, path := range []string{
		s.ObjectsPath,
		filepath.Join(s.ObjectsPath, "deadbeef.artifact"),
		filepath.Join(s.ObjectsPath, "c0ffee.partition.seal"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall (stat err = %v); the object store and its payloads must be gone", path, err)
		}
	}
}

// containsString reports whether xs contains want.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestUninstallRefusesLiveCandidate proves engine uninstall refuses while any
// daemon candidate holds a meta connection (so shared meta is never dropped
// under a live candidate).
func TestUninstallRefusesLiveCandidate(t *testing.T) {
	t.Run("uninstall-refuses-live-candidate", func(t *testing.T) {
		s := teardownWorkspace(t)
		cluster := storetest.NewRecorder()
		data := pgtest.New()

		alwaysHeld := &heldPredicate{held: true}
		_, err := daemon.UninstallEngine(context.Background(), daemon.UninstallDeps{
			LiveCandidate: alwaysHeld,
			Cluster:       cluster,
			Data:          data,
			Settings:      s,
			Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err == nil || !errors.Is(err, daemon.ErrLiveCandidate) {
			t.Fatalf("UninstallEngine with live candidate = %v, want ErrLiveCandidate", err)
		}
		if stmts := cluster.Statements(); len(stmts) != 0 {
			t.Errorf("live-candidate guard let cluster execs %v; meta must not be touched", stmts)
		}
	})
}

// heldPredicate is a LiveCandidatePredicate that reports held for the test.
type heldPredicate struct{ held bool }

func (h *heldPredicate) LiveCandidateHoldsMeta(context.Context) (bool, error) {
	return h.held, nil
}
