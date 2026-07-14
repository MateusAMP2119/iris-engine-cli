package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeReadPoolCredMeta is an in-memory create-once single-row table: the first
// mint stores its candidate, every later mint returns that stored secret
// unchanged -- exactly the INSERT ... ON CONFLICT DO NOTHING + read-back the live
// seam runs, with no live Postgres.
type fakeReadPoolCredMeta struct {
	stored      string
	tableEnsure int
	mints       int
	ensureErr   error
	mintErr     error
}

func (f *fakeReadPoolCredMeta) ensureReadPoolCredentialTable(context.Context) error {
	f.tableEnsure++
	return f.ensureErr
}

func (f *fakeReadPoolCredMeta) mintReadPoolCredential(_ context.Context, candidate string) (string, error) {
	if f.mintErr != nil {
		return "", f.mintErr
	}
	f.mints++
	if f.stored == "" {
		f.stored = candidate // this call won the create-once race.
	}
	return f.stored, nil // always the persisted secret, candidate or the prior winner.
}

// TestEnsureReadPoolCredentialPersistsCreateOnce proves the read-pool login secret is
// persisted create-once in engine-owned meta and reused across daemon starts: a
// second start reads the stored secret back rather than minting a fresh one, so an
// earlier node's pool credential is never invalidated by a later start (the latent
// multi-node gap left by minting the pool secret fresh on every daemon start). The
// table is ensured create-if-missing first (bootstrap).
func TestEnsureReadPoolCredentialPersistsCreateOnce(t *testing.T) {
	ctx := context.Background()
	meta := &fakeReadPoolCredMeta{}

	// First daemon start: mints and persists a fresh secret.
	s1, err := ensureReadPoolCredential(ctx, meta, GenerateSecret)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if s1.IsZero() {
		t.Fatal("first start produced a zero read-pool secret")
	}
	if meta.tableEnsure != 1 {
		t.Errorf("table ensured %d times on first start, want 1 (bootstrap create-if-missing)", meta.tableEnsure)
	}

	// Second daemon start (a restart, or an HA standby against the same cluster): it
	// generates its OWN fresh candidate, but the create-once store returns the first
	// start's persisted secret, so both nodes share one credential.
	s2, err := ensureReadPoolCredential(ctx, meta, GenerateSecret)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if s2.reveal() != s1.reveal() {
		t.Fatalf("second start reused a different secret; a later start must not re-mint and reset the shared login's password (last-starter-wins bug)")
	}
	if meta.stored != s1.reveal() {
		t.Errorf("persisted secret drifted from the first start's secret")
	}
}

// TestEnsureReadPoolCredentialSurfacesErrors proves the table-ensure and mint errors
// are wrapped and returned (no swallowed error, no zero-secret returned on failure).
func TestEnsureReadPoolCredentialSurfacesErrors(t *testing.T) {
	ctx := context.Background()

	sentinel := errors.New("boom")
	if _, err := ensureReadPoolCredential(ctx, &fakeReadPoolCredMeta{ensureErr: sentinel}, GenerateSecret); !errors.Is(err, sentinel) {
		t.Errorf("ensure-table error not surfaced: %v", err)
	}
	if _, err := ensureReadPoolCredential(ctx, &fakeReadPoolCredMeta{mintErr: sentinel}, GenerateSecret); !errors.Is(err, sentinel) {
		t.Errorf("mint error not surfaced: %v", err)
	}
}

// TestReadPoolCredentialTableDDLFromModel proves the bootstrap create-if-missing DDL
// is rendered from the meta schema model (the single source of truth), so it can
// never drift from the roster and golden-DDL contracts.
func TestReadPoolCredentialTableDDLFromModel(t *testing.T) {
	ddl := readPoolCredentialTableDDL()
	if ddl == "" {
		t.Fatal("read_pool_credential is absent from the meta roster")
	}
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS read_pool_credential", "id = 1", "secret text"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("rendered DDL missing %q:\n%s", want, ddl)
		}
	}
}
