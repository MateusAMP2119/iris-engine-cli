package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// This file proves the map-shaped execution surface the data routes ride with the
// recording read-pool fakes -- integration tier, no live Postgres: ExecuteRead
// preserves the full SET ROLE / read-only transaction / RESET ROLE checkout cycle
// around the fixed prepared statement while scanning the served rows into
// column-keyed maps, ExecuteReadSelf runs the same cycle assuming no role (the
// ambient socket caller is the engine itself), and a Postgres privilege refusal
// (SQLSTATE 42501) surfaces as ErrReadForbidden so the route layer can answer 403
// without ever parsing Postgres error text.

// sliceRows is a poolRows fake serving scripted row values.
type sliceRows struct {
	data [][]any
	i    int
}

func (r *sliceRows) Next() bool {
	r.i++
	return r.i <= len(r.data)
}

func (r *sliceRows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan %d dests for %d values", len(dest), len(row))
	}
	for i := range dest {
		p, ok := dest[i].(*any)
		if !ok {
			return fmt.Errorf("dest %d is %T, want *any", i, dest[i])
		}
		*p = row[i]
	}
	return nil
}

func (r *sliceRows) Err() error { return nil }
func (r *sliceRows) Close()     {}

// rowsSession wraps the recording fake session with a scripted result set.
type rowsSession struct {
	*fakeReadSession
	data [][]any
}

func (s *rowsSession) queryPrepared(ctx context.Context, name string, args ...any) (poolRows, error) {
	if _, err := s.fakeReadSession.queryPrepared(ctx, name, args...); err != nil {
		return nil, err
	}
	return &sliceRows{data: s.data}, nil
}

// rowsAcquirer hands out one rowsSession.
type rowsAcquirer struct {
	sess *rowsSession
}

func (a *rowsAcquirer) acquire(context.Context) (readSession, error) { return a.sess, nil }

// TestExecuteReadMapsRowsUnderRoleCycle proves ExecuteRead is the same
// role-cycle read the pool always runs -- SET ROLE, one read-only statement,
// RESET ROLE on release -- returning the rows as column-keyed maps in served
// order (the /data and /q serving form).
func TestExecuteReadMapsRowsUnderRoleCycle(t *testing.T) {
	t.Run("read-pool-set-role-cycle", func(t *testing.T) {
		t.Run("runs the role cycle and scans rows into column maps", func(t *testing.T) {
			var script []string
			sess := &rowsSession{
				fakeReadSession: newFakeReadSession("s1", &script),
				data:            [][]any{{int64(1), "paid"}, {int64(2), "open"}},
			}
			pool := newReadPool(&rowsAcquirer{sess: sess})

			rows, err := pool.ExecuteRead(context.Background(), "iris_pat_r_alice", "data_analytics_orders_x",
				"SELECT id, status\nFROM analytics.orders\nORDER BY id ASC\nLIMIT $1;", []any{2}, []string{"id", "status"})
			if err != nil {
				t.Fatalf("ExecuteRead: %v", err)
			}
			want := []map[string]any{
				{"id": int64(1), "status": "paid"},
				{"id": int64(2), "status": "open"},
			}
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("rows = %v, want %v", rows, want)
			}
			wantScript := []string{
				`s1: SET ROLE "iris_pat_r_alice"`,
				"s1: BEGIN READ ONLY",
				"s1: prepare data_analytics_orders_x",
				"s1: query data_analytics_orders_x",
				"s1: COMMIT",
				"s1: RESET ROLE",
				"s1: release",
			}
			if !reflect.DeepEqual(script, wantScript) {
				t.Errorf("cycle = %v, want %v", script, wantScript)
			}
			if !reflect.DeepEqual(sess.lastArgs, []any{2}) {
				t.Errorf("bound args = %v, want the caller's bound params", sess.lastArgs)
			}
		})

		t.Run("refuses an empty role: no read ever runs unattributed", func(t *testing.T) {
			var script []string
			pool := newReadPool(&rowsAcquirer{sess: &rowsSession{fakeReadSession: newFakeReadSession("s1", &script)}})
			_, err := pool.ExecuteRead(context.Background(), "", "n", "SELECT 1", nil, []string{"c"})
			if !errors.Is(err, ErrInvalidRoleOwner) {
				t.Fatalf("ExecuteRead with empty role = %v, want ErrInvalidRoleOwner", err)
			}
			if len(script) != 0 {
				t.Errorf("a roleless read touched the pool: %v", script)
			}
		})

		t.Run("ExecuteReadSelf runs the same cycle assuming no role", func(t *testing.T) {
			var script []string
			sess := &rowsSession{
				fakeReadSession: newFakeReadSession("s1", &script),
				data:            [][]any{{int64(7)}},
			}
			pool := newReadPool(&rowsAcquirer{sess: sess})
			rows, err := pool.ExecuteReadSelf(context.Background(), "q_orders_x", "SELECT id FROM analytics.orders LIMIT $1;", []any{1}, []string{"id"})
			if err != nil {
				t.Fatalf("ExecuteReadSelf: %v", err)
			}
			if len(rows) != 1 || rows[0]["id"] != int64(7) {
				t.Errorf("rows = %v, want the scripted row", rows)
			}
			wantScript := []string{
				"s1: BEGIN READ ONLY",
				"s1: prepare q_orders_x",
				"s1: query q_orders_x",
				"s1: COMMIT",
				"s1: RESET ROLE",
				"s1: release",
			}
			if !reflect.DeepEqual(script, wantScript) {
				t.Errorf("self cycle = %v, want the role cycle minus SET ROLE", script)
			}
		})

		t.Run("a Postgres privilege refusal surfaces as ErrReadForbidden", func(t *testing.T) {
			var script []string
			sess := &rowsSession{fakeReadSession: newFakeReadSession("s1", &script)}
			sess.queryErr = &pgconn.PgError{Code: "42501", Message: "permission denied for table orders"}
			pool := newReadPool(&rowsAcquirer{sess: sess})
			_, err := pool.ExecuteRead(context.Background(), "iris_pat_r_alice", "n", "SELECT status FROM analytics.orders LIMIT $1;", []any{1}, []string{"status"})
			if !errors.Is(err, ErrReadForbidden) {
				t.Fatalf("privilege refusal = %v, want ErrReadForbidden", err)
			}
		})

		t.Run("any other failure is not a grant refusal", func(t *testing.T) {
			var script []string
			sess := &rowsSession{fakeReadSession: newFakeReadSession("s1", &script)}
			sess.queryErr = errors.New("connection reset")
			pool := newReadPool(&rowsAcquirer{sess: sess})
			_, err := pool.ExecuteRead(context.Background(), "iris_pat_r_alice", "n", "SELECT 1", nil, []string{"c"})
			if err == nil || errors.Is(err, ErrReadForbidden) {
				t.Fatalf("non-privilege failure = %v, want an error that is not ErrReadForbidden", err)
			}
		})
	})
}
