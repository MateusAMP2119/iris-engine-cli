package store

import (
	"context"
	"fmt"
)

// This file is the meta read path for role credentials: the plain-MVCC read of
// the engine-minted secret persisted for a login role (credentials.pg_role ->
// secret). The run planes read a pipeline role's secret to build the scoped
// IRIS_DB_URL each run connects with, and the apply path reads it back so a
// re-apply reuses the persisted credential instead of minting a fresh one
// (create-once, mirroring the read-pool credential).

// RoleCredentialReader is the plain-MVCC read seam for a login role's persisted
// credential. A pgx-pool-backed implementation and a fake both satisfy it.
type RoleCredentialReader interface {
	// RoleSecret returns the persisted credential for pgRole; ok is false when the
	// role has no credentials row (never an error).
	RoleSecret(ctx context.Context, pgRole string) (Secret, bool, error)
}

// selectRoleSecretSQL reads one role's persisted credential.
const selectRoleSecretSQL = `SELECT secret FROM credentials WHERE pg_role = $1`

// pgxRoleCredentialReader is the pgx-pool-backed RoleCredentialReader.
type pgxRoleCredentialReader struct {
	pool readPool
}

// compile-time proof the pgx adapter satisfies the credential read seam.
var _ RoleCredentialReader = (*pgxRoleCredentialReader)(nil)

// newPgxRoleCredentialReader builds a credential reader over a pooled-query seam.
func newPgxRoleCredentialReader(pool readPool) *pgxRoleCredentialReader {
	return &pgxRoleCredentialReader{pool: pool}
}

// RoleSecret reads the role's persisted credential in one plain MVCC query.
func (r *pgxRoleCredentialReader) RoleSecret(ctx context.Context, pgRole string) (Secret, bool, error) {
	rows, err := r.pool.query(ctx, selectRoleSecretSQL, pgRole)
	if err != nil {
		return Secret{}, false, fmt.Errorf("store: read role credential for %q: %w", pgRole, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Secret{}, false, fmt.Errorf("store: read role credential for %q: %w", pgRole, err)
		}
		return Secret{}, false, nil
	}
	var secret string
	if err := rows.Scan(&secret); err != nil {
		return Secret{}, false, fmt.Errorf("store: scan role credential for %q: %w", pgRole, err)
	}
	if secret == "" {
		return Secret{}, false, nil
	}
	return Secret{value: secret}, true, nil
}
