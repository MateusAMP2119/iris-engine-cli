package daemon

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// credsFake scripts the role-credential read: the named roles hold a freshly
// minted secret, the rest none.
type credsFake struct{ secrets map[string]store.Secret }

// newCredsFake mints one secret per named role.
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

// RoleSecret answers the scripted credential read.
func (f credsFake) RoleSecret(_ context.Context, pgRole string) (store.Secret, bool, error) {
	s, ok := f.secrets[pgRole]
	return s, ok, nil
}
