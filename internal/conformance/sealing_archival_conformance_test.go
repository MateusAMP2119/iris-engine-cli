//go:build conformance

package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/archive"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// checkpointRow is one journal_checkpoints row read back from meta for chain and
// signature validation.
type checkpointRow struct {
	seq       int64
	idFrom    int64
	idTo      int64
	digest    []byte
	parent    []byte
	signature []byte
	location  string
}

// readCheckpoints reads every journal_checkpoints row from meta in seq order.
func readCheckpoints(ctx context.Context, t *testing.T, metaConn *pgx.Conn) []checkpointRow {
	t.Helper()
	rows, err := metaConn.Query(ctx,
		"SELECT seq, id_from, id_to, digest, parent_digest, signature, location FROM journal_checkpoints ORDER BY seq")
	if err != nil {
		t.Fatalf("read journal_checkpoints: %v", err)
	}
	defer rows.Close()
	var out []checkpointRow
	for rows.Next() {
		var c checkpointRow
		if err := rows.Scan(&c.seq, &c.idFrom, &c.idTo, &c.digest, &c.parent, &c.signature, &c.location); err != nil {
			t.Fatalf("scan checkpoint: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate checkpoints: %v", err)
	}
	return out
}

// engineKeyFromWorkspace loads the engine key from the workspace key file install
// minted, so the test can verify checkpoint signatures against the same key the
// daemon signs with (an offline auditor uses only the public half).
func engineKeyFromWorkspace(t *testing.T, ws string) daemon.EngineKey {
	t.Helper()
	path := filepath.Join(ws, ".iris", daemon.EngineKeyFileName)
	key, err := daemon.LoadOrMintEngineKey(path)
	if err != nil {
		t.Fatalf("load engine key from workspace %s: %v", path, err)
	}
	return key
}

// waitForCheckpoints polls meta until at least n journal_checkpoints rows exist, or
// the deadline passes; it returns the rows seen (which may be fewer than n on
// timeout, so the caller asserts).
func waitForCheckpoints(ctx context.Context, t *testing.T, metaConn *pgx.Conn, n int, within time.Duration) []checkpointRow {
	t.Helper()
	deadline := time.Now().Add(within)
	var cps []checkpointRow
	for time.Now().Before(deadline) {
		cps = readCheckpoints(ctx, t, metaConn)
		if len(cps) >= n {
			return cps
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cps
}

// TestSealWaitsForInflightRun drives the real binary and a running daemon against
// a real Postgres and proves sealing does not cut a partition mid-run: with a
// tiny journal_partition_rows, a run whose writes cross the threshold while it is
// still running causes no seal until the run reaches a terminal state; the run's
// journal window stays wholly inside one sealed partition.
//
// spec: S13/seal-waits-for-inflight-run
func TestSealWaitsForInflightRun(t *testing.T) {
	t.Run("S13/seal-waits-for-inflight-run", func(t *testing.T) {
		freshDatabases(t)
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Tiny threshold so writes easily cross it.
		t.Setenv("IRIS_JOURNAL_PARTITION_ROWS", "5")

		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			t.Fatalf("daemon socket never became ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader")
		}

		dataDSN := dataSourceForWorkspace(t, ws)
		adminDSN := adminDataDSN(t, ws)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		client, err := pg.Connect(ctx, testConnSource{dsn: adminDSN})
		if err != nil {
			t.Fatalf("connect data admin: %v", err)
		}
		t.Cleanup(client.Close)
		_ = pg.EnsureJournal(ctx, client)
		_ = client.EnsureCaptureFunction(ctx)
		for _, stmt := range []string{
			`CREATE SCHEMA IF NOT EXISTS analytics`,
			`CREATE TABLE IF NOT EXISTS analytics.orders (id integer PRIMARY KEY, amount numeric)`,
		} {
			_ = client.Exec(ctx, stmt)
		}
		for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
			_ = client.Exec(ctx, trig)
		}

		writePipelineDecl(t, ws, "sleeper", "name: sleeper\nrun: [\"sh\", \"-c\", \"sleep 6; exit 0\"]\nwrites:\n  - table: analytics.orders\n    fields: [id, amount]\n")

		bin.Run(t, RunOptions{Args: []string{"declare", "apply", filepath.Join("pipelines", "sleeper")}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

		// Launch run async so it is in-flight while we cross threshold.
		runDone := make(chan Result, 1)
		go func() {
			res := bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "sleeper"}, Dir: ws, Timeout: 2 * time.Minute})
			runDone <- res
		}()

		metaConn, err := pgx.Connect(ctx, metaDSN(t, ws))
		if err != nil {
			t.Fatalf("connect meta: %v", err)
		}
		defer func() { _ = metaConn.Close(ctx) }()

		var runID int64
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var state string
			if err := metaConn.QueryRow(ctx,
				"SELECT id, state FROM runs WHERE pipeline='sleeper' ORDER BY id DESC LIMIT 1",
			).Scan(&runID, &state); err == nil && runID != 0 && state == "running" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if runID == 0 {
			t.Fatalf("no sleeper run recorded in running state")
		}

		dataConn, err := pgx.Connect(ctx, pg.InjectRunID(dataDSN, runID))
		if err != nil {
			t.Fatalf("connect data with run: %v", err)
		}
		defer func() { _ = dataConn.Close(ctx) }()
		for i := 0; i < 10; i++ {
			if _, err := dataConn.Exec(ctx, fmt.Sprintf("INSERT INTO analytics.orders (id, amount) VALUES (%d, %d)", 100+i, i)); err != nil {
				t.Fatalf("write row %d: %v", i, err)
			}
		}

		// While the run is in flight (running) with the threshold crossed, the seal
		// step must not have cut a partition: sealing is a post-terminal step, and the
		// in-flight guard defers it, so no checkpoint exists yet.
		var ncpDuring int
		_ = metaConn.QueryRow(ctx, "SELECT count(*) FROM journal_checkpoints").Scan(&ncpDuring)
		if ncpDuring != 0 {
			t.Errorf("checkpoint cut while run %d still in flight; seal did not wait for the in-flight run (S13/seal-waits-for-inflight-run)", runID)
		}

		var res Result
		select {
		case res = <-runDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("sleeper run did not complete")
		}
		res.RequireExit(t, 0)

		var ceiling int64
		_ = metaConn.QueryRow(ctx, "SELECT COALESCE(journal_ceiling,0) FROM runs WHERE id=$1", runID).Scan(&ceiling)
		if ceiling == 0 {
			t.Fatalf("run %d has no journal_ceiling after terminal; seal step did not record window (S13/seal-waits-for-inflight-run)", runID)
		}

		// After the run finished, the resident partition is due (10 rows > threshold 5)
		// and no run is in flight, so a checkpoint has been cut. Its id range must
		// contain the run's whole journal window: no cut falls strictly inside
		// [floor, ceiling], proving the window was never split.
		cps := waitForCheckpoints(ctx, t, metaConn, 1, 10*time.Second)
		if len(cps) == 0 {
			t.Fatalf("no journal_checkpoints after crossing threshold with the run terminal; sealing did not occur (S13/seal-waits-for-inflight-run)")
		}
		var floor int64
		_ = metaConn.QueryRow(ctx, "SELECT COALESCE(journal_floor,0) FROM runs WHERE id=$1", runID).Scan(&floor)
		coversWindow := false
		for _, c := range cps {
			if c.idTo > floor && c.idTo < ceiling {
				t.Errorf("checkpoint id_to=%d falls inside run %d window (%d,%d]; seal split the window (S13/seal-waits-for-inflight-run)", c.idTo, runID, floor, ceiling)
			}
			if c.idFrom <= ceiling && c.idTo >= ceiling {
				coversWindow = true
			}
		}
		if !coversWindow {
			t.Errorf("no checkpoint covers run %d ceiling %d; the sealed partition does not hold the whole window (S13/seal-waits-for-inflight-run)", runID, ceiling)
		}

		// The checkpoint the seal cut carries a real signature over its digest, which
		// verifies against the engine key.
		key := engineKeyFromWorkspace(t, ws)
		for _, c := range cps {
			if len(c.signature) == 0 {
				t.Errorf("checkpoint seq %d has no signature (S13/seal-waits-for-inflight-run)", c.seq)
				continue
			}
			if !key.VerifyDigest(c.digest, c.signature) {
				t.Errorf("checkpoint seq %d signature does not verify against the engine key (S13/seal-waits-for-inflight-run)", c.seq)
			}
		}
	})
}

// TestSealCompactionDropsConsumed proves that after sealing, compaction nulls
// released pre-images and folds duplicate (schema,table,row_pk,run_id) stamps to
// the latest op, while each run's exact write set survives.
//
// spec: S13/seal-compaction-drops-consumed
func TestSealCompactionDropsConsumed(t *testing.T) {
	t.Run("S13/seal-compaction-drops-consumed", func(t *testing.T) {
		const (
			superuser = "postgres"
			superpw   = "superpw"
		)
		port := freePort(t)
		dataDir := filepath.Join(t.TempDir(), "data")
		runtimeDir := filepath.Join(t.TempDir(), "runtime")

		cluster := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V18).
			Username(superuser).Password(superpw).Database("postgres").
			Port(port).
			DataPath(dataDir).RuntimePath(runtimeDir).
			StartTimeout(90 * time.Second))
		if err := cluster.Start(); err != nil {
			t.Fatalf("start cluster: %v", err)
		}
		t.Cleanup(func() { _ = cluster.Stop() })

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		adminMaintenance := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", superuser, superpw, port)
		dataDSN := fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", superuser, superpw, port, pg.DataDatabase)

		client, err := pg.Connect(ctx, testConnSource{dsn: adminMaintenance})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)

		if err := pg.EnsureJournal(ctx, client); err != nil {
			t.Fatalf("EnsureJournal: %v", err)
		}
		if err := client.EnsureCaptureFunction(ctx); err != nil {
			t.Fatalf("EnsureCaptureFunction: %v", err)
		}
		for _, stmt := range []string{
			`CREATE SCHEMA IF NOT EXISTS analytics`,
			`CREATE TABLE analytics.orders (id integer PRIMARY KEY, amount numeric)`,
		} {
			_ = client.Exec(ctx, stmt)
		}
		for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
			_ = client.Exec(ctx, trig)
		}

		run := int64(424242)
		conn, err := pgx.Connect(ctx, pg.InjectRunID(dataDSN, run))
		if err != nil {
			t.Fatalf("writer conn: %v", err)
		}
		defer conn.Close(ctx)

		_, _ = conn.Exec(ctx, "INSERT INTO analytics.orders (id, amount) VALUES (1, 10)")
		_, _ = conn.Exec(ctx, "UPDATE analytics.orders SET amount=11 WHERE id=1")
		_, _ = conn.Exec(ctx, "UPDATE analytics.orders SET amount=12 WHERE id=1")

		// Drive compaction exactly as seal would on the range (0 upper = open tail).
		if cerr := client.CompactJournalRange(ctx, 0, 0); cerr != nil {
			t.Fatalf("CompactJournalRange: %v (S13/seal-compaction-drops-consumed)", cerr)
		}

		// After sealing compacts: released pre-images nulled, duplicate stamps folded
		// to the latest op per (schema,table,row_pk,run_id); each run's write set
		// survives.
		var pre int
		_ = conn.QueryRow(ctx, "SELECT count(*) FROM public.data_journal WHERE run_id=$1 AND pre_image IS NOT NULL", run).Scan(&pre)
		if pre != 0 {
			t.Errorf("expected released pre-images nulled after compaction, got %d (S13/seal-compaction-drops-consumed)", pre)
		}
		var rows int
		var op string
		if err := conn.QueryRow(ctx, "SELECT count(*), COALESCE(max(op), '') FROM public.data_journal WHERE run_id=$1", run).Scan(&rows, &op); err != nil {
			t.Fatalf("read folded row: %v", err)
		}
		if rows != 1 {
			t.Errorf("expected duplicate stamps folded to 1 per (s,t,pk,run), got %d (S13/seal-compaction-drops-consumed)", rows)
		}
		if op != "update" {
			t.Errorf("folded stamp op = %q, want the latest op (update) (S13/seal-compaction-drops-consumed)", op)
		}
	})
}

// TestSealedPartitionExportsDrops proves threshold gating end to end: a run whose
// resident writes stay below journal_partition_rows does NOT seal (no checkpoint, no
// exported object), and once further writes cross the threshold the partition seals
// -- exported to the object store under its checkpoint digest as a valid archive,
// its rows dropped from Postgres.
//
// spec: S13/sealed-partition-exports-drops
func TestSealedPartitionExportsDrops(t *testing.T) {
	t.Run("S13/sealed-partition-exports-drops", func(t *testing.T) {
		freshDatabases(t)
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		t.Setenv("IRIS_JOURNAL_PARTITION_ROWS", "3")

		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = WaitForSocket(readyCtx, socket)
		cancel()
		_ = waitForLeader(t, socket)

		writePipelineDecl(t, ws, "writer", "name: writer\nrun: [\"sh\", \"-c\", \"exit 0\"]\nwrites:\n  - table: analytics.orders\n    fields: [id, amount]\n")
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", filepath.Join("pipelines", "writer")}, Dir: ws}).RequireExit(t, 0)

		dataDSN := dataSourceForWorkspace(t, ws)
		adminDSN := adminDataDSN(t, ws)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		client, err := pg.Connect(ctx, testConnSource{dsn: adminDSN})
		if err != nil {
			t.Fatalf("pg connect: %v", err)
		}
		defer client.Close()
		_ = pg.EnsureJournal(ctx, client)
		_ = client.EnsureCaptureFunction(ctx)
		_ = client.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS analytics`)
		_ = client.Exec(ctx, `CREATE TABLE IF NOT EXISTS analytics.orders (id integer PRIMARY KEY, amount numeric)`)
		for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
			_ = client.Exec(ctx, trig)
		}

		metaConn, err := pgx.Connect(ctx, metaDSN(t, ws))
		if err != nil {
			t.Fatalf("connect meta: %v", err)
		}
		defer func() { _ = metaConn.Close(ctx) }()

		dataConn, err := pgx.Connect(ctx, pg.InjectRunID(dataDSN, 0))
		if err != nil {
			t.Fatalf("data conn: %v", err)
		}
		defer dataConn.Close(ctx)

		objects := filepath.Join(ws, ".iris", "objects")

		// Phase 1: below threshold. Two resident rows (< 3) then a terminal run -> the
		// partition is not due, so no seal: no checkpoint, no exported object.
		for i := 0; i < 2; i++ {
			if _, err := dataConn.Exec(ctx, fmt.Sprintf("INSERT INTO analytics.orders (id, amount) VALUES (%d, %d)", 200+i, i)); err != nil {
				t.Fatalf("phase-1 write %d: %v", i, err)
			}
		}
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "writer"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

		var ncp int
		_ = metaConn.QueryRow(ctx, "SELECT count(*) FROM journal_checkpoints").Scan(&ncp)
		if ncp != 0 {
			t.Errorf("below-threshold run sealed: %d checkpoints, want 0 (threshold gating, S13/sealed-partition-exports-drops)", ncp)
		}
		if entries, _ := os.ReadDir(objects); len(entries) != 0 {
			t.Errorf("below-threshold run exported %d objects, want 0 (S13/sealed-partition-exports-drops)", len(entries))
		}

		// Phase 2: cross the threshold. Two more rows (4 resident >= 3) then a terminal
		// run -> the partition seals: a checkpoint is cut, the partition is exported as
		// a valid archive under its digest, and its rows are dropped from Postgres.
		for i := 2; i < 4; i++ {
			if _, err := dataConn.Exec(ctx, fmt.Sprintf("INSERT INTO analytics.orders (id, amount) VALUES (%d, %d)", 200+i, i)); err != nil {
				t.Fatalf("phase-2 write %d: %v", i, err)
			}
		}
		bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "writer"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

		cps := waitForCheckpoints(ctx, t, metaConn, 1, 10*time.Second)
		if len(cps) == 0 {
			t.Fatalf("above-threshold run did not seal: no checkpoint (S13/sealed-partition-exports-drops)")
		}
		cp := cps[len(cps)-1]

		// The sealed partition is exported to the object store under its checkpoint
		// digest as a valid archive whose header digest and signature match the
		// checkpoint and verify against the engine key.
		objPath := filepath.Join(objects, fmt.Sprintf("%x", cp.digest))
		hdr, archRows, err := archive.Read(objPath)
		if err != nil {
			t.Fatalf("read exported partition object %s: %v (S13/sealed-partition-exports-drops)", objPath, err)
		}
		if string(hdr.Digest) != string(cp.digest) {
			t.Errorf("exported object digest %x != checkpoint digest %x (S13/sealed-partition-exports-drops)", hdr.Digest, cp.digest)
		}
		if string(store.ComputeDigest(archRows)) != string(cp.digest) {
			t.Errorf("digest over exported rows does not match the checkpoint digest (S13/sealed-partition-exports-drops)")
		}
		key := engineKeyFromWorkspace(t, ws)
		if !key.VerifyDigest(hdr.Digest, hdr.Signature) {
			t.Errorf("exported partition signature does not verify against the engine key (S13/sealed-partition-exports-drops)")
		}

		// The sealed rows are dropped from the resident journal: the sealed id range is
		// gone from Postgres (exported and detached), with the checkpoint flipped to
		// archived.
		var residentInRange int
		_ = dataConn.QueryRow(ctx, "SELECT count(*) FROM public.data_journal WHERE id >= $1 AND id <= $2", cp.idFrom, cp.idTo).Scan(&residentInRange)
		if residentInRange != 0 {
			t.Errorf("sealed id range [%d,%d] still resident in Postgres (%d rows); partition not dropped (S13/sealed-partition-exports-drops)", cp.idFrom, cp.idTo, residentInRange)
		}
		if cp.location != "archived" {
			t.Errorf("checkpoint location = %q, want archived after export+drop (S13/sealed-partition-exports-drops)", cp.location)
		}
	})
}

// TestCheckpointChainValidates proves that across two consecutive seals the
// journal_checkpoints rows form a valid chain: each digest signs and verifies
// against the engine key, each digest matches its exported partition bytes, and the
// second checkpoint's parent_digest chains to the first.
//
// spec: S13/checkpoint-chain-validates
func TestCheckpointChainValidates(t *testing.T) {
	t.Run("S13/checkpoint-chain-validates", func(t *testing.T) {
		freshDatabases(t)
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		t.Setenv("IRIS_JOURNAL_PARTITION_ROWS", "2")

		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = WaitForSocket(readyCtx, socket)
		cancel()
		_ = waitForLeader(t, socket)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		metaConn, err := pgx.Connect(ctx, metaDSN(t, ws))
		if err != nil {
			t.Fatalf("meta conn: %v", err)
		}
		defer metaConn.Close(ctx)

		dataDSN := dataSourceForWorkspace(t, ws)
		adminDSN := adminDataDSN(t, ws)
		client, err := pg.Connect(ctx, testConnSource{dsn: adminDSN})
		if err != nil {
			t.Fatalf("pg connect for checkpoint drive: %v", err)
		}
		defer client.Close()
		_ = pg.EnsureJournal(ctx, client)
		_ = client.EnsureCaptureFunction(ctx)
		_ = client.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS analytics`)
		_ = client.Exec(ctx, `CREATE TABLE IF NOT EXISTS analytics.orders (id integer PRIMARY KEY, amount numeric)`)
		for _, trig := range pg.RenderCaptureTriggers("analytics", "orders") {
			_ = client.Exec(ctx, trig)
		}
		writePipelineDecl(t, ws, "ckpt", "name: ckpt\nrun: [\"sh\", \"-c\", \"exit 0\"]\nwrites:\n  - table: analytics.orders\n    fields: [id, amount]\n")
		bin.Run(t, RunOptions{Args: []string{"declare", "apply", filepath.Join("pipelines", "ckpt")}, Dir: ws}).RequireExit(t, 0)

		dataConn, err := pgx.Connect(ctx, pg.InjectRunID(dataDSN, 0))
		if err != nil {
			t.Fatalf("data conn for ckpt: %v", err)
		}
		defer dataConn.Close(ctx)

		// Two consecutive seals: each writes 3 rows (> threshold 2) then a terminal run
		// that seals the resident partition. The second seal starts from a fresh tail
		// (the first partition was dropped), so its checkpoint chains to the first.
		id := 300
		for pass := 0; pass < 2; pass++ {
			for i := 0; i < 3; i++ {
				if _, err := dataConn.Exec(ctx, fmt.Sprintf("INSERT INTO analytics.orders (id, amount) VALUES (%d, %d)", id, i)); err != nil {
					t.Fatalf("pass %d write %d: %v", pass, i, err)
				}
				id++
			}
			bin.Run(t, RunOptions{Args: []string{"pipeline", "run", "ckpt"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)
			if cps := waitForCheckpoints(ctx, t, metaConn, pass+1, 10*time.Second); len(cps) < pass+1 {
				t.Fatalf("after pass %d: %d checkpoints, want at least %d (S13/checkpoint-chain-validates)", pass, len(cps), pass+1)
			}
		}

		cps := readCheckpoints(ctx, t, metaConn)
		if len(cps) < 2 {
			t.Fatalf("want at least 2 checkpoints for a chain, got %d (S13/checkpoint-chain-validates)", len(cps))
		}

		key := engineKeyFromWorkspace(t, ws)
		objects := filepath.Join(ws, ".iris", "objects")

		// Each checkpoint: signature verifies against the engine key, and the digest
		// matches the exported partition bytes (offline-verifiable).
		for _, c := range cps {
			if !key.VerifyDigest(c.digest, c.signature) {
				t.Errorf("checkpoint seq %d signature does not verify against the engine key (S13/checkpoint-chain-validates)", c.seq)
			}
			_, archRows, rerr := archive.Read(filepath.Join(objects, fmt.Sprintf("%x", c.digest)))
			if rerr != nil {
				t.Errorf("read exported object for checkpoint seq %d: %v (S13/checkpoint-chain-validates)", c.seq, rerr)
				continue
			}
			if string(store.ComputeDigest(archRows)) != string(c.digest) {
				t.Errorf("checkpoint seq %d digest does not match its exported bytes (S13/checkpoint-chain-validates)", c.seq)
			}
		}

		// The chain links: the second checkpoint's parent is the first's digest, and
		// store.ValidateChain accepts the chain against the engine public key (the
		// offline auditor's check).
		if string(cps[1].parent) != string(cps[0].digest) {
			t.Errorf("checkpoint seq %d parent %x does not chain to prior digest %x (S13/checkpoint-chain-validates)", cps[1].seq, cps[1].parent, cps[0].digest)
		}
		chain := make([]store.CheckpointRow, 0, len(cps))
		for _, c := range cps {
			chain = append(chain, store.CheckpointRow{
				Seq: c.seq, IDFrom: c.idFrom, IDTo: c.idTo,
				Digest: c.digest, ParentDigest: c.parent, Signature: c.signature, Location: c.location,
			})
		}
		if err := store.ValidateChain(chain, key.Public()); err != nil {
			t.Errorf("checkpoint chain does not validate against the engine public key: %v (S13/checkpoint-chain-validates)", err)
		}
	})
}

// dataSourceForWorkspace returns a DSN targeting the data database for the
// workspace (managed or external), suitable for a plain pgx.Connect.
func dataSourceForWorkspace(t *testing.T, ws string) string {
	t.Helper()
	if ext := os.Getenv("IRIS_PG_DSN"); ext != "" {
		cfg, err := pgx.ParseConfig(ext)
		if err != nil {
			t.Fatalf("parse IRIS_PG_DSN: %v", err)
		}
		cfg.Database = pg.DataDatabase
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	}
	pgDir := filepath.Join(ws, ".iris", "pg")
	port := readPostmasterPort(t, filepath.Join(pgDir, "data", "postmaster.pid"))
	pwBytes, err := os.ReadFile(filepath.Join(pgDir, "superuser.pw")) //nolint:gosec
	if err != nil {
		t.Fatalf("read managed superuser pw: %v", err)
	}
	pw := strings.TrimSpace(string(pwBytes))
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		daemon.ManagedSuperuser, pw, port, pg.DataDatabase)
}

// adminDataDSN returns an admin (superuser) DSN for the data database of ws.
func adminDataDSN(t *testing.T, ws string) string {
	t.Helper()
	if ext := os.Getenv("IRIS_PG_DSN"); ext != "" {
		cfg, err := pgx.ParseConfig(ext)
		if err != nil {
			t.Fatalf("parse IRIS_PG_DSN: %v", err)
		}
		cfg.Database = pg.DataDatabase
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	}
	pgDir := filepath.Join(ws, ".iris", "pg")
	port := readPostmasterPort(t, filepath.Join(pgDir, "data", "postmaster.pid"))
	pwBytes, err := os.ReadFile(filepath.Join(pgDir, "superuser.pw")) //nolint:gosec
	if err != nil {
		t.Fatalf("read managed superuser pw: %v", err)
	}
	pw := strings.TrimSpace(string(pwBytes))
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		daemon.ManagedSuperuser, pw, port, pg.DataDatabase)
}
