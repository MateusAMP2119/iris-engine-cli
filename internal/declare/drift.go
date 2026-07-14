package declare

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNilDeclaredTable is returned by the schema and ledger drift classifiers when
// the declared table head is nil: a programming error the caller must fix, not a
// drift. Both classifiers surface it identically, never a silent empty report.
var ErrNilDeclaredTable = errors.New("declare: classify drift: nil declared table")

// This file holds drift classification as pure diffing logic under the
// additive-only doctrine. Three comparisons -- schema
// (live Postgres vs the declared head), ledger (table.yaml vs the migration-ledger
// head), and grant (Postgres grants vs the meta access ledger's bounds) -- each
// reduce to the same rule: only additive gaps auto-resolve; every other
// discrepancy is reported without automatic action, and non-additive schema and
// ledger changes are refused outright.
//
// The classifier is pure and holds no database knowledge. It consumes declared
// Table heads (this package) and abstract views of the live world -- LiveTable,
// LedgerState, GrantView -- that its callers in internal/pg fill: the sync engine
// (pg/sync.go) drives the schema and ledger comparisons, the grant reconcile
// (pg/grants.go) drives the grant comparison. This leaf owns the classification
// vocabulary and its rules; executing the resolved actions -- the ADD COLUMN DDL,
// the migration file, the GRANT -- belongs to internal/pg. Who supplies each view
// is documented on its type below; several are still assembled only by their
// consumers' tests, the daemon's apply path driving provisioning's own coarser
// views instead.

// journalSchema and journalTableName name the engine-owned data journal
// (public.data_journal), the one surface the schema-drift comparison always
// excludes. They mirror internal/pg's JournalName without importing it: declare is
// a leaf package and must not depend on the data-database
// client, so the doctrine constant is restated here.
const (
	journalSchema    = "public"
	journalTableName = "data_journal"
)

// DriftKind is the closed additive/non-additive classification of a drift. Only
// additive gaps auto-resolve; everything else is reported without automatic
// action.
type DriftKind string

// The two drift kinds.
const (
	// DriftAdditive is a gap the engine may close additively (add a column, a
	// migration, a capture trigger, or a missing grant).
	DriftAdditive DriftKind = "additive"
	// DriftNonAdditive is any other discrepancy: an extra, renamed, retyped, or
	// removed object, or a grant beyond the ledger's bounds.
	DriftNonAdditive DriftKind = "non_additive"
)

// DriftAction is the closed set of outcomes a classified drift carries. It is
// deliberately gate-free: a non-additive change is refused or reported outright,
// never offered a confirmation gate.
type DriftAction string

// The three drift actions.
const (
	// ActionAutofix resolves an additive gap automatically (the engine acts).
	ActionAutofix DriftAction = "autofix"
	// ActionRefuse blocks apply on a non-additive schema or ledger change; the
	// engine never auto-drops or rewrites the offending object.
	ActionRefuse DriftAction = "refuse"
	// ActionReport surfaces a non-additive grant beyond bounds; it is reported,
	// never silently reconciled.
	ActionReport DriftAction = "report"
)

// DriftDomain names which of the three comparisons produced a drift.
type DriftDomain string

// The three drift domains.
const (
	// DomainSchema is live Postgres versus the declared head.
	DomainSchema DriftDomain = "schema"
	// DomainLedger is table.yaml versus the migration-ledger head.
	DomainLedger DriftDomain = "ledger"
	// DomainGrant is Postgres grants versus the meta access ledger's bounds.
	DomainGrant DriftDomain = "grant"
)

// DriftSubject names the kind of object a drift concerns.
type DriftSubject string

// The drift subjects.
const (
	// SubjectColumn is a table column.
	SubjectColumn DriftSubject = "column"
	// SubjectCaptureTrigger is the engine's capture trigger on a declared table.
	SubjectCaptureTrigger DriftSubject = "capture_trigger"
	// SubjectGrant is a Postgres grant.
	SubjectGrant DriftSubject = "grant"
)

// Drift is one classified discrepancy between the declared world and a live,
// ledger, or grant view. It is a closed value: the domain and subject that
// produced it, the object name, its additive/non-additive kind, the action the
// engine takes, and a human-readable detail. By construction it carries no
// confirmation field: a non-additive change is refused or reported outright, so
// there is no gate to offer.
type Drift struct {
	// Domain is the comparison that produced this drift.
	Domain DriftDomain
	// Subject is the kind of object concerned.
	Subject DriftSubject
	// Name identifies the object (a column name, or a grant/trigger identity).
	Name string
	// Kind is the additive/non-additive classification.
	Kind DriftKind
	// Action is the resolved outcome (autofix, refuse, or report).
	Action DriftAction
	// Detail is a human-readable reason for the classification.
	Detail string
}

// DriftReport is the outcome of classifying one or more drift comparisons: the
// drifts found, closed over the additive-only doctrine. Like Drift, it carries no
// confirmation gate -- a non-additive change is refused outright.
type DriftReport struct {
	// Drifts are the classified discrepancies, in a deterministic order (declared
	// order first, then extra objects sorted by name).
	Drifts []Drift
}

// Refused reports whether any drift refuses apply (a non-additive schema or
// ledger change). A report with no refusing drift applies cleanly once its
// autofixes run.
func (r DriftReport) Refused() bool {
	for _, d := range r.Drifts {
		if d.Action == ActionRefuse {
			return true
		}
	}
	return false
}

// Autofixes returns the drifts the engine resolves automatically (additive gaps).
func (r DriftReport) Autofixes() []Drift {
	return r.filter(func(d Drift) bool { return d.Action == ActionAutofix })
}

// NonAdditive returns the drifts classified non-additive (refused or reported).
func (r DriftReport) NonAdditive() []Drift {
	return r.filter(func(d Drift) bool { return d.Kind == DriftNonAdditive })
}

// filter returns the drifts for which keep is true, never nil for a non-empty
// match, and nil when none match.
func (r DriftReport) filter(keep func(Drift) bool) []Drift {
	var out []Drift
	for _, d := range r.Drifts {
		if keep(d) {
			out = append(out, d)
		}
	}
	return out
}

// IsEngineOwnedTable reports whether (schema, table) names an engine-owned surface
// excluded from the schema-drift comparison: the partitioned data journal in
// public. Engine-owned objects -- the journal and the
// capture triggers -- are never flagged as drift; table.yaml governs only declared
// user tables, while the journal is undeclared and engine-ensured. Column-level
// exclusion is deliberately absent: an engine-added column on a user table is
// non-additive drift, refused.
func IsEngineOwnedTable(schema, table string) bool {
	return schema == journalSchema && table == journalTableName
}

// LiveColumn is one column physically present in Postgres, as the schema-drift
// comparison sees it: its name and canonical Postgres type. The type is compared
// against the declared column's resolved Postgres type (ResolveColumnType).
type LiveColumn struct {
	// Name is the live column name.
	Name string
	// Type is the canonical Postgres type (e.g. "integer", "varchar(20)").
	Type string
}

// LiveTable is the live-Postgres view of one declared table for schema-drift
// classification: the columns physically present and whether the engine's capture
// trigger is installed. Engine-owned surfaces never enter the column set -- the
// journal is a separate table (IsEngineOwnedTable) and the capture trigger is a
// trigger object tracked by HasCaptureTrigger, never a column. The classifier is
// pure over the view, and no production reader assembles one today: its only
// consumer is the sync engine's schema-fix planner (pg.PlanSchemaFix), whose tests
// build the view, while the apply path the daemon drives goes through provisioning,
// which reads a coarser live view of its own (pg.LiveView: which schemas, tables,
// and capture triggers exist) from information_schema and pg_trigger.
type LiveTable struct {
	// Schema is the table's schema name.
	Schema string
	// Table is the table name.
	Table string
	// Columns are the live columns.
	Columns []LiveColumn
	// HasCaptureTrigger reports whether the engine's capture trigger is installed.
	HasCaptureTrigger bool
}

// ClassifySchema classifies live Postgres against a set of declared table heads.
// It walks the declared tables, matches each to its
// live counterpart by (schema, table), and classifies per ClassifySchemaDrift.
// Engine-owned live tables (the journal) never enter the comparison: they are
// filtered before any diff, so they are never flagged even when present in the
// live view. A declared table with no live counterpart is created wholesale by
// provisioning, not a drift, so it contributes nothing here.
func ClassifySchema(declared []*Table, live []LiveTable) (DriftReport, error) {
	byName := make(map[string]LiveTable, len(live))
	for _, lt := range live {
		if IsEngineOwnedTable(lt.Schema, lt.Table) {
			continue // the journal is excluded from the comparison entirely.
		}
		byName[lt.Schema+"."+lt.Table] = lt
	}

	var report DriftReport
	for _, d := range declared {
		if d == nil {
			continue
		}
		lt, ok := byName[d.Schema+"."+d.Table]
		if !ok {
			continue // provisioning creates a missing table; not a drift.
		}
		sub, err := ClassifySchemaDrift(d, lt)
		if err != nil {
			return DriftReport{}, err
		}
		report.Drifts = append(report.Drifts, sub.Drifts...)
	}
	return report, nil
}

// ClassifySchemaDrift classifies live Postgres against the declared head of one
// table. A declared column absent from live is an
// additive gap (auto ADD COLUMN); an extra live column, or one whose live type no
// longer matches the declared head (a rename leaves the old name extra, a retype
// changes the type), is non-additive and refuses apply, never auto-dropped. The
// engine's capture trigger is engine-owned: a missing one is additive/autofix
// (like a missing column), and its presence is never flagged. An engine-added
// column on a user table is not engine-owned -- it is an extra live column,
// non-additive drift, refused (table.yaml stays
// authoritative). A declared column whose YAML type is outside the closed type set
// returns an error naming the table and column (apply refuses on it separately).
func ClassifySchemaDrift(declared *Table, live LiveTable) (DriftReport, error) {
	if declared == nil {
		return DriftReport{}, ErrNilDeclaredTable
	}
	qualified := declared.Schema + "." + declared.Table

	liveByName := make(map[string]LiveColumn, len(live.Columns))
	for _, lc := range live.Columns {
		liveByName[lc.Name] = lc
	}
	declaredByName := make(map[string]struct{}, len(declared.Columns))

	var report DriftReport
	// Declared columns, in declaration order: additive when missing, non-additive
	// when the live type diverges (retype).
	for _, col := range declared.Columns {
		declaredByName[col.Name] = struct{}{}
		wantType, err := ResolveColumnType(col)
		if err != nil {
			return DriftReport{}, fmt.Errorf("declare: classify schema drift: table %s: %w", qualified, err)
		}
		name := qualified + "." + col.Name
		lc, present := liveByName[col.Name]
		switch {
		case !present:
			report.Drifts = append(report.Drifts, Drift{
				Domain: DomainSchema, Subject: SubjectColumn, Name: name,
				Kind: DriftAdditive, Action: ActionAutofix,
				Detail: fmt.Sprintf("declared column %q absent from live table; add %s", col.Name, wantType),
			})
		case lc.Type != wantType:
			report.Drifts = append(report.Drifts, Drift{
				Domain: DomainSchema, Subject: SubjectColumn, Name: name,
				Kind: DriftNonAdditive, Action: ActionRefuse,
				Detail: fmt.Sprintf("live column %q is %s, declared %s; a retype is non-additive, apply refuses", col.Name, lc.Type, wantType),
			})
		}
	}

	// Extra live columns (present in live, absent from the declared head), sorted
	// by name for a deterministic report: non-additive, refused, never auto-dropped.
	// This covers a renamed-away old column and an engine-added column alike; the
	// engine-owned exclusion is object-level (journal, trigger), never column-level.
	var extras []string
	for _, lc := range live.Columns {
		if _, ok := declaredByName[lc.Name]; !ok {
			extras = append(extras, lc.Name)
		}
	}
	sort.Strings(extras)
	for _, name := range extras {
		report.Drifts = append(report.Drifts, Drift{
			Domain: DomainSchema, Subject: SubjectColumn, Name: qualified + "." + name,
			Kind: DriftNonAdditive, Action: ActionRefuse,
			Detail: fmt.Sprintf("live column %q is not in the declared head; extra/renamed columns are non-additive, apply refuses (never auto-dropped)", name),
		})
	}

	// The engine's capture trigger: engine-owned. A missing one is additive/autofix,
	// like a missing column; an installed one is never flagged.
	if !live.HasCaptureTrigger {
		report.Drifts = append(report.Drifts, Drift{
			Domain: DomainSchema, Subject: SubjectCaptureTrigger, Name: qualified,
			Kind: DriftAdditive, Action: ActionAutofix,
			Detail: "capture trigger absent; the engine installs it additively, like a missing column",
		})
	}
	return report, nil
}

// LedgerColumn is one column in the reconstructed ledger head: the state after a
// table's migration chain (0001_create plus each additive ADD COLUMN) is applied.
// Its type is the YAML type token, matching table.yaml, so ledger comparison is
// name-and-type over the declared shapes.
type LedgerColumn struct {
	// Name is the ledger column name.
	Name string
	// Type is the YAML type token recorded by the migration chain.
	Type string
}

// LedgerState is the ledger-head view of one table: the columns the applied
// migration chain has established, reconstructed from the migrations/ files and the
// meta migrations table. The classifier is pure over it. Its only consumer is the
// sync engine (pg.PlanLedgerSync), which takes it inside a pg.LedgerView that only
// the sync engine's own tests assemble; the apply path the daemon drives
// reconstructs the same two facts into provisioning's own view (pg.TableLedger)
// instead.
type LedgerState struct {
	// Columns are the ledger-head columns.
	Columns []LedgerColumn
}

// ClassifyLedgerDrift classifies a table's declared head (table.yaml) against its
// migration-ledger head. A declared column absent from the ledger is an additive
// gap: the next migration is generated to add it (the sync engine in internal/pg
// writes the immutable migration file and records the new head). A ledger column
// absent from the declared head -- a column removed from table.yaml -- is
// non-additive and refused, never dropped. A column whose declared YAML type
// differs from the ledger's is a non-additive retype, also refused. A nil declared
// head returns ErrNilDeclaredTable, consistent with ClassifySchemaDrift.
func ClassifyLedgerDrift(declared *Table, ledger LedgerState) (DriftReport, error) {
	if declared == nil {
		return DriftReport{}, ErrNilDeclaredTable
	}
	qualified := declared.Schema + "." + declared.Table

	ledgerByName := make(map[string]LedgerColumn, len(ledger.Columns))
	for _, lc := range ledger.Columns {
		ledgerByName[lc.Name] = lc
	}
	declaredByName := make(map[string]struct{}, len(declared.Columns))

	var report DriftReport
	for _, col := range declared.Columns {
		declaredByName[col.Name] = struct{}{}
		name := qualified + "." + col.Name
		lc, present := ledgerByName[col.Name]
		switch {
		case !present:
			report.Drifts = append(report.Drifts, Drift{
				Domain: DomainLedger, Subject: SubjectColumn, Name: name,
				Kind: DriftAdditive, Action: ActionAutofix,
				Detail: fmt.Sprintf("declared column %q is beyond the ledger head; generate the next migration to add it", col.Name),
			})
		case strings.TrimSpace(lc.Type) != strings.TrimSpace(col.Type):
			report.Drifts = append(report.Drifts, Drift{
				Domain: DomainLedger, Subject: SubjectColumn, Name: name,
				Kind: DriftNonAdditive, Action: ActionRefuse,
				Detail: fmt.Sprintf("declared column %q is %s, ledger head has %s; a retype is non-additive, apply refuses", col.Name, col.Type, lc.Type),
			})
		}
	}

	// Ledger columns removed from table.yaml, sorted by name: non-additive, refused,
	// never dropped.
	var removed []string
	for _, lc := range ledger.Columns {
		if _, ok := declaredByName[lc.Name]; !ok {
			removed = append(removed, lc.Name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		report.Drifts = append(report.Drifts, Drift{
			Domain: DomainLedger, Subject: SubjectColumn, Name: qualified + "." + name,
			Kind: DriftNonAdditive, Action: ActionRefuse,
			Detail: fmt.Sprintf("column %q is in the ledger head but removed from table.yaml; a removal is non-additive, apply refuses (never dropped)", name),
		})
	}
	return report, nil
}

// Grant is one Postgres grant, as the grant-drift comparison sees it: a role, the
// schema and object it applies to, and the privilege. It is the unit both the meta
// access ledger's bounds and the live Postgres grants are expressed in.
type Grant struct {
	// Role is the grantee role name.
	Role string
	// Schema is the object's schema (e.g. "public", "meta").
	Schema string
	// Object is the granted object (a table or database name).
	Object string
	// Privilege is the granted privilege (e.g. "SELECT", "CONNECT").
	Privilege string
}

// key returns a stable identity for a grant, for set membership and reporting.
func (g Grant) key() string {
	return strings.Join([]string{g.Role, g.Schema, g.Object, g.Privilege}, ":")
}

// GrantView is the bounds-check input for grant drift: the grants the meta access
// ledger asserts (Bounds) and the grants Postgres currently holds (Live). On public,
// pipeline and data-PAT roles may hold read only, and none may connect to meta; a
// grant Postgres holds beyond Bounds is a stray grant. The classifier is pure over
// the view. pg's grant reconcile (pg.ReconcileGrants, through
// ClassifyFieldGrantDrift) is its consumer, and reads the Live half through the
// pg.LiveGrantReader seam -- a pg_catalog / information_schema.column_privileges
// read. No production reader implements that seam today: a fake stands in at test
// tier, and the engine's role provisioning applies the ledger's grants without
// consulting a live read.
type GrantView struct {
	// Bounds are the grants the meta access ledger asserts (the allowed set).
	Bounds []Grant
	// Live are the grants Postgres currently holds.
	Live []Grant
}

// ClassifyGrantDrift classifies Postgres grants against the meta access ledger's
// bounds. A bound grant Postgres lacks is an additive
// gap: reconciliation grants it (autofix). A grant Postgres holds beyond the
// bounds is a stray grant: non-additive, reported, never silently fixed. Only
// additive gaps auto-resolve; all else is reported.
func ClassifyGrantDrift(view GrantView) DriftReport {
	boundKeys := make(map[string]struct{}, len(view.Bounds))
	for _, g := range view.Bounds {
		boundKeys[g.key()] = struct{}{}
	}
	liveKeys := make(map[string]struct{}, len(view.Live))
	for _, g := range view.Live {
		liveKeys[g.key()] = struct{}{}
	}

	var report DriftReport
	// Missing grants (bound but not live), in bounds order: additive/autofix.
	for _, g := range view.Bounds {
		if _, ok := liveKeys[g.key()]; !ok {
			report.Drifts = append(report.Drifts, Drift{
				Domain: DomainGrant, Subject: SubjectGrant, Name: grantName(g),
				Kind: DriftAdditive, Action: ActionAutofix,
				Detail: fmt.Sprintf("ledger asserts grant %s but Postgres lacks it; reconcile grants it", grantName(g)),
			})
		}
	}
	// Stray grants (live but beyond bounds), sorted by name for a deterministic
	// report -- like the schema/ledger extras: non-additive/report.
	var strays []Grant
	for _, g := range view.Live {
		if _, ok := boundKeys[g.key()]; !ok {
			strays = append(strays, g)
		}
	}
	sort.Slice(strays, func(i, j int) bool { return grantName(strays[i]) < grantName(strays[j]) })
	for _, g := range strays {
		report.Drifts = append(report.Drifts, Drift{
			Domain: DomainGrant, Subject: SubjectGrant, Name: grantName(g),
			Kind: DriftNonAdditive, Action: ActionReport,
			Detail: fmt.Sprintf("Postgres holds grant %s beyond the ledger bounds; a stray grant is non-additive, reported, never silently fixed", grantName(g)),
		})
	}
	return report
}

// grantName renders a grant's human identity for a drift name, e.g.
// "SELECT on public.orders to reader".
func grantName(g Grant) string {
	return fmt.Sprintf("%s on %s.%s to %s", g.Privilege, g.Schema, g.Object, g.Role)
}

// DataMode is a pipeline's data mode in meta: disposable or permanent. It
// governs wipe eligibility, never capture.
type DataMode string

// The two data modes.
const (
	// DataDisposable marks a pipeline's data wipe-eligible (dev/throwaway).
	DataDisposable DataMode = "disposable"
	// DataPermanent marks a pipeline's data permanent (post-promotion).
	DataPermanent DataMode = "permanent"
)

// UpstreamRead pairs a table a pipeline declares reads on with the data mode of
// the pipeline that writes it (from meta). It is the per-read input to the
// cross-mode check.
type UpstreamRead struct {
	// Table is the dotted schema.table the reader declares reads on.
	Table string
	// Mode is the writing pipeline's data mode.
	Mode DataMode
}

// WarningKind is the closed set of non-refusing advisories apply can surface.
type WarningKind string

// The warning kinds.
const (
	// WarnCrossModeRead is raised when a permanent-data pipeline declares reads on
	// a disposable-mode upstream table: legitimate mid-promotion, so apply warns and
	// never refuses.
	WarnCrossModeRead WarningKind = "cross_mode_read"
)

// Warning is one non-refusing advisory apply surfaces, and carries in its --json
// output. A warning never blocks apply; it is guidance, distinct from a refusing
// Drift. Its JSON tags are the shape the apply envelope's warnings array takes.
type Warning struct {
	// Kind is the closed warning kind.
	Kind WarningKind `json:"kind"`
	// Table is the table the warning concerns, when applicable.
	Table string `json:"table,omitempty"`
	// Message is the human-readable advisory.
	Message string `json:"message"`
}

// CheckCrossModeReads returns a warning for each disposable-mode upstream table a
// permanent-data reader declares reads on: the legitimate mid-promotion state
// where a promoted pipeline consumes an as-yet-disposable upstream.
// It warns, never refuses -- the return is warnings only, with no error
// path -- and a disposable reader or a permanent upstream yields nothing. promote
// repeats the same warning while the upstream stays disposable.
func CheckCrossModeReads(reader DataMode, upstreams []UpstreamRead) []Warning {
	if reader != DataPermanent {
		return nil
	}
	var warns []Warning
	for _, u := range upstreams {
		if u.Mode != DataDisposable {
			continue
		}
		warns = append(warns, Warning{
			Kind:  WarnCrossModeRead,
			Table: u.Table,
			Message: fmt.Sprintf(
				"permanent-data pipeline reads disposable-mode table %s; apply proceeds (legitimate mid-promotion), and promote repeats this warning while the upstream stays disposable",
				u.Table),
		})
	}
	return warns
}
