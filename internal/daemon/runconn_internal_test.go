package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file proves the run-connection builder: a run whose pipeline role holds a
// persisted credential connects as that role (never as the admin identity), the
// run id rides the scoped DSN, and a pipeline with no credential falls back to
// the admin-derived DSN so an upgrade never strands it.

// credsFake scripts the role-credential read: the named roles hold a freshly
// minted secret, the rest none.
type credsFake struct{ secrets map[string]store.Secret }

func newCredsFake(t *testing.T, roles ...string) credsFake {
	t.Helper()
	f := credsFake{secrets: map[string]store.Secret{}}
	for _, r := range roles {
		s, err := store.GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		f.secrets[r] = s
	}
	return f
}

func (f credsFake) RoleSecret(_ context.Context, pgRole string) (store.Secret, bool, error) {
	s, ok := f.secrets[pgRole]
	return s, ok, nil
}

func TestRunConnBuilderScopedIdentity(t *testing.T) {
	t.Run("run-conn-scoped-identity", func(t *testing.T) {
		ctx := context.Background()
		const adminDSN = "postgres://iris_admin:adminpw@dbhost:5433/data?sslmode=disable" //nolint:gosec // G101: test-only fake DSN

		t.Run("a provisioned pipeline connects as its own role, never the admin", func(t *testing.T) {
			b, err := newRunConnBuilder(adminDSN, newCredsFake(t, "iris_pipeline_load"), nil)
			if err != nil {
				t.Fatalf("newRunConnBuilder: %v", err)
			}
			dsn := b.dsnFor(ctx, "load", 42)
			if !strings.Contains(dsn, "iris_pipeline_load:") || !strings.Contains(dsn, "@dbhost:5433/data") {
				t.Errorf("scoped DSN = %q, want the pipeline role's identity on the admin DSN's host/db", dsn)
			}
			if strings.Contains(dsn, "iris_admin") || strings.Contains(dsn, "adminpw") {
				t.Errorf("scoped DSN %q leaks the admin identity", dsn)
			}
			if !strings.Contains(dsn, "iris.run_id") {
				t.Errorf("scoped DSN %q does not carry the iris.run_id setting", dsn)
			}
			if !strings.Contains(dsn, "sslmode=disable") {
				t.Errorf("scoped DSN %q dropped the base DSN's options", dsn)
			}
		})

		t.Run("a pipeline with no persisted credential falls back to the admin DSN", func(t *testing.T) {
			b, err := newRunConnBuilder(adminDSN, credsFake{}, nil)
			if err != nil {
				t.Fatalf("newRunConnBuilder: %v", err)
			}
			dsn := b.dsnFor(ctx, "legacy", 7)
			if !strings.HasPrefix(dsn, adminDSN) {
				t.Errorf("fallback DSN = %q, want the admin-derived base %q with the run id appended", dsn, adminDSN)
			}
			if !strings.Contains(dsn, "iris.run_id") {
				t.Errorf("fallback DSN %q does not carry the iris.run_id setting", dsn)
			}
		})

		t.Run("a nil builder yields an empty connection", func(t *testing.T) {
			var b *runConnBuilder
			if got := b.dsnFor(ctx, "p", 1); got != "" {
				t.Errorf("nil builder returned %q, want empty", got)
			}
		})
	})
}
