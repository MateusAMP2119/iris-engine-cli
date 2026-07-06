package pg

import "fmt"

// RenderCaptureTrigger renders the CREATE TRIGGER statement that installs the
// engine's always-on write-capture trigger on a declared user table
// (specification sections 3 and 5): a per-statement AFTER trigger firing on every
// INSERT, UPDATE, or DELETE into the engine's capture function.
//
// This renders only the CREATE TRIGGER binding -- the seam by which migration sync
// auto-fixes a missing capture trigger additively, like a missing column. The
// capture function's PL/pgSQL body (iris.capture(), which writes the provenance
// rows into public.data_journal) is owned and emitted by E06.2; here the trigger
// references it by its stable engine-owned name. The trigger name embeds the
// (schema, table) it guards so it is unique per declared table, and every
// user-supplied identifier is double-quoted, consistent with the rest of pg's DDL
// rendering. The output is deterministic, so a golden diff is a contract diff.
func RenderCaptureTrigger(schema, table string) string {
	name := fmt.Sprintf("iris_capture_%s_%s", schema, table)
	return fmt.Sprintf(
		"CREATE TRIGGER %s\n"+
			"    AFTER INSERT OR UPDATE OR DELETE ON %s.%s\n"+
			"    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();",
		quoteIdentifier(name), quoteIdentifier(schema), quoteIdentifier(table))
}
