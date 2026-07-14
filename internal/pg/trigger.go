package pg

import "fmt"

// RenderCaptureTriggers renders the CREATE TRIGGER statements that install the
// engine's always-on write-capture triggers on a declared user table:
// statement-level triggers with transition tables, one INSERT...SELECT per
// statement -- a 10M-row load fires one trigger, not 10M.
//
// The set is three triggers, not one, because Postgres transition tables are
// per-operation: an AFTER STATEMENT trigger may reference a NEW TABLE only for
// INSERT, an OLD TABLE only for DELETE, and both for UPDATE. A single combined
// INSERT/UPDATE/DELETE trigger cannot carry a REFERENCING clause at all, so the
// capture function would see no changed rows. The three triggers are therefore:
//   - INSERT: REFERENCING NEW TABLE AS new_rows
//   - UPDATE: REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
//   - DELETE: REFERENCING OLD TABLE AS old_rows
//
// This renders only the CREATE TRIGGER bindings -- the seam by which migration sync
// auto-fixes a missing capture trigger additively, like a missing column (a live
// table missing any of the three is the missing-trigger condition, repaired by
// installing the full set). The capture function's PL/pgSQL body (iris.capture(),
// which reads the transition tables and writes the provenance rows into
// public.data_journal) is owned and emitted by CaptureFunctionDDL in capture.go,
// and installed by the live client's EnsureCaptureFunction; here the triggers
// reference it by its stable engine-owned name. Each trigger name embeds the
// operation and the (schema, table) it guards so it is unique per table, and every
// user-supplied identifier is double-quoted, consistent with the rest of pg's DDL
// rendering. The output is deterministic, so a golden diff is a contract diff.
func RenderCaptureTriggers(schema, table string) []string {
	return []string{
		renderCaptureTrigger(schema, table, "ins", "INSERT", "NEW TABLE AS new_rows"),
		renderCaptureTrigger(schema, table, "upd", "UPDATE", "OLD TABLE AS old_rows NEW TABLE AS new_rows"),
		renderCaptureTrigger(schema, table, "del", "DELETE", "OLD TABLE AS old_rows"),
	}
}

// renderCaptureTrigger renders one per-operation capture trigger: op is the SQL
// event (INSERT/UPDATE/DELETE), tag the short operation tag embedded in the trigger
// name, and referencing the transition-table clause for that operation.
//
// The statement is a DROP TRIGGER IF EXISTS ... ON ...; CREATE TRIGGER ... two-step
// so it is idempotent on every Postgres version (CREATE OR REPLACE TRIGGER is 14+
// only). A partial prior provisioning run may leave one or more of the three
// triggers installed; a plain CREATE TRIGGER re-apply then fails "trigger already
// exists" and makes no progress. The leading drop is safe: a trigger holds no state
// of its own -- the capture behaviour lives in iris.capture() (capture.go) and in
// the journal rows it has already written -- so dropping the binding and re-creating
// it identically loses nothing. Both statements carry no bound arguments, so a
// no-argument Exec runs them over the simple query protocol, which executes the two
// commands in one round trip.
func renderCaptureTrigger(schema, table, tag, op, referencing string) string {
	name := fmt.Sprintf("iris_capture_%s_%s_%s", tag, schema, table)
	return fmt.Sprintf(
		"DROP TRIGGER IF EXISTS %s ON %s.%s;\n"+
			"CREATE TRIGGER %s\n"+
			"    AFTER %s ON %s.%s\n"+
			"    REFERENCING %s\n"+
			"    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();",
		quoteIdentifier(name), quoteIdentifier(schema), quoteIdentifier(table),
		quoteIdentifier(name), op, quoteIdentifier(schema), quoteIdentifier(table), referencing)
}
