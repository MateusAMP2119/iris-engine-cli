package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the data-database grant surface: it renders field-level GRANT DDL
// and reconciles a role's live Postgres
// grants against the meta access ledger. pg owns the data database, so the grant
// DDL is issued here (store owns the ledger's truth in meta; the two never cross).
// The rendering is deterministic, so a golden diff is a contract diff.
//
// The access ledger is authoritative: reconciliation emits GRANT DDL to make
// Postgres match the ledger, additively. Only additive
// gaps -- a ledger field the role lacks -- auto-resolve; a live grant beyond the
// ledger is a stray, reported and never revoked. A column-level GRANT is idempotent
// in Postgres, so re-issuing a reconcile's additive GRANTs is always safe.

// RenderGrant renders one field-level GRANT for role on schema.table.field: for a
// read grant, GRANT SELECT ("field") ON "schema"."table" TO "role"; for a write
// grant, GRANT INSERT ("field"), UPDATE ("field") ON "schema"."table" TO "role"
// (the two column-level write privileges Postgres supports). Every identifier is
// double-quoted, so a reserved word or a name containing a quote renders as a valid
// identifier. An empty role, schema, table, or field, or an unknown access kind,
// returns an error rather than emitting invalid SQL.
func RenderGrant(role, schema, table, field string, access declare.AccessKind) (string, error) {
	priv, err := columnPrivileges(field, access)
	if err != nil {
		return "", fmt.Errorf("pg: render GRANT %s.%s.%s to %q: %w", schema, table, field, role, err)
	}
	if err := requireNonEmpty(role, schema, table, field); err != nil {
		return "", fmt.Errorf("pg: render GRANT: %w", err)
	}
	return fmt.Sprintf("GRANT %s ON %s.%s TO %s;",
		priv, quoteIdentifier(schema), quoteIdentifier(table), quoteIdentifier(role)), nil
}

// columnPrivileges renders the column-level privilege clause for one field grant:
// SELECT ("field") for a read, INSERT ("field"), UPDATE ("field") for a write. An
// unknown access kind is an error.
func columnPrivileges(field string, access declare.AccessKind) (string, error) {
	col := quoteIdentifier(field)
	switch access {
	case declare.AccessRead:
		return "SELECT (" + col + ")", nil
	case declare.AccessWrite:
		return "INSERT (" + col + "), UPDATE (" + col + ")", nil
	default:
		return "", fmt.Errorf("unknown access kind %q (want read or write)", access)
	}
}

// requireNonEmpty returns an error if any of the grant's identifiers is empty, so a
// blank identifier never renders into invalid GRANT DDL.
func requireNonEmpty(role, schema, table, field string) error {
	for _, p := range []struct{ name, val string }{
		{"role", role}, {"schema", schema}, {"table", table}, {"field", field},
	} {
		if p.val == "" {
			return fmt.Errorf("%s is empty", p.name)
		}
	}
	return nil
}

// GrantReconcile is the outcome of reconciling a role's live grants against the
// meta access ledger: the additive GRANT statements that make Postgres match the
// ledger, and the strays Postgres holds beyond the ledger. Additive gaps
// auto-resolve; strays are reported, never revoked.
type GrantReconcile struct {
	// Grants are the GRANT statements for ledger fields Postgres lacks, in ledger
	// order. Applying them makes Postgres match the ledger additively; each is
	// idempotent, so re-applying is safe.
	Grants []string
	// Strays are the live grants beyond the ledger: non-additive drifts, reported
	// and never revoked.
	Strays []declare.Drift
}

// LiveGrantReader reads a role's current field-level grants from the data database
// (a pg_catalog / information_schema.column_privileges read). *Client implements it
// against a live Postgres; a fake stands in at integration tier so reconcile is
// provable with no live database.
type LiveGrantReader interface {
	// ReadFieldGrants returns the field-level grants Postgres currently holds for
	// role.
	ReadFieldGrants(ctx context.Context, role string) ([]declare.FieldGrant, error)
}

// ReconcileGrants diffs a role's live grants against the meta ledger's fixed
// per-field set and plans the reconciliation: a
// GRANT for each ledger field Postgres lacks (additive, in ledger order), and a
// report of each stray Postgres holds beyond the ledger (non-additive). It never
// grants a field absent from the ledger, so a column added to the table after a
// data PAT's mint -- absent from the fixed ledger -- is never granted. It renders
// only; ApplyReconcile or Reconcile issues the DDL.
func ReconcileGrants(role string, ledger, live []declare.FieldGrant) (GrantReconcile, error) {
	liveSet := make(map[string]struct{}, len(live))
	for _, g := range live {
		liveSet[fieldGrantKey(g)] = struct{}{}
	}

	var out GrantReconcile
	for _, g := range ledger {
		if _, held := liveSet[fieldGrantKey(g)]; held {
			continue // already granted; a column-level GRANT is idempotent but we skip the no-op.
		}
		ddl, err := RenderGrant(role, g.Schema, g.Table, g.Field, g.Access)
		if err != nil {
			return GrantReconcile{}, fmt.Errorf("pg: reconcile grants for %q: %w", role, err)
		}
		out.Grants = append(out.Grants, ddl)
	}
	out.Strays = declare.ClassifyFieldGrantDrift(role, ledger, live).NonAdditive()
	return out, nil
}

// fieldGrantKey is a FieldGrant's identity for the live-vs-ledger set diff, matching
// on schema, table, field, and access.
func fieldGrantKey(g declare.FieldGrant) string {
	return strings.Join([]string{g.Schema, g.Table, g.Field, string(g.Access)}, "\x00")
}

// ApplyReconcile issues a plan's additive GRANTs through db in order, making
// Postgres match the ledger. It issues no REVOKE: strays
// are reported, never silently fixed. Each GRANT is idempotent, so a retry after a
// partial failure re-issues cleanly. On the first statement error it stops and
// returns the error, naming the statement.
func ApplyReconcile(ctx context.Context, db DB, plan GrantReconcile) error {
	for _, stmt := range plan.Grants {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: apply grant reconcile: %q: %w", stmt, err)
		}
	}
	return nil
}

// Reconcile reads the role's live grants, diffs them against the ledger, applies
// the additive GRANTs onto the data database, and returns the plan (its stray set
// in particular) for reporting. The ledger is
// authoritative: reconciliation emits GRANT DDL to make Postgres match it, and
// strays beyond the ledger are reported, never revoked.
func Reconcile(ctx context.Context, db DB, reader LiveGrantReader, role string, ledger []declare.FieldGrant) (GrantReconcile, error) {
	live, err := reader.ReadFieldGrants(ctx, role)
	if err != nil {
		return GrantReconcile{}, fmt.Errorf("pg: reconcile grants for %q: read live grants: %w", role, err)
	}
	plan, err := ReconcileGrants(role, ledger, live)
	if err != nil {
		return GrantReconcile{}, err
	}
	if err := ApplyReconcile(ctx, db, plan); err != nil {
		return GrantReconcile{}, err
	}
	return plan, nil
}
