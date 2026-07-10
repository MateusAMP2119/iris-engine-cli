package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file is the SQL-safety layer of the read API (specification section 7):
// the surface executes only engine-built statements with bound params, and caller
// input never becomes SQL text. For /q the statement text is the endpoint's
// compiled SQL, fixed at apply; for /data it is assembled here -- once per shape,
// from validated bare identifiers only -- into the same fixed-text,
// all-params-optional form the endpoint compiler emits, so request handling never
// assembles SQL for either route: BindArgs turns a validated QueryPlan into the
// positional argument vector the read pool binds against the prepared text.
// Every identifier is a single bare name (no dots, no quotes), so a statement can
// never carry a database-qualified reference: meta, a separate database, is
// unaddressable from the data surface.

// bareIdentRe is the shape of every identifier the /data assembler accepts: one
// bare lowercase Postgres name. No dot (no schema-, database-, or
// catalog-qualification smuggled through a name), no quote, no space, no
// uppercase -- declared names are lowercase and anything else is refused.
var bareIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// pgTypeRe is the shape of a Postgres type token the assembler will render as a
// cast: lowercase words with an optional (n) or (n,m) parametrization, matching
// the closed declared-column type mapping. Anything else is refused, so a type
// string can never carry SQL.
var pgTypeRe = regexp.MustCompile(`^[a-z][a-z0-9 ]*( *\([0-9]+(, *[0-9]+)?\))?$`)

// DataStatement is the engine-assembled fixed statement for one /data shape
// (schema, table, projection): a stable prepared-statement name, the fixed
// parameterized SELECT text, and the ordered binding plan -- the same triple a
// compiled /q endpoint carries, so both routes run through the read pool
// identically.
type DataStatement struct {
	// Name is the stable prepared-statement name for this shape.
	Name string
	// SQL is the fixed parameterized text; every filter is an optional NULL-bound
	// param, so one text serves every request of the shape.
	SQL string
	// Params is the ordered parameter-binding plan, one slot per $n in SQL.
	Params []declare.ParamSlot
}

// checkBareIdent refuses any identifier that is not a single bare lowercase name.
func checkBareIdent(kind, name string) error {
	if !bareIdentRe.MatchString(name) {
		return fmt.Errorf("api: build data statement: %s %q is not a bare identifier", kind, name)
	}
	return nil
}

// BuildDataStatement assembles the fixed statement for one /data shape from
// validated identifiers only (specification section 7): schema, table, the
// projected columns, the filterable columns with their Postgres types, and the
// table's primary key. Every name must be a single bare identifier and every type
// a plain type token; anything else is refused, so no caller-influenced byte ever
// reaches statement text. The output is deterministic: each filter column binds
// one equality slot plus an inclusive range pair (<col>_from / <col>_to, the
// /data grammar's eq/range filters; a range param whose name collides with
// another filter column is skipped, the declared column wins), in sorted column
// order, then the keyset after cursor (single-column primary keys only), then
// the limit, mirroring the endpoint compiler's optional-NULL-bound form. The
// statement references only the given columns -- callers pass the columns a
// request actually addresses, so no unaddressed column's grant is ever dragged
// into a read.
func BuildDataStatement(schema, table string, projection []string, filters map[string]string, primaryKey []string) (*DataStatement, error) {
	if err := checkBareIdent("schema", schema); err != nil {
		return nil, err
	}
	if err := checkBareIdent("table", table); err != nil {
		return nil, err
	}
	if len(projection) == 0 {
		return nil, fmt.Errorf("api: build data statement for %s.%s: empty projection", schema, table)
	}
	for _, c := range projection {
		if err := checkBareIdent("projected column", c); err != nil {
			return nil, err
		}
	}
	if len(primaryKey) == 0 {
		return nil, fmt.Errorf("api: build data statement for %s.%s: empty primary key", schema, table)
	}
	for _, c := range primaryKey {
		if err := checkBareIdent("primary key column", c); err != nil {
			return nil, err
		}
	}
	filterCols := make([]string, 0, len(filters))
	for c, pt := range filters {
		if err := checkBareIdent("filter column", c); err != nil {
			return nil, err
		}
		if !pgTypeRe.MatchString(pt) {
			return nil, fmt.Errorf("api: build data statement for %s.%s: column %q has malformed type %q", schema, table, c, pt)
		}
		filterCols = append(filterCols, c)
	}
	sort.Strings(filterCols)

	var (
		clauses []string
		params  []declare.ParamSlot
		idx     = 1
	)
	// optional renders the endpoint compiler's optionally-bound predicate form: a
	// NULL param means the request omitted this filter, so the clause matches
	// every row and the text stays fixed across requests.
	optional := func(col, pt, op string) string {
		p := fmt.Sprintf("$%d::%s", idx, pt)
		return fmt.Sprintf("(%s IS NULL OR %s %s %s)", p, col, op, p)
	}

	for _, c := range filterCols {
		pt := filters[c]
		clauses = append(clauses, optional(c, pt, "="))
		params = append(params, declare.ParamSlot{Index: idx, Param: c, Kind: declare.ParamEq, Column: c, PgType: pt})
		idx++
		if _, taken := filters[c+"_from"]; !taken {
			clauses = append(clauses, optional(c, pt, ">="))
			params = append(params, declare.ParamSlot{Index: idx, Param: c + "_from", Kind: declare.ParamRangeFrom, Column: c, PgType: pt})
			idx++
		}
		if _, taken := filters[c+"_to"]; !taken {
			clauses = append(clauses, optional(c, pt, "<="))
			params = append(params, declare.ParamSlot{Index: idx, Param: c + "_to", Kind: declare.ParamRangeTo, Column: c, PgType: pt})
			idx++
		}
	}

	// The keyset after cursor exists only for a single-column primary key: a
	// composite key has no single-value cursor (the /data grammar refuses one).
	if len(primaryKey) == 1 {
		key := primaryKey[0]
		keyType, ok := filters[key]
		if !ok {
			return nil, fmt.Errorf("api: build data statement for %s.%s: primary key column %q has no known type", schema, table, key)
		}
		clauses = append(clauses, optional(key, keyType, ">"))
		params = append(params, declare.ParamSlot{Index: idx, Param: "after", Kind: declare.ParamAfter, Column: key, PgType: keyType})
		idx++
	}

	limitSlot := declare.ParamSlot{Index: idx, Param: "limit", Kind: declare.ParamLimit}

	orderCols := make([]string, len(primaryKey))
	for i, c := range primaryKey {
		orderCols[i] = c + " ASC"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s\n", strings.Join(projection, ", "))
	fmt.Fprintf(&b, "FROM %s.%s\n", schema, table)
	if len(clauses) > 0 {
		fmt.Fprintf(&b, "WHERE %s\n", strings.Join(clauses, "\n  AND "))
	}
	fmt.Fprintf(&b, "ORDER BY %s\n", strings.Join(orderCols, ", "))
	fmt.Fprintf(&b, "LIMIT $%d;", limitSlot.Index)
	params = append(params, limitSlot)

	sql := b.String()
	sum := sha256.Sum256([]byte(sql))
	name := fmt.Sprintf("data_%s_%s_%s", schema, table, hex.EncodeToString(sum[:4]))

	return &DataStatement{Name: name, SQL: sql, Params: params}, nil
}

// opForSlotKind maps a binding-plan slot kind to the predicate operator that
// fills it; ok is false for the cursor and limit slots, which bind from the
// cursor plan instead.
func opForSlotKind(k declare.ParamKind) (PredicateOp, bool) {
	switch k {
	case declare.ParamEq:
		return OpEq, true
	case declare.ParamRangeFrom:
		return OpGte, true
	case declare.ParamRangeTo:
		return OpLte, true
	default:
		return "", false
	}
}

// BindArgs turns a validated QueryPlan into the positional argument vector for a
// fixed statement's binding plan -- the compiled slots of a /q endpoint or the
// assembled slots of a /data shape. Every predicate must find its slot and the
// cursor must fit the text (ascending only; the fixed texts carry no descending
// form), so no part of a request can be silently dropped; an omitted optional
// param binds NULL. Values land only in the returned args, never in SQL text.
func BindArgs(slots []declare.ParamSlot, plan *QueryPlan) ([]any, error) {
	if plan == nil {
		return nil, errors.New("api: bind args: nil query plan")
	}
	if plan.Cursor.Descending {
		return nil, errors.New("api: bind args: a fixed read statement pages ascending only; a descending cursor has no slot")
	}

	args := make([]any, len(slots))
	used := make([]bool, len(plan.Predicates))
	boundCursor := false
	for i, s := range slots {
		if s.Index != i+1 {
			return nil, fmt.Errorf("api: bind args: slot %d carries index %d; the binding plan is misaligned", i+1, s.Index)
		}
		switch s.Kind {
		case declare.ParamLimit:
			args[i] = plan.Cursor.Limit
		case declare.ParamAfter:
			if b := plan.Cursor.Bound; b != nil {
				if b.Op != OpGt {
					return nil, fmt.Errorf("api: bind args: cursor operator %q does not fit the ascending after slot", b.Op)
				}
				args[i] = b.Value
				boundCursor = true
			}
		default:
			op, ok := opForSlotKind(s.Kind)
			if !ok {
				return nil, fmt.Errorf("api: bind args: slot %q has unknown kind %q", s.Param, s.Kind)
			}
			for j, p := range plan.Predicates {
				if !used[j] && p.Column == s.Column && p.Op == op {
					args[i] = p.Value
					used[j] = true
					break
				}
			}
		}
	}
	for j, p := range plan.Predicates {
		if !used[j] {
			return nil, fmt.Errorf("api: bind args: predicate on %q (%s) has no slot in the statement's binding plan", p.Column, p.Op)
		}
	}
	if plan.Cursor.Bound != nil && !boundCursor {
		return nil, errors.New("api: bind args: the plan carries a cursor bound but the statement has no after slot")
	}
	return args, nil
}
