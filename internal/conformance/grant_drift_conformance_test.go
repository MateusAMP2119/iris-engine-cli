//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the grant-drift leg: a data PAT's role is drifted out-of-band on
// the data cluster -- its ledgered grant revoked, a stray column granted -- and
// a new leadership term reconciles it: the ledgered grant is re-issued (the
// ledger is authoritative), the stray is reported in the daemon log, and the
// stray is NEVER revoked (reported, not silently fixed).

func TestGrantDriftReconciledOnLeadership(t *testing.T) {
	freshDatabases(t)
	bin := Build(t)

	ws := shortWorkspace(t)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	setupWriterPipeline(t, ws, "wdrift", "drift", 9401, 9402)

	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	stopOnce := func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	}
	t.Cleanup(stopOnce)

	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("socket not ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("never leader")
	}
	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/wdrift"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	// Mint a data PAT with one ledgered field read: testdata.items.val.
	res := bin.Run(t, RunOptions{
		Args:    []string{"--json", "pat", "create", "--scope", "data", "--read", "testdata.items.val"},
		Dir:     ws,
		Timeout: time.Minute,
	})
	res.RequireExit(t, 0)
	var minted struct {
		Data struct {
			DataRole string `json:"data_role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Stdout, &minted); err != nil {
		t.Fatalf("decode pat create: %v\n%s", err, res.Stdout)
	}
	role := minted.Data.DataRole
	if role == "" {
		t.Fatalf("pat create surfaced no data role: %s", res.Stdout)
	}

	ctx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	data, err := pgx.Connect(ctx, dataDSN(t, ws))
	if err != nil {
		t.Fatalf("connect data db: %v", err)
	}
	defer func() { _ = data.Close(ctx) }()

	hasCol := func(col string) bool {
		var ok bool
		if err := data.QueryRow(ctx,
			`SELECT has_column_privilege($1, 'testdata.items'::regclass, $2, 'SELECT')`, role, col).Scan(&ok); err != nil {
			t.Fatalf("has_column_privilege(%s, %s): %v", role, col, err)
		}
		return ok
	}
	if !hasCol("val") {
		t.Fatal("the minted role does not hold its ledgered grant; the mint path is broken")
	}

	// Drift the role out-of-band: revoke the ledgered grant and grant a stray.
	if _, err := data.Exec(ctx, fmt.Sprintf(`REVOKE SELECT ("val") ON testdata.items FROM %s`, role)); err != nil {
		t.Fatalf("out-of-band revoke: %v", err)
	}
	if _, err := data.Exec(ctx, fmt.Sprintf(`GRANT SELECT ("id") ON testdata.items TO %s`, role)); err != nil {
		t.Fatalf("out-of-band stray grant: %v", err)
	}

	// A fresh leadership term reconciles: stop and start the engine.
	stopOnce()
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	readyCtx2, cancel3 := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx2, socket); err != nil {
		cancel3()
		t.Fatalf("socket not ready after restart: %v", err)
	}
	cancel3()
	if !waitForLeader(t, socket) {
		t.Fatal("never leader after restart")
	}

	t.Run("lost-ledgered-grant-reissued", func(t *testing.T) {
		if !hasCol("val") {
			t.Error("the ledgered grant was not re-issued on the new leadership term; the ledger must be authoritative")
		}
	})

	t.Run("stray-reported-never-revoked", func(t *testing.T) {
		if !hasCol("id") {
			t.Error("the stray grant was revoked; strays are reported, never silently fixed")
		}
		logBytes, err := os.ReadFile(filepath.Join(ws, ".iris", "logs", "daemon.log")) //nolint:gosec // G304: the daemon log under the test's own temp workspace.
		if err != nil {
			t.Fatalf("read daemon log: %v", err)
		}
		logText := string(logBytes)
		if !strings.Contains(logText, "STRAY") || !strings.Contains(logText, role) {
			t.Errorf("the stray grant was not reported in the daemon log for role %s:\n%s", role, tailOf(logText, 2000))
		}
	})
}

// tailOf returns at most n trailing bytes of s, for failure diagnostics.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
