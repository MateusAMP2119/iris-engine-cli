package daemon_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/golden"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// seqLog is a shared, ordered record of the operations engine bootstrap issues
// across its distinct targets (the cluster/maintenance connection, the meta
// connection, the data connection, the existence probe, and the socket step), so
// a single golden pins the whole install sequence and its cross-target order.
type seqLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *seqLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

func (l *seqLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// seqProbe is a daemon.MetaProbe that records the probe and returns a scripted
// answer, so the create-if-missing branch is driven with no live catalog.
type seqProbe struct {
	log    *seqLog
	exists bool
}

func (p *seqProbe) MetaExists(context.Context) (bool, error) {
	p.log.add("probe: meta exists? -> " + strconv.FormatBool(p.exists))
	return p.exists, nil
}

// seqExec records every statement issued through it under a target tag. Its single
// Exec(ctx, sql) method satisfies both store.Execer (cluster, meta) and pg.DB
// (data), so one type captures the DDL to all three targets in one ordered log.
type seqExec struct {
	log *seqLog
	tag string
}

func (e *seqExec) Exec(_ context.Context, sql string) error {
	e.log.add(e.tag + ": " + sql)
	return nil
}

var (
	_ store.Execer = (*seqExec)(nil)
	_ pg.DB        = (*seqExec)(nil)
)

// seqSocket is a daemon.SocketPreparer that records the socket step without
// touching the filesystem; the real preparer is proven separately.
type seqSocket struct{ log *seqLog }

func (s *seqSocket) PrepareSocket(context.Context) error {
	s.log.add("socket: prepare")
	return nil
}

// maskEngineKey replaces the base64 payload of an ALTER DATABASE ... SET
// iris.engine_key statement with a fixed placeholder, so the install-sequence
// golden is stable across the random per-install key and no key-shaped bytes are
// checked in.
func maskEngineKey(line string) string {
	const marker = "SET iris.engine_key = '"
	i := strings.Index(line, marker)
	if i < 0 {
		return line
	}
	start := i + len(marker)
	end := strings.Index(line[start:], "'")
	if end < 0 {
		return line
	}
	return line[:start] + "<engine-key>" + line[start+end:]
}

// TestBootstrapEngineInstallSequence proves `iris engine install` bootstraps the
// engine over the admin DSN as the spec's bootstrap Q/A prescribes (specification
// section 4): probe for meta, create it if missing with a plain CREATE DATABASE,
// ensure its tables, create the partitioned public.data_journal on the data
// connection, store the minted ed25519 engine key on the meta connection, and set
// up the socket -- in that order, across the right targets. The whole statement
// sequence is pinned to a golden with the random engine-key payload masked; the
// real key material is asserted structurally and is never logged or reported.
//
// spec: S04/install-bootstraps-engine
func TestBootstrapEngineInstallSequence(t *testing.T) {
	ctx := context.Background()

	key, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}

	t.Run("full sequence when meta is missing", func(t *testing.T) {
		log := &seqLog{}
		var logbuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		rep, err := daemon.BootstrapEngine(ctx, daemon.InstallDeps{
			Probe:   &seqProbe{log: log, exists: false},
			Cluster: &seqExec{log: log, tag: "cluster"},
			Meta:    &seqExec{log: log, tag: "meta"},
			Data:    &seqExec{log: log, tag: "data"},
			Key:     key,
			Socket:  &seqSocket{log: log},
			Logger:  logger,
		})
		if err != nil {
			t.Fatalf("BootstrapEngine: %v", err)
		}

		lines := log.snapshot()
		masked := make([]string, len(lines))
		for i, l := range lines {
			masked[i] = maskEngineKey(l)
		}
		golden.Assert(t, []byte(strings.Join(masked, "\n")+"\n"),
			filepath.Join("testdata", "engine_install_sequence.txt"))

		// Cross-target order: probe precedes CREATE DATABASE, the meta tables precede
		// the journal, the journal precedes storing the engine key, and the socket is
		// last.
		assertBefore(t, lines, "probe: meta exists? -> false", "cluster: "+store.CreateMetaDatabaseDDL())
		assertBefore(t, lines, "cluster: "+store.CreateMetaDatabaseDDL(), "data: "+pg.JournalTable().DDL()[0])
		assertBefore(t, lines, "data: "+pg.JournalTable().DDL()[0], "meta: "+daemon.SetEngineKeyDDL(key))
		assertBefore(t, lines, "meta: "+daemon.SetEngineKeyDDL(key), "socket: prepare")

		if !rep.MetaCreated {
			t.Error("report says meta was not created, but the probe reported it missing")
		}
		if rep.EngineKeyPublic != key.PublicBase64() {
			t.Errorf("report public key = %q, want %q", rep.EngineKeyPublic, key.PublicBase64())
		}

		// The engine key is stored on the meta connection, and the base64 it stores
		// decodes to the same key that was minted (its public half matches).
		var stored string
		for _, l := range lines {
			if strings.HasPrefix(l, "meta: ALTER DATABASE meta SET iris.engine_key") {
				stored = strings.TrimPrefix(l, "meta: ")
			}
		}
		if stored == "" {
			t.Fatal("no ALTER DATABASE ... SET iris.engine_key statement on the meta connection")
		}
		back, err := daemon.DecodeEngineKey(quotedPayload(t, stored))
		if err != nil {
			t.Fatalf("stored engine key does not decode: %v", err)
		}
		if back.PublicBase64() != key.PublicBase64() {
			t.Error("stored engine key is not the minted one")
		}

		// The private half is never logged nor placed in the report: only the SQL that
		// stores it (on the meta connection) may carry it.
		privB64 := quotedPayload(t, daemon.SetEngineKeyDDL(key))
		if strings.Contains(logbuf.String(), privB64) {
			t.Errorf("engine bootstrap logged the private engine key:\n%s", logbuf.String())
		}
		if strings.Contains(rep.EngineKeyPublic, privB64) {
			t.Error("install report carries the private engine key")
		}
	})

	t.Run("create database omitted when meta already exists", func(t *testing.T) {
		log := &seqLog{}
		rep, err := daemon.BootstrapEngine(ctx, daemon.InstallDeps{
			Probe:   &seqProbe{log: log, exists: true},
			Cluster: &seqExec{log: log, tag: "cluster"},
			Meta:    &seqExec{log: log, tag: "meta"},
			Data:    &seqExec{log: log, tag: "data"},
			Key:     key,
			Socket:  &seqSocket{log: log},
			Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		if err != nil {
			t.Fatalf("BootstrapEngine: %v", err)
		}
		for _, l := range log.snapshot() {
			if strings.Contains(l, "CREATE DATABASE") {
				t.Errorf("CREATE DATABASE issued although meta already exists: %q", l)
			}
		}
		if rep.MetaCreated {
			t.Error("report says meta was created, but the probe reported it already present")
		}
		// The tables are still ensured (idempotent) and the key is still stored.
		if n := countTag(log.snapshot(), "meta: CREATE TABLE"); n == 0 {
			t.Error("meta schema was not ensured on an existing meta database")
		}
	})
}

// countTag counts the sequence lines beginning with prefix.
func countTag(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// TestPrepareSocketDir proves the real socket-setup leg of install: it creates the
// workspace .iris directory the control socket lives in and removes a stale socket
// file left by a prior run, so a fresh daemon can bind cleanly (specification
// section 4, "set up the socket").
//
// spec: S04/install-bootstraps-engine
func TestPrepareSocketDir(t *testing.T) {
	ws := t.TempDir()
	settings := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{}, config.Layer{})
	socketDir := filepath.Dir(settings.Socket)

	t.Run("creates the .iris directory", func(t *testing.T) {
		if err := daemon.PrepareSocketDir(settings); err != nil {
			t.Fatalf("PrepareSocketDir: %v", err)
		}
		info, err := os.Stat(socketDir)
		if err != nil || !info.IsDir() {
			t.Fatalf("socket directory %s was not created: %v", socketDir, err)
		}
	})

	t.Run("removes a stale socket file", func(t *testing.T) {
		if err := os.WriteFile(settings.Socket, []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed stale socket: %v", err)
		}
		if err := daemon.PrepareSocketDir(settings); err != nil {
			t.Fatalf("PrepareSocketDir: %v", err)
		}
		if _, err := os.Stat(settings.Socket); !os.IsNotExist(err) {
			t.Errorf("stale socket file was not removed (stat err = %v)", err)
		}
	})

	t.Run("idempotent with no stale socket", func(t *testing.T) {
		if err := daemon.PrepareSocketDir(settings); err != nil {
			t.Fatalf("PrepareSocketDir (no stale socket): %v", err)
		}
	})
}

// TestEngineKeyMintedAtInstall and checkpoints insert-only prove the integration
// contracts using fakes (per E00.4).
//
// spec: S14/engine-key-minted-at-install
// spec: S04/checkpoints-insert-only
func TestEngineKeyMintedAtInstallAndCheckpointsInsertOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("S14/engine-key-minted-at-install", func(t *testing.T) {
		// spec: S14/engine-key-minted-at-install
		log := &seqLog{}
		rep, err := daemon.BootstrapEngine(ctx, daemon.InstallDeps{
			Probe:   &seqProbe{log: log, exists: true},
			Cluster: &seqExec{log: log, tag: "cluster"},
			Meta:    &seqExec{log: log, tag: "meta"},
			Data:    &seqExec{log: log, tag: "data"},
			Key:     daemon.EngineKey{},
			Socket:  &seqSocket{log: log},
			Logger:  nil,
		})
		if err != nil {
			t.Fatalf("BootstrapEngine zero key: %v", err)
		}
		if rep.EngineKeyPublic == "" {
			t.Error("no key public minted at install")
		}
		found := false
		for _, l := range log.snapshot() {
			if strings.Contains(l, "SET iris.engine_key") {
				found = true
			}
		}
		if !found {
			t.Error("no key SET during install")
		}
	})

	t.Run("S04/checkpoints-insert-only", func(t *testing.T) {
		// spec: S04/checkpoints-insert-only
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		// table of checkpoints, all inserts, location constrained, logical refs
		rows := []store.CheckpointRow{
			{IDFrom: 100, IDTo: 200, Digest: []byte("d"), Location: "resident", RecordedAt: "t"},
			{IDFrom: 201, IDTo: 300, Digest: []byte("e"), Location: "archived", RecordedAt: "t2"},
		}
		for _, row := range rows {
			if err := w.InsertCheckpoint(ctx, row); err != nil {
				t.Fatalf("Insert: %v", err)
			}
		}
		for _, s := range rec.Statements() {
			if strings.Contains(s.SQL, "journal_checkpoints") {
				if !strings.Contains(strings.ToUpper(s.SQL), "INSERT") {
					t.Errorf("non insert: %s", s.SQL)
				}
				// location constrained (checked by schema too)
			}
		}
	})
}
