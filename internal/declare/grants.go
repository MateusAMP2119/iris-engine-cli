package declare

import (
	"fmt"
	"strings"
)

// This file is the grant-intent leaf of specification sections 3, 4, and 7: the
// per-field access a declaration or a data-PAT mint asks for, expressed as pure
// values with no database knowledge. It complements drift.go's grant-drift
// classifier (which diffs Postgres grants against the ledger's bounds): here we
// derive the intended grant set that becomes the ledger, and codify the standing
// bounds every engine-managed role is held to.
//
// Two granularities meet the grant world. The ledger records field-level grants
// (grants: pg_role, schema, table, field, access), so intent is expressed as
// FieldGrant; drift classification (drift.go) diffs privilege-level Grants (SELECT,
// INSERT, CONNECT on schema.object) so it can also police the standing bounds a
// per-field ledger cannot express -- a write on public, a CONNECT to meta.

// metaSchema names the meta control database, the one surface no engine-managed
// pipeline or data-PAT role may ever connect to (specification section 5). It is
// restated here rather than imported: declare is a leaf and must not depend on the
// meta client (store), so the standing-bounds rule states the doctrine constant
// itself, as drift.go does for the data journal.
const metaSchema = "meta"

// AccessKind is a field grant's access kind: read (a declared reads entry, or a
// data-PAT read) or write (a declared writes entry). It mirrors the meta grants
// table's access column (specification sections 3 and 4).
type AccessKind string

// The field-grant access kinds. The string values match the meta grants ledger's
// access column, so the store maps a FieldGrant onto a grants row by value.
const (
	// AccessRead is field-level read access.
	AccessRead AccessKind = "read"
	// AccessWrite is field-level write access.
	AccessWrite AccessKind = "write"
)

// FieldGrant is one field-level access grant a declaration or a data-PAT mint
// intends: the schema, table, and single field a role is granted, and the access
// kind. It is the unit the meta access ledger records (one grants row) and the pg
// grant reconcile renders (one column-level GRANT).
type FieldGrant struct {
	// Schema is the grant's schema.
	Schema string
	// Table is the grant's table.
	Table string
	// Field is the single column the grant covers (field-level: no all-columns).
	Field string
	// Access is the grant's access kind (read or write).
	Access AccessKind
}

// key is a FieldGrant's stable identity for set membership and de-duplication.
func (g FieldGrant) key() string {
	return strings.Join([]string{g.Schema, g.Table, g.Field, string(g.Access)}, "\x00")
}

// GrantsFromAccess expands a pipeline declaration's reads and writes into the exact
// per-field grant set the meta access ledger records (specification sections 3 and
// 4): one FieldGrant per (entry, field), reads tagged AccessRead and writes
// AccessWrite. The order is deterministic -- reads before writes, entry order, then
// field order -- and an exact-duplicate grant (same schema, table, field, access)
// is recorded once. Each entry must name a dotted schema.table and a non-empty
// field; a malformed entry yields an error (ValidateAccess is the gate, this
// expansion assumes a validated declaration but still refuses a malformed one
// rather than emit a broken grant).
func GrantsFromAccess(reads, writes []Access) ([]FieldGrant, error) {
	var out []FieldGrant
	seen := map[string]struct{}{}
	add := func(list string, entries []Access, access AccessKind) error {
		for i, a := range entries {
			schema, table, ok := splitDottedTable(a.Table)
			if !ok {
				return fmt.Errorf("declare: grants from access: %s[%d]: table %q is not a dotted schema.table name", list, i, a.Table)
			}
			if len(a.Fields) == 0 {
				return fmt.Errorf("declare: grants from access: %s[%d] (table %q): fields is required and must be non-empty", list, i, a.Table)
			}
			for _, f := range a.Fields {
				g := FieldGrant{Schema: schema, Table: table, Field: f, Access: access}
				if _, dup := seen[g.key()]; dup {
					continue
				}
				seen[g.key()] = struct{}{}
				out = append(out, g)
			}
		}
		return nil
	}
	if err := add("reads", reads, AccessRead); err != nil {
		return nil, err
	}
	if err := add("writes", writes, AccessWrite); err != nil {
		return nil, err
	}
	return out, nil
}

// DataPATRead is one --read or --endpoint argument at data-PAT mint (specification
// section 7). It is one of three shapes: a field-explicit read (Table dotted, Field
// set), a bare schema.table read (Table dotted, Field empty -- every declared field
// at mint time), or an --endpoint read (Endpoint set -- that endpoint's source
// fields). Endpoint takes precedence when set.
type DataPATRead struct {
	// Table is the dotted schema.table the read targets; empty for an --endpoint
	// read.
	Table string
	// Field is the explicit column; empty for a bare schema.table (all declared
	// fields) or an --endpoint read.
	Field string
	// Endpoint is the endpoint name; when set this is an --endpoint read and Table
	// and Field are ignored.
	Endpoint string
}

// EndpointSource is an endpoint's source table and projected fields, the set an
// --endpoint data-PAT read expands to (specification section 7). Source is the
// dotted schema.table the endpoint reads; Fields is its explicit field projection.
type EndpointSource struct {
	// Source is the endpoint's dotted schema.table source.
	Source string
	// Fields are the endpoint's projected fields.
	Fields []string
}

// ExpandDataPATGrants resolves a data PAT's mint read specs into the fixed
// per-field read grant set recorded at mint (specification section 7). A
// field-explicit read grants that one field; a bare schema.table grants every field
// the table declares at mint time (recorded per field, so a column added after mint
// is never silently granted -- the recorded set is fixed here); an --endpoint read
// grants the endpoint's source fields. declaredFields maps "schema.table" to the
// table's declared columns as of mint; endpoints maps an endpoint name to its
// source and fields. Every grant is AccessRead: a data PAT owns a read-only role.
// The order is deterministic (spec order, then field order) and an exact duplicate
// is recorded once. An unknown table or endpoint, a bare table that declares no
// fields, or a malformed table name yields an error naming it.
func ExpandDataPATGrants(reads []DataPATRead, declaredFields map[string][]string, endpoints map[string]EndpointSource) ([]FieldGrant, error) {
	var out []FieldGrant
	seen := map[string]struct{}{}
	emit := func(schema, table, field string) {
		g := FieldGrant{Schema: schema, Table: table, Field: field, Access: AccessRead}
		if _, dup := seen[g.key()]; dup {
			return
		}
		seen[g.key()] = struct{}{}
		out = append(out, g)
	}

	for i, r := range reads {
		switch {
		case r.Endpoint != "":
			ep, ok := endpoints[r.Endpoint]
			if !ok {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: unknown endpoint %q", i, r.Endpoint)
			}
			schema, table, ok := splitDottedTable(ep.Source)
			if !ok {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: endpoint %q source %q is not a dotted schema.table", i, r.Endpoint, ep.Source)
			}
			if len(ep.Fields) == 0 {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: endpoint %q declares no source fields", i, r.Endpoint)
			}
			for _, f := range ep.Fields {
				emit(schema, table, f)
			}
		default:
			schema, table, ok := splitDottedTable(r.Table)
			if !ok {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: table %q is not a dotted schema.table", i, r.Table)
			}
			if r.Field != "" {
				emit(schema, table, r.Field) // field-explicit
				continue
			}
			// Bare schema.table: every field declared at mint time, recorded per field.
			fields, ok := declaredFields[r.Table]
			if !ok {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: table %q is not declared", i, r.Table)
			}
			if len(fields) == 0 {
				return nil, fmt.Errorf("declare: expand data-PAT grants: read[%d]: bare table %q declares no fields", i, r.Table)
			}
			for _, f := range fields {
				emit(schema, table, f)
			}
		}
	}
	return out, nil
}

// ClassifyFieldGrantDrift classifies a role's live field grants against the meta
// access ledger's fixed per-field set (specification section 5), reusing the
// privilege-level ClassifyGrantDrift by mapping each FieldGrant to its column-level
// privilege. A ledger field the role lacks is an additive gap (reconcile grants
// it); a live field grant beyond the ledger is a stray -- non-additive, reported,
// never silently fixed. The reconcile in pg drives the additive GRANTs from the
// ledger directly; this classification supplies the drift report (its stray set in
// particular).
func ClassifyFieldGrantDrift(role string, ledger, live []FieldGrant) DriftReport {
	return ClassifyGrantDrift(GrantView{
		Bounds: toGrants(role, ledger),
		Live:   toGrants(role, live),
	})
}

// toGrants maps field grants to the privilege-level Grants ClassifyGrantDrift
// diffs, encoding the field in the privilege so two grants on the same table but
// different fields stay distinct.
func toGrants(role string, fgs []FieldGrant) []Grant {
	out := make([]Grant, 0, len(fgs))
	for _, g := range fgs {
		out = append(out, Grant{
			Role:      role,
			Schema:    g.Schema,
			Object:    g.Table,
			Privilege: fieldPrivilege(g.Field, g.Access),
		})
	}
	return out
}

// fieldPrivilege renders a field grant's column-level privilege token, e.g.
// "SELECT(amount)" for a read or "INSERT,UPDATE(amount)" for a write. It is a
// stable key for drift set membership and a human-readable privilege in a stray
// report.
func fieldPrivilege(field string, access AccessKind) string {
	if access == AccessWrite {
		return "INSERT,UPDATE(" + field + ")"
	}
	return "SELECT(" + field + ")"
}

// ExceedsStandingBounds reports whether a live grant is beyond the standing bounds
// every engine-managed pipeline or data-PAT role is held to regardless of the
// per-field ledger (specification sections 4 and 5): on public a role may hold read
// (SELECT) only, and no role may CONNECT to meta. A grant matching either rule is a
// stray beyond bounds -- non-additive drift, reported, never silently fixed. The
// reason is a human-readable explanation, empty when the grant is within bounds.
func ExceedsStandingBounds(g Grant) (exceeds bool, reason string) {
	if strings.EqualFold(g.Privilege, "CONNECT") && (g.Schema == metaSchema || g.Object == metaSchema) {
		return true, fmt.Sprintf("role %q holds CONNECT on meta; no engine-managed role may connect to the meta control database", g.Role)
	}
	if g.Schema == publicSchema && !strings.EqualFold(g.Privilege, "SELECT") {
		return true, fmt.Sprintf("role %q holds %s on public.%s; on public an engine-managed role may hold read (SELECT) only", g.Role, g.Privilege, g.Object)
	}
	return false, ""
}
