package store

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file is the access-ledger record path driven from a declaration or a
// data-PAT mint: it turns the intended per-field grants (declare's FieldGrant
// intent) into the meta grants ledger, rewriting a role's grants as one atomic
// full-role rewrite over the single writer. It builds on ReplaceGrants (the
// atomic write in roles.go); the value here is driving that write from a
// declaration's reads/writes and a data PAT's expanded per-field grant set, so
// the ledger reflects exactly the declared access, nothing more. Truth lives in
// meta; pg reconciles it onto the data database.

// RecordAccessGrants records a pipeline role's declared reads and writes in the
// meta access ledger: it expands the declaration's reads/writes into the exact
// per-field grant set and rewrites the role's grants as one atomic full-role
// rewrite. Apply drives this from a declaration, so the ledger records exactly
// the declared reads (as read grants) and writes (as write grants), nothing more.
// A malformed reads/writes entry is refused before any write. It is a leader-only
// meta write, riding the single Writer.
func (w *Writer) RecordAccessGrants(ctx context.Context, pgRole string, reads, writes []declare.Access) error {
	grants, err := declare.GrantsFromAccess(reads, writes)
	if err != nil {
		return fmt.Errorf("store: writer record access grants for %q: %w", pgRole, err)
	}
	// The atomic write's error prefix is owned by ReplaceGrants (via RecordGrants),
	// which already names the role, so it is returned unwrapped here.
	return w.RecordGrants(ctx, pgRole, grants)
}

// RecordGrants records a role's fixed per-field grant set in the meta access
// ledger as one atomic full-role rewrite. It is the record half of a data-PAT
// mint: the caller expands the mint read specs (field-explicit, bare
// schema.table, or --endpoint) into the fixed per-field grants via
// declare.ExpandDataPATGrants, and this records them per field. Passing an empty
// set clears the role's grants. It maps each field grant onto a grants row and
// delegates to the atomic ReplaceGrants, so the whole set commits together or not
// at all. It is a leader-only meta write, riding the single Writer.
func (w *Writer) RecordGrants(ctx context.Context, pgRole string, grants []declare.FieldGrant) error {
	rows := make([]Grant, 0, len(grants))
	for _, g := range grants {
		rows = append(rows, Grant{
			Schema: g.Schema,
			Table:  g.Table,
			Field:  g.Field,
			Access: GrantAccess(g.Access),
		})
	}
	// ReplaceGrants owns the atomic-write error prefix and already names the role,
	// so its error is returned unwrapped rather than re-prefixed here.
	return w.ReplaceGrants(ctx, pgRole, rows)
}
