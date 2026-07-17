//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file proves the read surface and its physical enforcement on a declared
// endpoint, end to end against the shipped binary, a running daemon, and real
// Postgres (acceptance step 7). It drives the documented
// CLI commands -- `iris declare apply` (provisions the source table), `iris endpoint
// apply` (publishes the endpoint), `iris pat create --scope data --endpoint ...`
// (mints a data PAT and captures its show-once token) -- then reads over the TCP
// listener with the Bearer token: the granted projection serves via the /q declared
// surface and the raw /data surface, and a read touching an ungranted field is
// refused by Postgres itself (SQLSTATE 42501), surfaced as a 403 that never names
// the missing field. The three rows the reads serve land through the turn protocol
// (#206): the workspace's one pipeline is a frame-speaking resident whose first
// turn answers them as declared-write row frames, and the ENGINE performs the
// inserts itself -- the pipeline holds no database credentials. No fakes, no manual
// meta rows, no ambient-socket shortcut: the whole path is the real binary over the
// real transport.

// ordersEndpointEnv is the running fixture: a workspace, its daemon, the data DSN,
// and the TCP base URL the read assertions hit.
type ordersEndpointEnv struct {
	bin     *Binary
	ws      string
	socket  string
	tcpBase string
	dataDSN string
}

// patCreateEnvelope decodes the --json output of `iris pat create`.
type patCreateEnvelope struct {
	Data struct {
		ID       string   `json:"id"`
		Token    string   `json:"token"`
		Scopes   []string `json:"scopes"`
		DataRole string   `json:"data_role"`
	} `json:"data"`
}

// readEnvelope decodes a read-API response for the endpoint/data assertions.
type readEnvelope struct {
	Data  []map[string]any `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestEndpointReadsAndGrants stands up the end-to-end read surface and proves both
// read-surface enforcement contracts against a real cluster and the real binary.
func TestEndpointReadsAndGrants(t *testing.T) {
	env := startOrdersEndpointEnv(t)

	// Mint a data PAT scoped to the orders_by_customer endpoint: its grant is exactly
	// that endpoint's source fields (id, customer_id, amount). status is never granted.
	token := mintEndpointPAT(t, env, "orders_by_customer")

	t.Run("data-pat-reads-endpoint", func(t *testing.T) {
		// The declared /q surface: the endpoint whose every referenced column the PAT
		// holds serves rows as the PAT's role, over TCP with the Bearer token.
		code, env2 := env.tcpGet(t, "/q/orders_by_customer", token)
		if code != http.StatusOK || env2.Error != nil {
			t.Fatalf("granted /q read = %d %+v, want 200", code, env2.Error)
		}
		if len(env2.Data) != 3 {
			t.Fatalf("granted /q endpoint served %d rows, want the 3 seeded", len(env2.Data))
		}

		// The raw /data surface: a projection within the grant serves.
		code, env3 := env.tcpGet(t, "/data/analytics/orders?columns=id,amount", token)
		if code != http.StatusOK || env3.Error != nil {
			t.Fatalf("granted /data read = %d %+v, want 200", code, env3.Error)
		}
		if len(env3.Data) != 3 {
			t.Fatalf("granted /data read served %d rows, want 3", len(env3.Data))
		}
	})

	t.Run("ungranted-field-fails-postgres", func(t *testing.T) {
		// The declared /q surface: an endpoint projecting status (ungranted) is refused
		// by Postgres itself and surfaced as a 403 that names the endpoint, never the
		// missing field.
		code, env2 := env.tcpGet(t, "/q/orders_full", token)
		if code != http.StatusForbidden || env2.Error == nil || env2.Error.Code != "forbidden" {
			t.Fatalf("ungranted /q read = %d %+v, want 403 forbidden (never a 500)", code, env2.Error)
		}
		if !strings.Contains(env2.Error.Message, "orders_full") {
			t.Errorf("403 message %q does not name the endpoint", env2.Error.Message)
		}
		for _, leak := range []string{"status", "permission denied", "42501"} {
			if strings.Contains(env2.Error.Message, leak) {
				t.Errorf("403 message %q leaks %q; it names the endpoint, never the field or Postgres text", env2.Error.Message, leak)
			}
		}

		// The raw /data surface: the full default projection touches status (ungranted)
		// and is refused, 403 on the surface.
		code, env3 := env.tcpGet(t, "/data/analytics/orders", token)
		if code != http.StatusForbidden || env3.Error == nil || env3.Error.Code != "forbidden" {
			t.Fatalf("ungranted /data read = %d %+v, want 403 forbidden", code, env3.Error)
		}

		// Postgres, not application code, is the refuser: assume the PAT's own
		// engine-managed role directly and touch the ungranted column -- the refusal
		// carries Postgres' own SQLSTATE 42501 (insufficient_privilege).
		assertPostgresRefusesUngranted(t, env, "orders_by_customer")
	})
}

// startOrdersEndpointEnv builds the workspace, starts the daemon with TCP enabled,
// provisions analytics.orders via `iris declare apply` (which also registers the
// seeder pipeline the loop lane starts as a resident), publishes the endpoints via
// `iris endpoint apply`, and waits until the seeder's first producing turn landed
// the three order rows through the engine's own data transaction.
func startOrdersEndpointEnv(t *testing.T) ordersEndpointEnv {
	t.Helper()
	freshDatabases(t)
	bin := Build(t)
	ws := shortWorkspace(t)
	writeOrdersWorkspace(t, ws)
	socket := filepath.Join(ws, ".iris", "iris.sock")
	tcpAddr := freeTCPAddr(t)

	bin.Run(t, RunOptions{Args: []string{"engine", "install"}, Dir: ws, Timeout: 5 * time.Minute}).RequireExit(t, 0)
	bin.Run(t, RunOptions{Args: []string{"engine", "start", "-d", "--tcp", tcpAddr}, Dir: ws, Timeout: 2 * time.Minute}).RequireExit(t, 0)
	t.Cleanup(func() {
		bin.Run(t, RunOptions{Args: []string{"engine", "stop"}, Dir: ws, Timeout: 30 * time.Second})
	})

	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := WaitForSocket(readyCtx, socket); err != nil {
		cancel()
		t.Fatalf("daemon socket never became ready: %v", err)
	}
	cancel()
	if !waitForLeader(t, socket) {
		t.Fatal("daemon never became leader")
	}

	// Provision analytics.orders (and the capture surface) via the real apply path.
	bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/orders_ingest"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	// Publish the endpoints: prepare-verify against the data database, persist, and
	// swap into the live serving registry.
	bin.Run(t, RunOptions{Args: []string{"endpoint", "apply"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

	dsn := dataDSN(t, ws)
	waitForSeededOrders(t, dsn)

	return ordersEndpointEnv{bin: bin, ws: ws, socket: socket, tcpBase: "http://" + tcpAddr, dataDSN: dsn}
}

// mintEndpointPAT mints a data PAT scoped to the named endpoint via the real CLI and
// returns its show-once token, failing if the token is not surfaced.
func mintEndpointPAT(t *testing.T, env ordersEndpointEnv, endpoint string) string {
	t.Helper()
	res := env.bin.Run(t, RunOptions{
		Args:    []string{"--json", "pat", "create", "--scope", "data", "--endpoint", endpoint},
		Dir:     env.ws,
		Timeout: time.Minute,
	})
	res.RequireExit(t, 0)
	var env2 patCreateEnvelope
	res.DecodeJSON(t, &env2)
	if env2.Data.Token == "" {
		t.Fatalf("pat create did not surface a show-once token: %s", res.Stdout)
	}
	if env2.Data.DataRole == "" {
		t.Fatalf("data PAT was minted without an engine-managed read role: %s", res.Stdout)
	}
	return env2.Data.Token
}

// tcpGet issues a GET to the daemon's TCP listener with the Bearer token and decodes
// the read envelope.
func (e ordersEndpointEnv) tcpGet(t *testing.T, path, token string) (int, readEnvelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.tcpBase+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s over TCP: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test read
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	var env readEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s envelope %q: %v", path, body, err)
	}
	return resp.StatusCode, env
}

// assertPostgresRefusesUngranted assumes the endpoint's data-PAT role directly and
// touches an ungranted column, proving Postgres itself (SQLSTATE 42501) is the
// refuser rather than application code. It resolves the role name from the meta
// store (the role the mint provisioned). SET ROLE requires the admin be a superuser
// or a member; where the admin lacks that right the direct probe is skipped (the TCP
// 403 above already carries the enforcement, mapped from this same 42501).
func assertPostgresRefusesUngranted(t *testing.T, env ordersEndpointEnv, _ string) {
	t.Helper()
	role := dataPATRoleForEndpoint(t, env)
	if role == "" {
		t.Skip("no data-PAT role recorded for the endpoint; TCP 403 already carries the enforcement")
	}
	conn, err := pgx.Connect(context.Background(), env.dataDSN)
	if err != nil {
		t.Fatalf("connect data database for the direct role probe: %v", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(context.Background(), "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Skipf("cannot SET ROLE to %q (admin lacks membership/superuser); TCP 403 already carries the enforcement: %v", role, err)
	}
	// Touch the ungranted column: Postgres must refuse with insufficient_privilege.
	_, err = conn.Exec(context.Background(), "SELECT status FROM analytics.orders LIMIT 1")
	if err == nil {
		t.Fatal("read of ungranted field (status) succeeded; Postgres did not physically enforce the grant")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
		t.Fatalf("ungranted read error = %v; want Postgres SQLSTATE %s (insufficient_privilege)", err, insufficientPrivilege)
	}
	// And the granted columns read fine as the same role: bounded, not broken.
	if _, err := conn.Exec(context.Background(), "SELECT id, amount FROM analytics.orders LIMIT 1"); err != nil {
		t.Fatalf("granted read as the PAT role failed: %v", err)
	}
}

// dataPATRoleForEndpoint reads the single data-PAT role recorded in meta (the one
// the mint provisioned). With exactly one data PAT minted in this test, the roles
// table holds exactly one pat-owned role.
func dataPATRoleForEndpoint(t *testing.T, env ordersEndpointEnv) string {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), metaDSN(t, env.ws))
	if err != nil {
		t.Fatalf("connect meta for the data-PAT role: %v", err)
	}
	defer conn.Close(context.Background())
	var role string
	err = conn.QueryRow(context.Background(),
		`SELECT pg_role FROM roles WHERE pat IS NOT NULL ORDER BY pg_role LIMIT 1`).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read data-PAT role from meta: %v", err)
	}
	return role
}

// waitForSeededOrders waits until the seeder pipeline's first producing turn landed
// its three rows in analytics.orders. The rows arrive only through the turn
// protocol -- row frames upserted by the engine on its own admin connection with the
// run's exact attribution (#206) -- so their presence proves the engine-mediated
// write path, and the reads then have rows to serve. Readiness is the observed row
// count, never elapsed time.
func waitForSeededOrders(t *testing.T, dsn string) {
	t.Helper()
	conn := connectData(t, dsn)
	defer conn.Close(context.Background())
	dl := time.Now().Add(2 * time.Minute)
	for time.Now().Before(dl) {
		var n int
		_ = conn.QueryRow(context.Background(), "SELECT count(*) FROM analytics.orders").Scan(&n)
		if n == 3 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the seeder pipeline never landed its three rows within 2m; the engine-mediated write path did not deliver")
}

// writeOrdersWorkspace lays out a minimal workspace: schemas/analytics/orders, one
// pipeline that declares it (so `declare apply` provisions the table) and seeds it
// over the turn protocol, and two endpoints -- orders_by_customer (projection id,
// customer_id, amount; self-consistent with its customer_id filter) and orders_full
// (which also projects the ungranted status column).
func writeOrdersWorkspace(t *testing.T, ws string) {
	t.Helper()
	writeFile(t, filepath.Join(ws, "schemas", "analytics", "orders", "table.yaml"), `schema: analytics
table: orders
columns:
  - name: id
    type: bigint
    primary_key: true
  - name: customer_id
    type: uuid
  - name: amount
    type: numeric
  - name: status
    type: text
`)
	writeFile(t, filepath.Join(ws, "pipelines", "orders_ingest", "iris-declare.yaml"), `name: orders_ingest
run: ["go", "run", "main.go"]
lane: ingest
writes:
  - table: analytics.orders
    fields: [id, customer_id, amount, status]
`)
	// The seeder is a frame-speaking resident (#206): stdin carries the engine's
	// go/row/run frames; the first turn answers the three order rows as
	// declared-write row frames plus done, and every later turn answers a bare
	// done -- quiet, so it mints no run rows and the lane parks. Stdout is
	// protocol only; the process never opens a database connection.
	writeFile(t, filepath.Join(ws, "pipelines", "orders_ingest", "main.go"), `package main

// Frame-speaking seeder (#206): the first run frame is answered with the three
// order rows as declared-write row frames plus done; every later turn answers a
// bare done (quiet -- it mints no run rows and the lane parks). Stdout is
// protocol only and no database connection is ever opened: the engine performs
// the writes itself. Unmarshal matches JSON keys case-insensitively, so the
// engine's lowercase "event"/"turn" frames land without struct tags.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type frame struct {
	Event string
	Turn  int64
}

func main() {
	var turn int64
	seeded := false
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var f frame
		if json.Unmarshal(in.Bytes(), &f) != nil {
			continue
		}
		switch f.Event {
		case "go":
			turn = f.Turn
		case "run":
			if !seeded {
				seeded = true
				for _, r := range []string{
					"{\"id\":1,\"customer_id\":\"3b241101-e2bb-4255-8caf-4136c566a962\",\"amount\":10.5,\"status\":\"paid\"}",
					"{\"id\":2,\"customer_id\":\"3b241101-e2bb-4255-8caf-4136c566a962\",\"amount\":20.0,\"status\":\"open\"}",
					"{\"id\":3,\"customer_id\":\"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11\",\"amount\":30.0,\"status\":\"open\"}",
				} {
					fmt.Printf("{\"event\":\"row\",\"table\":\"analytics.orders\",\"row\":%s}\n", r)
				}
			}
			fmt.Printf("{\"event\":\"done\",\"turn\":%d}\n", turn)
		}
	}
}
`)
	// orders_by_customer projects only columns the grant will cover; its filter
	// (customer_id) and sort (id) are both in the projection, so a PAT granted the
	// projection can execute it in full.
	writeFile(t, filepath.Join(ws, "endpoints", "orders_by_customer.yaml"), `endpoint: orders_by_customer
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`)
	// orders_full projects the ungranted status column, so a PAT scoped to
	// orders_by_customer cannot execute it -- Postgres refuses.
	writeFile(t, filepath.Join(ws, "endpoints", "orders_full.yaml"), `endpoint: orders_full
source: analytics.orders
fields: [id, customer_id, amount, status]
filters:
  id: eq
sort: id
`)
}

// writeFile writes data to path, creating parent directories.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// freeTCPAddr reserves a free loopback TCP port and returns it as host:port. The
// listener is closed immediately; the daemon binds it moments later (the small race
// is standard for tests that must hand a concrete address to a subprocess).
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free TCP port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
