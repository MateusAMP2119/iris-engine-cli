package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// This file proves the read-pool execution mechanics with recording fakes
// (integration tier, no live Postgres): the SET ROLE / RESET ROLE cycle per
// checkout around a single-statement read-only transaction, the
// session-scoped prepared statements that keep request handling from assembling
// SQL, the write-free read surface, and the data-database-only pool target that
// keeps engine storage unreachable.

// stubRows is a poolRows fake: an empty result set that records consumption and
// closing, so a test can prove the pool closes the cursor before it commits.
type stubRows struct {
	closed bool
}

func (r *stubRows) Next() bool        { return false }
func (r *stubRows) Scan(...any) error { return nil }
func (r *stubRows) Err() error        { return nil }
func (r *stubRows) Close()            { r.closed = true }

// fakeReadSession is a recording readSession: it appends every operation, in
// order, to the shared script, tracks its session-scoped prepared statements, and
// can be scripted to fail any step.
type fakeReadSession struct {
	name   string
	script *[]string

	// preparedStmts is the session's prepared-statement cache: name -> text.
	preparedStmts map[string]string
	// lastArgs are the bound args of the most recent queryPrepared call.
	lastArgs []any
	// rows is the cursor the most recent queryPrepared returned.
	rows *stubRows

	// execErr fails exec calls whose SQL text matches the key.
	execErr map[string]error
	// prepareErr fails the next prepare call.
	prepareErr error
	// queryErr fails the next queryPrepared call.
	queryErr error
	// broken records the broken flag of the release call.
	released bool
	broken   bool

	// prepares and queries count the respective calls.
	prepares int
	queries  int
}

func newFakeReadSession(name string, script *[]string) *fakeReadSession {
	return &fakeReadSession{name: name, script: script, preparedStmts: map[string]string{}}
}

func (s *fakeReadSession) record(op string) { *s.script = append(*s.script, s.name+": "+op) }

func (s *fakeReadSession) exec(_ context.Context, sql string) error {
	s.record(sql)
	if err := s.execErr[sql]; err != nil {
		return err
	}
	return nil
}

func (s *fakeReadSession) prepared(name string) bool {
	_, ok := s.preparedStmts[name]
	return ok
}

func (s *fakeReadSession) prepare(_ context.Context, name, text string) error {
	s.record("prepare " + name)
	s.prepares++
	if s.prepareErr != nil {
		return s.prepareErr
	}
	s.preparedStmts[name] = text
	return nil
}

func (s *fakeReadSession) queryPrepared(_ context.Context, name string, args ...any) (poolRows, error) {
	s.record("query " + name)
	s.queries++
	s.lastArgs = args
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	s.rows = &stubRows{}
	return s.rows, nil
}

func (s *fakeReadSession) release(broken bool) {
	if broken {
		s.record("release broken")
	} else {
		s.record("release")
	}
	s.released = true
	s.broken = broken
}

// fakeReadAcquirer is a recording readAcquirer that hands out its sessions in
// order, wrapping around, so a test controls whether two reads share one pooled
// session or draw fresh ones.
type fakeReadAcquirer struct {
	script   *[]string
	sessions []*fakeReadSession
	next     int
	err      error
}

func (a *fakeReadAcquirer) acquire(context.Context) (readSession, error) {
	if a.err != nil {
		return nil, a.err
	}
	s := a.sessions[a.next%len(a.sessions)]
	a.next++
	*a.script = append(*a.script, "acquire "+s.name)
	return s, nil
}

// newFakePool builds a ReadPool over n fake sessions sharing one script.
func newFakePool(n int) (*ReadPool, *fakeReadAcquirer, *[]string) {
	script := &[]string{}
	a := &fakeReadAcquirer{script: script}
	for i := 0; i < n; i++ {
		a.sessions = append(a.sessions, newFakeReadSession("conn"+string(rune('1'+i)), script))
	}
	return newReadPool(a), a, script
}

// mustReadStatement builds a ReadStatement or fails the test.
func mustReadStatement(t *testing.T, name, text string) ReadStatement {
	t.Helper()
	stmt, err := NewReadStatement(name, text)
	if err != nil {
		t.Fatalf("NewReadStatement(%q): %v", name, err)
	}
	return stmt
}

// drain consumes and discards every row.
func drain(rows ReadRows) error {
	for rows.Next() {
	}
	return rows.Err()
}

const qOrdersSQL = "SELECT id, customer_id FROM analytics.orders WHERE ($1::uuid IS NULL OR customer_id = $1::uuid) ORDER BY id ASC LIMIT $2;"

// TestReadPoolSetRoleCycle proves the per-checkout role cycle: each read checks a
// connection out of the shared pool on the data database, runs SET ROLE <pat_role>, executes a single-statement read-only
// transaction, and RESET ROLE on release -- on success, on failure, and for the
// /data statements that share the same mechanics.
func TestReadPoolSetRoleCycle(t *testing.T) {
	t.Run("read-pool-set-role-cycle", func(t *testing.T) {
		t.Run("a read runs SET ROLE, one read-only transaction, RESET ROLE, release", func(t *testing.T) {
			pool, a, script := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)

			if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
				t.Fatalf("Read: %v", err)
			}
			want := []string{
				"acquire conn1",
				`conn1: SET ROLE "iris_pat_7"`,
				"conn1: BEGIN READ ONLY",
				"conn1: prepare q_orders_by_customer",
				"conn1: query q_orders_by_customer",
				"conn1: COMMIT",
				"conn1: RESET ROLE",
				"conn1: release",
			}
			if got := *script; !equalScript(got, want) {
				t.Fatalf("cycle = %q, want %q", got, want)
			}
			if !a.sessions[0].rows.closed {
				t.Error("the read's row cursor was not closed before release")
			}
		})

		t.Run("RESET ROLE still runs when the read fails, after ROLLBACK", func(t *testing.T) {
			pool, a, script := newFakePool(1)
			boom := errors.New("permission denied for table orders")
			a.sessions[0].queryErr = boom
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)

			err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain)
			if !errors.Is(err, boom) {
				t.Fatalf("Read error = %v, want it to wrap %v", err, boom)
			}
			want := []string{
				"acquire conn1",
				`conn1: SET ROLE "iris_pat_7"`,
				"conn1: BEGIN READ ONLY",
				"conn1: prepare q_orders_by_customer",
				"conn1: query q_orders_by_customer",
				"conn1: ROLLBACK",
				"conn1: RESET ROLE",
				"conn1: release",
			}
			if got := *script; !equalScript(got, want) {
				t.Fatalf("failure cycle = %q, want %q", got, want)
			}
		})

		t.Run("a failed SET ROLE still resets and releases, and never opens a transaction", func(t *testing.T) {
			pool, a, script := newFakePool(1)
			boom := errors.New(`role "iris_pat_7" does not exist`)
			a.sessions[0].execErr = map[string]error{`SET ROLE "iris_pat_7"`: boom}
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)

			err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain)
			if !errors.Is(err, boom) {
				t.Fatalf("Read error = %v, want it to wrap %v", err, boom)
			}
			want := []string{
				"acquire conn1",
				`conn1: SET ROLE "iris_pat_7"`,
				"conn1: RESET ROLE",
				"conn1: release",
			}
			if got := *script; !equalScript(got, want) {
				t.Fatalf("set-role-failure cycle = %q, want %q", got, want)
			}
		})

		t.Run("a failed RESET ROLE marks the session broken so it never returns to the pool", func(t *testing.T) {
			pool, a, _ := newFakePool(1)
			boom := errors.New("connection reset by peer")
			a.sessions[0].execErr = map[string]error{"RESET ROLE": boom}
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)

			err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain)
			if !errors.Is(err, boom) {
				t.Fatalf("Read error = %v, want it to wrap %v", err, boom)
			}
			if !a.sessions[0].released || !a.sessions[0].broken {
				t.Errorf("released=%v broken=%v, want a broken release: a session that failed to RESET ROLE must be destroyed",
					a.sessions[0].released, a.sessions[0].broken)
			}
		})

		t.Run("each checkout of a shared session sets the caller's own role", func(t *testing.T) {
			pool, _, script := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)

			if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
				t.Fatalf("first Read: %v", err)
			}
			if err := pool.Read(context.Background(), "iris_pat_9", stmt, []any{nil, 100}, drain); err != nil {
				t.Fatalf("second Read: %v", err)
			}
			var roles []string
			for _, op := range *script {
				if strings.Contains(op, "SET ROLE") || strings.Contains(op, "RESET ROLE") {
					roles = append(roles, op)
				}
			}
			want := []string{
				`conn1: SET ROLE "iris_pat_7"`,
				"conn1: RESET ROLE",
				`conn1: SET ROLE "iris_pat_9"`,
				"conn1: RESET ROLE",
			}
			if !equalScript(roles, want) {
				t.Fatalf("role ops = %q, want %q", roles, want)
			}
		})

		t.Run("the same mechanics serve a /data statement", func(t *testing.T) {
			pool, _, script := newFakePool(1)
			stmt := mustReadStatement(t, "data_analytics_orders_ab12cd34",
				"SELECT id, status FROM analytics.orders WHERE ($1::text IS NULL OR status = $1::text) ORDER BY id ASC LIMIT $2;")

			if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{"shipped", 50}, drain); err != nil {
				t.Fatalf("Read: %v", err)
			}
			want := []string{
				"acquire conn1",
				`conn1: SET ROLE "iris_pat_7"`,
				"conn1: BEGIN READ ONLY",
				"conn1: prepare data_analytics_orders_ab12cd34",
				"conn1: query data_analytics_orders_ab12cd34",
				"conn1: COMMIT",
				"conn1: RESET ROLE",
				"conn1: release",
			}
			if got := *script; !equalScript(got, want) {
				t.Fatalf("/data cycle = %q, want %q", got, want)
			}
		})

		t.Run("an empty role is refused before any checkout", func(t *testing.T) {
			pool, _, script := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)
			if err := pool.Read(context.Background(), "", stmt, nil, drain); err == nil {
				t.Fatal("Read with an empty role succeeded, want an error")
			}
			if len(*script) != 0 {
				t.Errorf("empty-role read touched the pool: %q", *script)
			}
		})
	})
}

// TestRequestTimePreparedStatements proves the session-scoped prepared-statement
// behavior: request handling never assembles SQL --
// each pooled session prepares the fixed statement text on first use, later reads
// on that session execute the already-prepared statement, and request values ride
// as bound params only.
func TestRequestTimePreparedStatements(t *testing.T) {
	t.Run("request-time-prepared-statements", func(t *testing.T) {
		stmtText := qOrdersSQL

		t.Run("first use prepares the fixed text, later uses execute without re-preparing", func(t *testing.T) {
			pool, a, _ := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", stmtText)

			for i := 0; i < 3; i++ {
				if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
					t.Fatalf("Read %d: %v", i, err)
				}
			}
			s := a.sessions[0]
			if s.prepares != 1 {
				t.Errorf("session prepared %d times over 3 reads, want exactly 1 (session-scoped, first use)", s.prepares)
			}
			if s.queries != 3 {
				t.Errorf("session executed %d queries, want 3", s.queries)
			}
			if got := s.preparedStmts["q_orders_by_customer"]; got != stmtText {
				t.Errorf("prepared text = %q, want the fixed statement text %q", got, stmtText)
			}
		})

		t.Run("each session prepares independently", func(t *testing.T) {
			pool, a, _ := newFakePool(2)
			stmt := mustReadStatement(t, "q_orders_by_customer", stmtText)

			for i := 0; i < 2; i++ {
				if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
					t.Fatalf("Read %d: %v", i, err)
				}
			}
			for _, s := range a.sessions {
				if s.prepares != 1 {
					t.Errorf("session %s prepared %d times, want 1: prepared statements are session-scoped", s.name, s.prepares)
				}
			}
		})

		t.Run("distinct statements each prepare once on one session", func(t *testing.T) {
			pool, a, _ := newFakePool(1)
			q := mustReadStatement(t, "q_orders_by_customer", stmtText)
			d := mustReadStatement(t, "data_analytics_orders_ab12cd34",
				"SELECT id FROM analytics.orders WHERE ($1::bigint IS NULL OR id > $1::bigint) ORDER BY id ASC LIMIT $2;")

			for _, stmt := range []ReadStatement{q, d, q, d} {
				if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
					t.Fatalf("Read %s: %v", stmt.Name(), err)
				}
			}
			s := a.sessions[0]
			if s.prepares != 2 {
				t.Errorf("session prepared %d times for 2 distinct statements over 4 reads, want 2", s.prepares)
			}
		})

		t.Run("request values ride as bound args, never into the statement text", func(t *testing.T) {
			pool, a, _ := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", stmtText)
			hostile := "x'; DROP TABLE meta.pats; --"

			if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{hostile, 100}, drain); err != nil {
				t.Fatalf("Read: %v", err)
			}
			s := a.sessions[0]
			if len(s.lastArgs) != 2 || s.lastArgs[0] != hostile {
				t.Errorf("bound args = %v, want the request value bound as-is", s.lastArgs)
			}
			if strings.Contains(s.preparedStmts["q_orders_by_customer"], hostile) {
				t.Error("a request value leaked into the prepared statement text")
			}
		})
	})
}

// TestReadSurfaceNoWrites proves the write-free read surface: the pool refuses
// any statement that is not a single SELECT (the
// journal stays readable, never writable; no DML or DDL can enter), and every
// accepted read runs as exactly one statement inside a read-only transaction.
func TestReadSurfaceNoWrites(t *testing.T) {
	t.Run("read-surface-no-writes", func(t *testing.T) {
		t.Run("only a single SELECT is accepted as a read statement", func(t *testing.T) {
			rejected := []struct{ name, text string }{
				{"journal-insert", "INSERT INTO iris.journal (run_id) VALUES ($1)"},
				{"journal-update", "UPDATE iris.journal SET run_id = $1"},
				{"journal-delete", "DELETE FROM iris.journal WHERE run_id = $1"},
				{"ddl-drop", "DROP TABLE analytics.orders"},
				{"ddl-create", "CREATE TABLE public.x (id bigint)"},
				{"ddl-alter", "ALTER TABLE analytics.orders ADD COLUMN x bigint"},
				{"truncate", "TRUNCATE analytics.orders"},
				{"grant", "GRANT SELECT ON analytics.orders TO PUBLIC"},
				{"multi-statement", "SELECT 1; DELETE FROM analytics.orders"},
				{"piggybacked-write", "SELECT id FROM analytics.orders; DROP TABLE analytics.orders;"},
				{"empty", ""},
			}
			for _, tc := range rejected {
				if _, err := NewReadStatement("stmt_x", tc.text); err == nil {
					t.Errorf("%s: NewReadStatement accepted %q, want a refusal: no mutation path exists on the read surface", tc.name, tc.text)
				}
			}

			accepted := []string{
				qOrdersSQL,
				"select id from analytics.orders order by id asc limit $1",
				"\n  SELECT run_id FROM iris.journal ORDER BY id ASC LIMIT $1;",
			}
			for _, text := range accepted {
				if _, err := NewReadStatement("stmt_x", text); err != nil {
					t.Errorf("NewReadStatement rejected the single SELECT %q: %v", text, err)
				}
			}
		})

		t.Run("every read is exactly one statement inside a read-only transaction", func(t *testing.T) {
			pool, _, script := newFakePool(1)
			stmt := mustReadStatement(t, "q_orders_by_customer", qOrdersSQL)
			if err := pool.Read(context.Background(), "iris_pat_7", stmt, []any{nil, 100}, drain); err != nil {
				t.Fatalf("Read: %v", err)
			}

			ops := *script
			begin, commit := indexOf(ops, "conn1: BEGIN READ ONLY"), indexOf(ops, "conn1: COMMIT")
			if begin < 0 || commit < 0 || commit < begin {
				t.Fatalf("read ran outside BEGIN READ ONLY ... COMMIT: %q", ops)
			}
			var queries int
			for _, op := range ops[begin+1 : commit] {
				if strings.HasPrefix(op, "conn1: query ") {
					queries++
				} else if !strings.HasPrefix(op, "conn1: prepare ") {
					t.Errorf("unexpected operation inside the read-only transaction: %q", op)
				}
			}
			if queries != 1 {
				t.Errorf("%d statements executed inside the transaction, want exactly 1", queries)
			}
		})
	})
}

// TestEngineStorageUnreachable proves the database split: the shared read pool
// serves the data surface on the data database only --
// its connection can never target the meta database, so engine storage is
// unreachable through /data and /q.
func TestEngineStorageUnreachable(t *testing.T) {
	t.Run("engine-storage-unreachable", func(t *testing.T) {
		secret, err := GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}

		t.Run("the read pool refuses to target the meta database", func(t *testing.T) {
			_, err := BuildReadPoolConn(ScopedConnParams{
				Host: "localhost", Port: 5432, Database: MetaDatabase,
			}, "iris_engine", secret)
			if !errors.Is(err, ErrReadPoolMetaDatabase) {
				t.Fatalf("BuildReadPoolConn(meta) error = %v, want ErrReadPoolMetaDatabase", err)
			}
		})

		t.Run("the read pool targets the given data database", func(t *testing.T) {
			conn, err := BuildReadPoolConn(ScopedConnParams{
				Host: "localhost", Port: 5432, Database: "data",
			}, "iris_engine", secret)
			if err != nil {
				t.Fatalf("BuildReadPoolConn(data): %v", err)
			}
			dsn := conn.EnvValue()
			if !strings.HasSuffix(dsn, "/data") {
				t.Errorf("read-pool DSN targets %q, want the data database (dsn ends /data)", dsn)
			}
			if strings.Contains(dsn, "/"+MetaDatabase) {
				t.Errorf("read-pool DSN %q addresses the meta database", "ScopedConn(REDACTED)")
			}
		})
	})
}

// equalScript reports whether two recorded op sequences match exactly.
func equalScript(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// indexOf returns the index of the first exact match of op in ops, or -1.
func indexOf(ops []string, op string) int {
	for i, o := range ops {
		if o == op {
			return i
		}
	}
	return -1
}
