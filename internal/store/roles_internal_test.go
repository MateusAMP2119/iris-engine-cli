package store

import (
	"context"
	"testing"
)

// This internal test proves the one place the raw credential secret is read: the
// credentials write bind. It lives in package store so it can construct a Secret
// with a known value (the unexported field) and confirm that value -- not the
// redacted marker -- is what SetCredential binds into the credentials row, while
// every formatting path still redacts. External tests cannot reach the unexported
// field, so this write-path proof belongs here.

// recordingConn is a minimal MetaWriteConn that captures the single statement and
// its args, standing in for the leader's meta connection with no live Postgres. It
// is package-local to avoid the store <- storetest import cycle an internal test
// would otherwise hit.
type recordingConn struct {
	sql  string
	args []any
}

func (c *recordingConn) Exec(_ context.Context, sql string, args ...any) error {
	c.sql = sql
	c.args = args
	return nil
}

// TestSecretRevealReachesTheBindOnly proves the raw secret crosses into the
// credentials write -- the one legitimate exit -- while never leaking through a
// formatting path.
//
// spec: S04/credentials-pipeline-login-only
func TestSecretRevealReachesTheBindOnly(t *testing.T) {
	t.Run("S04/credentials-pipeline-login-only", func(t *testing.T) {
		const raw = "9c1f-known-engine-secret"
		s := Secret{value: raw}

		// The write path binds the raw secret so it physically lands in the
		// credentials.secret column.
		conn := &recordingConn{}
		w := NewWriter(conn)
		if err := w.SetCredential(context.Background(), "iris_load_orders", PipelineOwner("load_orders"), s); err != nil {
			t.Fatalf("SetCredential: %v", err)
		}
		if len(conn.args) != 2 {
			t.Fatalf("SetCredential bound %d args, want 2 (pg_role, secret)", len(conn.args))
		}
		if conn.args[1] != raw {
			t.Errorf("credentials secret bind = %v, want the raw secret %q (never the redacted marker)", conn.args[1], raw)
		}

		// reveal is the sole reader of the raw value.
		if s.reveal() != raw {
			t.Errorf("reveal() = %q, want %q", s.reveal(), raw)
		}
	})
}
