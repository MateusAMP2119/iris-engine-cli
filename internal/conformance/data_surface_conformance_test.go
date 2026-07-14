//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file proves the data surface's physical enforcement contracts against a
// real Postgres: the engine's own read-pool login checks out shared-pool
// sessions, SET ROLEs to the data PAT's engine-managed
// NOLOGIN role, and Postgres itself -- not application code -- bounds every
// read to the role's granted fields. /data serves the granted projection and
// refuses the rest; /q on an endpoint whose source fields the caller lacks
// answers 403 forbidden naming the endpoint and never the missing fields. It
// drives the real read pool and the real api mux over a provisioned cluster
// (the role_enforcement pattern: the enforcement DDL under test, enforced by a
// live database, without the full CLI machinery around it).

// dataSurfaceEnvelope decodes the read-API envelope for these assertions.
type dataSurfaceEnvelope struct {
	Data []map[string]any `json:"data"`
	Page *struct {
		NextAfter any `json:"next_after"`
		Limit     int `json:"limit"`
	} `json:"page"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ordersTableSpec is the declared source both surfaces read.
func ordersTableSpec() map[string]*declare.Table {
	return map[string]*declare.Table{"analytics.orders": {
		Schema: "analytics",
		Table:  "orders",
		Columns: []declare.Column{
			{Name: "id", Type: "bigint", PrimaryKey: true},
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "numeric"},
			{Name: "status", Type: "text"},
		},
	}}
}

// compileDataEndpoint compiles one endpoint document against the orders source.
func compileDataEndpoint(t *testing.T, doc string) *declare.CompiledEndpoint {
	t.Helper()
	ep, err := declare.ParseEndpoint([]byte(doc))
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	ce, err := declare.CompileEndpoint(ep, ordersTableSpec())
	if err != nil {
		t.Fatalf("compile endpoint: %v", err)
	}
	return ce
}

// fixedEndpoints is an api.EndpointSource over a fixed compiled set.
type fixedEndpoints map[string]*declare.CompiledEndpoint

func (m fixedEndpoints) Endpoint(name string) (*declare.CompiledEndpoint, bool) {
	ce, ok := m[name]
	return ce, ok
}

// fixedShapes is an api.DataSource over a fixed shape set.
type fixedShapes map[string]*api.DataShape

func (m fixedShapes) DataShape(schema, table string) (*api.DataShape, bool) {
	s, ok := m[schema+"."+table]
	return s, ok
}

// TestDataSurfacePostgresEnforcement stands up a real cluster, provisions the
// engine's read-pool login role and one data-PAT NOLOGIN role granted only
// analytics.orders[id, amount], and serves the real api mux over the real
// shared read pool to prove:
//
//   - /data reads within the granted fields succeed and reads touching an
//     ungranted field are refused by Postgres itself (SQLSTATE 42501), surfaced
//     as 403.
//   - /q on an endpoint whose source fields the caller's role lacks answers 403
//     forbidden naming the endpoint, never the missing fields.
func TestDataSurfacePostgresEnforcement(t *testing.T) {
	const (
		superuser = "postgres"
		superpw   = "superpw"
		poolRole  = "iris_engine_read"
		patRole   = "iris_pat_r_bounded"
	)
	port := freePort(t)
	cluster := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V18).
		Username(superuser).Password(superpw).Database("postgres").
		Port(port).
		DataPath(filepath.Join(t.TempDir(), "data")).
		RuntimePath(filepath.Join(t.TempDir(), "runtime")).
		StartTimeout(90 * time.Second))
	if err := cluster.Start(); err != nil {
		t.Fatalf("start bare Postgres cluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminDSN := fmt.Sprintf("postgres://%s:%s@localhost:%d/postgres?sslmode=disable", superuser, superpw, port)
	client, err := pg.Connect(ctx, testConnSource{dsn: adminDSN})
	if err != nil {
		t.Fatalf("pg.Connect (data database): %v", err)
	}
	t.Cleanup(client.Close)

	// The declared table plus rows, including rows a journal would call
	// disposable -- the surface serves them like any other.
	secret, err := store.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS analytics`,
		`CREATE TABLE IF NOT EXISTS analytics.orders (
			id bigint PRIMARY KEY,
			customer_id uuid,
			amount numeric,
			status text
		)`,
		`INSERT INTO analytics.orders (id, customer_id, amount, status) VALUES
			(1, '3b241101-e2bb-4255-8caf-4136c566a962', 10.5, 'paid'),
			(2, '3b241101-e2bb-4255-8caf-4136c566a962', 20.0, 'open'),
			(3, 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 30.0, 'open')`,
		// The engine's read-pool login: the one identity the shared pool
		// connects as. It holds no table grants of its own here; reads run as
		// the SET ROLE'd PAT role.
		`CREATE ROLE ` + poolRole + ` LOGIN`,
		store.RenderSetRolePassword(poolRole, secret),
		`GRANT CONNECT ON DATABASE ` + pg.DataDatabase + ` TO ` + poolRole,
		// The data PAT's engine-managed read role: NOLOGIN, granted exactly
		// analytics.orders[id, amount] -- customer_id and status are NOT granted.
		`CREATE ROLE ` + patRole + ` NOLOGIN`,
		`GRANT CONNECT ON DATABASE ` + pg.DataDatabase + ` TO ` + patRole,
		`GRANT USAGE ON SCHEMA analytics TO ` + patRole,
		`GRANT SELECT (id, amount) ON analytics.orders TO ` + patRole,
		// SET ROLE requires membership: the pool login may assume the PAT role.
		`GRANT ` + patRole + ` TO ` + poolRole,
	} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision cluster (%.60s...): %v", stmt, err)
		}
	}

	// The shared read pool, connected as the engine's own login on the data
	// database (the production wiring: BuildReadPoolConn refuses meta).
	conn, err := store.BuildReadPoolConn(store.ScopedConnParams{
		Host: "localhost", Port: int(port), Database: pg.DataDatabase, Options: "sslmode=disable",
	}, poolRole, secret)
	if err != nil {
		t.Fatalf("BuildReadPoolConn: %v", err)
	}
	pgxPool, err := pgxpool.New(ctx, conn.EnvValue())
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pgxPool.Close)
	readPool := store.NewPgxReadPool(pgxPool)

	// The real serving surface: the api mux over the real pool, one compiled
	// endpoint inside the caller's grants and one outside them.
	granted := compileDataEndpoint(t, `endpoint: orders_granted
source: analytics.orders
fields: [id, amount]
filters:
  id: eq
sort: id
`)
	forbidden := compileDataEndpoint(t, `endpoint: orders_full
source: analytics.orders
fields: [id, customer_id, amount]
filters:
  customer_id: eq
sort: id
`)
	shape := &api.DataShape{
		Schema: "analytics",
		Table:  "orders",
		Columns: []api.ResponseColumn{
			{Name: "id", PgType: "bigint"},
			{Name: "customer_id", PgType: "uuid"},
			{Name: "amount", PgType: "numeric"},
			{Name: "status", PgType: "text"},
		},
		PrimaryKey: []string{"id"},
	}
	mux := api.NewMux(
		api.WithEndpoints(fixedEndpoints{granted.Name: granted, forbidden.Name: forbidden}),
		api.WithEndpointReader(api.NewPoolReader(readPool)),
		api.WithDataSource(fixedShapes{"analytics.orders": shape}),
		api.WithReadExecutor(readPool),
	)
	// Requests arrive as the bounded data PAT (the transport half -- bearer
	// token to authority -- is proven in the daemon's listener tests).
	authority := api.Authority{PATID: "t1", Scopes: []pat.Scope{pat.ScopeData}, DataRole: patRole}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(api.WithAuthority(r.Context(), authority)))
	}))
	defer srv.Close()

	get := func(t *testing.T, path string) (int, dataSurfaceEnvelope) {
		t.Helper()
		res, err := http.Get(srv.URL + path) //nolint:gosec,noctx // a test-server URL.
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close() //nolint:errcheck // a test read
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("read %s body: %v", path, err)
		}
		var env dataSurfaceEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decode %s envelope %q: %v", path, body, err)
		}
		return res.StatusCode, env
	}

	t.Run("data-pat-reads-physically-bounded", func(t *testing.T) {
		t.Run("a read within the granted fields succeeds as the PAT's role", func(t *testing.T) {
			code, env := get(t, "/data/analytics/orders?columns=id,amount")
			if code != http.StatusOK || env.Error != nil {
				t.Fatalf("granted /data read = %d %+v, want 200", code, env.Error)
			}
			if len(env.Data) != 3 {
				t.Fatalf("granted read served %d rows, want all 3", len(env.Data))
			}
			for i, wantID := range []float64{1, 2, 3} {
				if env.Data[i]["id"] != wantID {
					t.Errorf("row %d id = %v, want %v (keyset order by the PK)", i, env.Data[i]["id"], wantID)
				}
			}
		})

		t.Run("a read touching an ungranted field is refused, 403 on the surface", func(t *testing.T) {
			// The full default projection includes customer_id and status, which
			// the role was never granted.
			code, env := get(t, "/data/analytics/orders")
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("ungranted /data read = %d %+v, want 403 forbidden", code, env.Error)
			}
		})

		t.Run("an ungranted filter is refused even inside a granted projection", func(t *testing.T) {
			code, env := get(t, "/data/analytics/orders?columns=id,amount&status=open")
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("ungranted filter = %d %+v, want 403 forbidden", code, env.Error)
			}
		})

		t.Run("Postgres, not application code, is the refuser", func(t *testing.T) {
			// Drive the pool directly with the same role: the refusal carries
			// Postgres' own SQLSTATE 42501 (insufficient_privilege) under the
			// store's forbidden sentinel.
			_, err := readPool.ExecuteRead(ctx, patRole, "probe_ungranted",
				"SELECT status FROM analytics.orders ORDER BY id ASC LIMIT $1;", []any{1}, []string{"status"})
			if !errors.Is(err, store.ErrReadForbidden) {
				t.Fatalf("ungranted read error = %v, want store.ErrReadForbidden", err)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
				t.Fatalf("refusal does not carry SQLSTATE %s from Postgres: %v", insufficientPrivilege, err)
			}
			// And the same role reads its granted fields through the same pool:
			// bounded, not broken.
			rows, err := readPool.ExecuteRead(ctx, patRole, "probe_granted",
				"SELECT id, amount FROM analytics.orders ORDER BY id ASC LIMIT $1;", []any{10}, []string{"id", "amount"})
			if err != nil {
				t.Fatalf("granted read through the pool: %v", err)
			}
			if len(rows) != 3 || rows[0]["id"] != int64(1) {
				t.Errorf("granted rows = %v, want the three seeded rows", rows)
			}
		})
	})

	t.Run("q-forbidden-names-endpoint", func(t *testing.T) {
		t.Run("an endpoint inside the caller's grants serves rows as the caller's role", func(t *testing.T) {
			code, env := get(t, "/q/orders_granted")
			if code != http.StatusOK || env.Error != nil {
				t.Fatalf("granted /q read = %d %+v, want 200", code, env.Error)
			}
			if len(env.Data) != 3 {
				t.Errorf("granted endpoint served %d rows, want 3", len(env.Data))
			}
		})

		t.Run("an endpoint outside the caller's grants is 403 naming the endpoint, never the fields", func(t *testing.T) {
			code, env := get(t, "/q/orders_full")
			if code != http.StatusForbidden || env.Error == nil || env.Error.Code != "forbidden" {
				t.Fatalf("ungranted /q read = %d %+v, want 403 forbidden (never a 500)", code, env.Error)
			}
			if !strings.Contains(env.Error.Message, "orders_full") {
				t.Errorf("message %q does not name the endpoint", env.Error.Message)
			}
			for _, leak := range []string{"customer_id", "status", "permission denied"} {
				if strings.Contains(env.Error.Message, leak) {
					t.Errorf("message %q leaks %q; the 403 names the endpoint, never the missing fields", env.Error.Message, leak)
				}
			}
		})
	})
}
