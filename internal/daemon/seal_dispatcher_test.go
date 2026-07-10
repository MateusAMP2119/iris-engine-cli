package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/archive"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// orderLog records the seal step's observable actions in the order they occur, so
// the dispatcher-step contract (compact, then checkpoint, then archive) is provable.
type orderLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

// fakeSealData is a recording sealDataStore: it returns fixed resident stats and
// compacted rows and logs each call so the seal flow can be asserted without a live
// database.
type fakeSealData struct {
	log          *orderLog
	count        int64
	minID, maxID int64
	runIDs       []int64
	rows         [][]byte

	compactFrom, compactTo int64
	queryFrom, queryTo     int64
	compacted, queried     int
	drops                  int
}

func (f *fakeSealData) ResidentJournalStats(context.Context) (int64, int64, int64, error) {
	return f.count, f.minID, f.maxID, nil
}

func (f *fakeSealData) ResidentRunIDs(context.Context) ([]int64, error) {
	return f.runIDs, nil
}

func (f *fakeSealData) CompactJournalRange(_ context.Context, from, to int64) error {
	f.compactFrom, f.compactTo = from, to
	f.compacted++
	f.log.add("compact")
	return nil
}

func (f *fakeSealData) QueryCompactedRows(_ context.Context, from, to int64) ([][]byte, error) {
	f.queryFrom, f.queryTo = from, to
	f.queried++
	f.log.add("query")
	return f.rows, nil
}

func (f *fakeSealData) DropPartitionForRange(context.Context, int64) error {
	f.drops++
	f.log.add("drop")
	return nil
}

// metaKeyState models the single-row engine_key meta table across the read seam
// (fakeSealMeta.ReadEngineKey) and the write seam (the INSERT the seal submits
// through the single writer, captured by recSealConn): a create-once store whose
// first writer wins, so the seal's mint-on-first-need path is provable end to end
// with no live database.
type metaKeyState struct {
	mu   sync.Mutex
	priv []byte
}

func (s *metaKeyState) get() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.priv...)
}

// setIfEmpty models INSERT ... ON CONFLICT (id) DO NOTHING: it stores priv only when
// no key is present, so a second minter is a no-op.
func (s *metaKeyState) setIfEmpty(priv []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv == nil {
		s.priv = append([]byte(nil), priv...)
	}
}

// fakeSealMeta is a canned JournalSealReader over a shared engine-key state.
type fakeSealMeta struct {
	prev    *store.CheckpointRow
	running int64
	keys    *metaKeyState
}

func (m fakeSealMeta) LatestCheckpoint(context.Context) (*store.CheckpointRow, error) {
	return m.prev, nil
}
func (m fakeSealMeta) RunningAmong(_ context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return m.running, nil
}
func (m fakeSealMeta) ReadEngineKey(context.Context) ([]byte, error) {
	if m.keys == nil {
		return nil, nil
	}
	if raw := m.keys.get(); len(raw) > 0 {
		return raw, nil
	}
	return nil, nil
}

// recSealConn is a recording store.MetaWriteConn: it captures every write the seal
// submits through the single dispatcher (the checkpoint insert and the archive
// flip), so the contract's checkpoint and archive steps are observable.
type recSealConn struct {
	log   *orderLog
	keys  *metaKeyState // when set, the engine_key INSERT lands here (create-once)
	mu    sync.Mutex
	execs []recSealExec
}

type recSealExec struct {
	sql  string
	args []any
}

func (c *recSealConn) Exec(_ context.Context, sql string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, recSealExec{sql: sql, args: args})
	switch {
	case strings.Contains(sql, "INSERT INTO journal_checkpoints"):
		c.log.add("checkpoint")
	case strings.Contains(sql, "SET location = 'archived'"):
		c.log.add("archive-flip")
	case strings.Contains(sql, "INSERT INTO engine_key"):
		c.log.add("mint-key")
		if c.keys != nil && len(args) > 0 {
			if priv, ok := args[0].([]byte); ok {
				c.keys.setIfEmpty(priv)
			}
		}
	}
	return nil
}

func (c *recSealConn) find(substr string) (recSealExec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.execs {
		if strings.Contains(e.sql, substr) {
			return e, true
		}
	}
	return recSealExec{}, false
}

// newTestEngineKey mints an ed25519 key and returns its raw private half (as the
// engine_key meta table stores it) plus the public half (for signature
// verification). The sealer loads the private half from the meta read seam.
func newTestEngineKey(t *testing.T) (priv []byte, pub ed25519.PublicKey) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate engine key: %v", err)
	}
	return privKey, pubKey
}

// startSealDispatcher builds a real single-writer dispatcher over the recording
// connection and returns it started, with cleanup registered.
func startSealDispatcher(t *testing.T, conn store.MetaWriteConn) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(conn)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	t.Cleanup(func() { cancel(); d.Stop() })
	return d
}

// TestSealDispatcherStep proves sealing runs as an opportunistic dispatcher step:
// when the resident partition is due, the step executes compact, then checkpoint,
// then archive (export + drop + flip). The checkpoint carries the real digest over
// the compacted rows, a signature that verifies against the engine key, and the
// parent that chains to the current head; the sealed partition is exported to the
// object store under its digest and dropped.
//
// spec: S14/seal-dispatcher-step
func TestSealDispatcherStep(t *testing.T) {
	t.Run("S14/seal-dispatcher-step", func(t *testing.T) {
		log := &orderLog{}
		rows := [][]byte{[]byte("1|role|7|analytics|orders|a|insert||promoted|t"), []byte("2|role|7|analytics|orders|b|insert||promoted|t")}
		data := &fakeSealData{log: log, count: 6, minID: 1, maxID: 6, runIDs: []int64{7}, rows: rows}

		priv, pub := newTestEngineKey(t)
		keys := &metaKeyState{priv: priv} // key already minted at install
		prev := &store.CheckpointRow{Seq: 3, IDFrom: 1, IDTo: 4, Digest: []byte("prevdigest")}
		meta := fakeSealMeta{prev: prev, running: 0, keys: keys}

		conn := &recSealConn{log: log, keys: keys}
		d := startSealDispatcher(t, conn)
		objects := store.NewObjectStore(t.TempDir())

		s := newJournalSealer(5, data, meta, d, objects, nil)
		s.sealAfterPass(context.Background())

		// Order: compact, then checkpoint, then archive (drop then flip).
		steps := log.snapshot()
		wantOrder := []string{"compact", "query", "checkpoint", "drop", "archive-flip"}
		if len(steps) != len(wantOrder) {
			t.Fatalf("seal steps = %v, want %v", steps, wantOrder)
		}
		for i := range wantOrder {
			if steps[i] != wantOrder[i] {
				t.Fatalf("seal step %d = %q, want %q (full: %v)", i, steps[i], wantOrder[i], steps)
			}
		}

		// Compaction and the compacted-row read span the resident partition's exact
		// half-open id range [minID, maxID+1).
		if data.compactFrom != 1 || data.compactTo != 7 {
			t.Errorf("compact range = [%d,%d), want [1,7)", data.compactFrom, data.compactTo)
		}
		if data.queryFrom != 1 || data.queryTo != 7 {
			t.Errorf("query range = [%d,%d), want [1,7)", data.queryFrom, data.queryTo)
		}
		if data.drops != 1 {
			t.Errorf("partition drops = %d, want 1", data.drops)
		}

		// The checkpoint carries the real digest over the compacted rows, chains to
		// the previous head, and its signature verifies against the engine key.
		ins, ok := conn.find("INSERT INTO journal_checkpoints")
		if !ok {
			t.Fatal("no checkpoint insert submitted")
		}
		idFrom, _ := ins.args[0].(int64)
		idTo, _ := ins.args[1].(int64)
		digest, _ := ins.args[2].([]byte)
		parent, _ := ins.args[3].([]byte)
		sig, _ := ins.args[4].([]byte)
		if idFrom != 1 || idTo != 6 {
			t.Errorf("checkpoint id range = [%d,%d], want [1,6]", idFrom, idTo)
		}
		wantDigest := store.ComputeDigest(rows)
		if string(digest) != string(wantDigest) {
			t.Errorf("checkpoint digest = %x, want %x (real hash over compacted rows)", digest, wantDigest)
		}
		if string(parent) != string(prev.Digest) {
			t.Errorf("checkpoint parent = %x, want %x (chains to head)", parent, prev.Digest)
		}
		if !ed25519.Verify(pub, digest, sig) {
			t.Error("checkpoint signature does not verify against the engine key")
		}

		// The sealed partition is exported to the object store under its digest, and
		// the exported bytes carry the same digest and signature (offline-verifiable).
		key := fmt.Sprintf("%x", wantDigest)
		h, got, err := archive.Read(objects.Path(key))
		if err != nil {
			t.Fatalf("read exported partition object: %v", err)
		}
		if string(h.Digest) != string(wantDigest) {
			t.Errorf("exported header digest = %x, want %x", h.Digest, wantDigest)
		}
		if !ed25519.Verify(pub, h.Digest, h.Signature) {
			t.Error("exported partition signature does not verify against the engine key")
		}
		if string(store.ComputeDigest(got)) != string(wantDigest) {
			t.Error("exported rows digest does not match the compacted rows")
		}
	})
}

// TestSealMintsEngineKeyOnFirstNeed proves the seal loads the engine key from the
// engine_key meta table and, when meta holds none yet (an engine installed before
// the key existed), mints one create-once (INSERT ... ON CONFLICT DO NOTHING)
// through the single writer before signing -- the meta-table persistence that
// superseded the per-database GUC and the workspace key file. The checkpoint is
// then signed with exactly the key stored in meta.
//
// spec: S04/engine-key-in-meta-table
func TestSealMintsEngineKeyOnFirstNeed(t *testing.T) {
	t.Run("S04/engine-key-in-meta-table", func(t *testing.T) {
		log := &orderLog{}
		rows := [][]byte{[]byte("1|role|7|analytics|orders|a|insert||promoted|t")}
		data := &fakeSealData{log: log, count: 6, minID: 1, maxID: 6, runIDs: []int64{7}, rows: rows}

		keys := &metaKeyState{} // empty: no key minted at install yet
		meta := fakeSealMeta{prev: nil, running: 0, keys: keys}
		conn := &recSealConn{log: log, keys: keys}
		d := startSealDispatcher(t, conn)
		objects := store.NewObjectStore(t.TempDir())

		s := newJournalSealer(5, data, meta, d, objects, nil)
		s.sealAfterPass(context.Background())

		// The seal minted the engine key into meta before signing, create-once.
		ins, ok := conn.find("INSERT INTO engine_key")
		if !ok {
			t.Fatal("seal did not mint the engine key when meta held none (S04/engine-key-in-meta-table)")
		}
		if !strings.Contains(ins.sql, "ON CONFLICT") {
			t.Errorf("engine-key mint is not create-once (missing ON CONFLICT DO NOTHING): %s", ins.sql)
		}
		if len(ins.args) == 0 {
			t.Fatal("engine-key insert carried no private-key argument")
		}

		// The minted key is now stored in meta; the checkpoint signature verifies
		// against exactly that key's public half (the seal signed with the stored key,
		// not a discarded second mint).
		stored := keys.get()
		if len(stored) == 0 {
			t.Fatal("engine key not stored in meta after mint (S04/engine-key-in-meta-table)")
		}
		key, err := DecodeEngineKeyBytes(stored)
		if err != nil {
			t.Fatalf("decode stored engine key: %v", err)
		}
		cp, ok := conn.find("INSERT INTO journal_checkpoints")
		if !ok {
			t.Fatal("no checkpoint after mint-on-first-need")
		}
		digest, _ := cp.args[2].([]byte)
		sig, _ := cp.args[4].([]byte)
		if !key.VerifyDigest(digest, sig) {
			t.Error("checkpoint signed with a key that differs from the one minted into meta (S04/engine-key-in-meta-table)")
		}
	})
}

// TestSealDispatcherStepDefers proves the step is a no-op when the partition is not
// due: below threshold, and while a run is still in flight, no compaction,
// checkpoint, or export occurs.
//
// spec: S14/seal-dispatcher-step
func TestSealDispatcherStepDefers(t *testing.T) {
	t.Run("S14/seal-dispatcher-step", func(t *testing.T) {
		priv, _ := newTestEngineKey(t)
		rows := [][]byte{[]byte("row")}

		cases := []struct {
			name    string
			count   int64
			running int64
		}{
			{"below threshold does not seal", 3, 0},
			{"in-flight run defers seal", 10, 1},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				log := &orderLog{}
				data := &fakeSealData{log: log, count: c.count, minID: 1, maxID: c.count, runIDs: []int64{42}, rows: rows}
				keys := &metaKeyState{priv: priv}
				meta := fakeSealMeta{running: c.running, keys: keys}
				conn := &recSealConn{log: log, keys: keys}
				d := startSealDispatcher(t, conn)
				objects := store.NewObjectStore(t.TempDir())

				s := newJournalSealer(5, data, meta, d, objects, nil)
				s.sealAfterPass(context.Background())

				if data.compacted != 0 || data.queried != 0 || data.drops != 0 {
					t.Errorf("not-due seal touched the data journal: compacted=%d queried=%d drops=%d", data.compacted, data.queried, data.drops)
				}
				if _, ok := conn.find("INSERT INTO journal_checkpoints"); ok {
					t.Error("not-due seal submitted a checkpoint")
				}
				if steps := log.snapshot(); len(steps) != 0 {
					t.Errorf("not-due seal ran steps %v, want none", steps)
				}
			})
		}
	})
}
