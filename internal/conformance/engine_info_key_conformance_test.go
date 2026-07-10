//go:build conformance

package conformance

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
)

// TestEngineInfoShowsEngineKeyPublic drives the shipped binary and proves that
// `iris engine info` reads the engine key back from the engine_key meta table and
// exposes its PUBLIC half -- never the private half -- through both the --json data
// envelope and the human readout (specification sections 4 and 11). The production
// reader reaches meta the mode-appropriate way: the configured admin DSN in
// external mode, or the running managed instance's engine-owned runtime files
// (superuser credential + postmaster.pid port) in managed mode, never by starting a
// second postmaster. This pins the live wiring end to end against a real binary, a
// real daemon, and real Postgres, where the earlier tiers stop at fakes.
//
// spec: S04/engine-key-public-via-info
func TestEngineInfoShowsEngineKeyPublic(t *testing.T) {
	t.Run("S04/engine-key-public-via-info", func(t *testing.T) {
		bin := Build(t)
		ws := shortWorkspace(t)
		socket := filepath.Join(ws, ".iris", "iris.sock")

		// Install mints the engine key into meta; a detached daemon makes the managed
		// instance reachable (external mode is reachable regardless), so the reader can
		// read the key the production way and the daemon's role merges into the readout.
		bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
		bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d"}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
		t.Cleanup(func() {
			bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
		})

		readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := WaitForSocket(readyCtx, socket); err != nil {
			cancel()
			t.Fatalf("daemon socket never ready: %v", err)
		}
		cancel()
		if !waitForLeader(t, socket) {
			t.Fatal("daemon never became leader; cannot exercise the merged info readout")
		}

		// Read the key straight from meta so the test knows the expected public half and
		// the private bytes that must never reach an output stream.
		priv := readEngineKeyPrivate(t, ws)
		expectedKey, err := daemon.DecodeEngineKeyBytes(priv)
		if err != nil {
			t.Fatalf("decode engine key from meta: %v", err)
		}
		wantPublic := expectedKey.PublicBase64()
		privB64 := base64.StdEncoding.EncodeToString(priv)
		privHex := hex.EncodeToString(priv)

		// --json: the data envelope carries the public half, read the production way
		// from meta; it never carries private material. The daemon-held runtime fields
		// merge in through GET /info -- now that the info plane is wired into Run(), the
		// merged document also reports the elected leadership role and the display-only
		// uptime (specification section 15), so this asserts them here too.
		jres := bin.Run(t, RunOptions{Args: []string{"--json", "engine", "info"}, Dir: ws, Timeout: time.Minute})
		jres.RequireExit(t, 0)
		var doc struct {
			Data struct {
				EngineKeyPublic string `json:"engine_key_public"`
				Role            string `json:"role"`
				Uptime          string `json:"uptime"`
			} `json:"data"`
		}
		jres.DecodeJSON(t, &doc)
		if doc.Data.EngineKeyPublic != wantPublic {
			t.Errorf("engine info --json engine_key_public = %q, want %q (the public half of the key stored in meta)",
				doc.Data.EngineKeyPublic, wantPublic)
		}
		if s := string(jres.Stdout); strings.Contains(s, privB64) || strings.Contains(s, privHex) {
			t.Errorf("engine info --json leaked the private key half")
		}
		// The daemon-held runtime readout merges through GET /info: an unwired info
		// plane 500s and the CLI falls back to the daemonless local readout, leaving
		// role and uptime empty. Against a live leader they are populated.
		if doc.Data.Role != "leader" {
			t.Errorf("engine info --json role = %q, want leader (GET /info must merge the daemon's runtime readout)", doc.Data.Role)
		}
		if doc.Data.Uptime == "" {
			t.Error("engine info --json reports no uptime; the wired info plane always renders one")
		}

		// Human readout: shows the public half, never the private material, and -- with
		// the info plane wired -- names the leadership role instead of the not-reachable
		// fallback line.
		hres := bin.Run(t, RunOptions{Args: []string{"engine", "info"}, Dir: ws, Timeout: time.Minute})
		hres.RequireExit(t, 0)
		out := string(hres.Stdout)
		if !strings.Contains(out, wantPublic) {
			t.Errorf("human engine info did not show the public half %q:\n%s", wantPublic, out)
		}
		if both := out + string(hres.Stderr); strings.Contains(both, privB64) || strings.Contains(both, privHex) {
			t.Errorf("human engine info leaked the private key half")
		}
		if !strings.Contains(out, "role:") || strings.Contains(out, "not reachable") {
			t.Errorf("human engine info did not merge the daemon role (info plane unwired?):\n%s", out)
		}
	})
}

// readEngineKeyPrivate reads the raw engine-key private bytes straight from the
// engine_key meta table, so the test knows both the expected public half and the
// private material that must never appear on any output stream. It reaches meta
// independently of the daemon (external DSN or the running managed instance).
func readEngineKeyPrivate(t *testing.T, ws string) []byte {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, metaDSN(t, ws))
	if err != nil {
		t.Fatalf("connect meta to read engine key: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var priv []byte
	if err := conn.QueryRow(ctx, "SELECT private_key FROM engine_key WHERE id = 1").Scan(&priv); err != nil {
		t.Fatalf("read engine key private half from meta: %v", err)
	}
	return priv
}
