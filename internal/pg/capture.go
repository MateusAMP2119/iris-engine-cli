package pg

// This file owns the engine's always-on write-capture function, iris.capture(),
// and the schema that hosts it (specification section 4: data_journal capture).
// The per-operation CREATE TRIGGER bindings that reference it live in trigger.go;
// this is the function body they bind to -- the PL/pgSQL that reads the statement's
// transition tables and writes one provenance stamp per changed row into
// public.data_journal.
//
// pg owns the data database, so it owns this DDL. EnsureCaptureFunction (live.go)
// applies it create-or-replace before every provisioning apply, so a dropped
// function self-heals; the emitted text is deterministic, so a golden diff is a
// contract diff.

// CaptureSchema is the engine-owned schema that hosts the capture function. It is
// distinct from the user schemas/ tree and from public (which hosts the journal),
// so the function name never collides with a declared object.
const CaptureSchema = "iris"

// CaptureFunctionName is the schema-qualified capture function the per-table
// triggers bind to (see trigger.go). It is engine-owned and stable.
const CaptureFunctionName = "iris.capture"

// CaptureSchemaDDL renders the create-if-missing DDL for the iris schema that hosts
// the capture function. It is idempotent, applied before the function so the
// function's schema always exists.
func CaptureSchemaDDL() string {
	return "CREATE SCHEMA IF NOT EXISTS " + CaptureSchema + ";"
}

// CaptureFunctionDDL renders the always-on write-capture function iris.capture()
// (specification section 4). It is a statement-level trigger function: bound FOR
// EACH STATEMENT with transition tables (trigger.go), it fires once per write
// statement and issues exactly one INSERT...SELECT into public.data_journal over
// the statement's transition table, so a 10M-row load stamps in one insert, not
// 10M. The hot write path only inserts a stamp: nothing is partitioned, sealed, or
// archived here (that is all downstream, E07).
//
// Per changed row it stamps: the writing role (session_user), the run id, the
// (schema, table) it guards, the row's primary key as text (row_pk), the operation
// (insert/update/delete), the pre-image, undo='open', and an opaque recorded_at
// audit string. There is no post-image.
//
//   - Attribution. run_id rides the injected connection as the per-session setting
//     iris.run_id, read here in-transaction with current_setting. E06.3 sets it on
//     the connection Iris injects; this function only reads it. data_journal.run_id
//     is NOT NULL, so a write with no run in session cannot be stamped and the write
//     fails -- no row is ever keyed to a role without a run.
//   - Writer identity. The function is SECURITY DEFINER so the INSERT runs as the
//     journal's owner: the journal grants no INSERT to anyone (only SELECT TO
//     PUBLIC), so a pipeline role's write can only reach the journal through the
//     owner-run trigger. Because the definer context masks current_user, the writing
//     role is read from session_user (the connection's login role, unaffected by the
//     definer boundary or a SET ROLE), which is the role Iris injected. search_path
//     is pinned so the definer function cannot be hijacked by a caller's search_path.
//   - pre_image. The full prior row (to_json) on a wipe-eligible update or delete;
//     null on an insert. The undo-eligibility / born-promoted refinement that nulls
//     it in the common case is downstream (E06.4/E06.6); here every entry is born
//     undo='open', so it correctly carries its pre-image.
//   - row_pk. Resolved from the firing table's primary key, in key order, as the
//     text of the key column(s). A single-column key renders its bare value (the
//     provenance key data_journal indexes on); a composite key joins with '|'.
func CaptureFunctionDDL() string {
	return `CREATE OR REPLACE FUNCTION iris.capture() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $iris_capture$
DECLARE
    v_run_id  bigint := current_setting('iris.run_id')::bigint;
    v_pk_expr text;
    v_source  text;
    v_op      text;
    v_pre     text;
BEGIN
    -- Resolve the firing table's primary-key columns, in key order, into a
    -- concat_ws argument list over the transition-table row alias r. row_pk is the
    -- text of that key: a single-column key renders its bare value.
    SELECT string_agg(format('r.%I::text', a.attname), ', ' ORDER BY k.ord)
      INTO v_pk_expr
    FROM pg_catalog.pg_index i
    CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
    JOIN pg_catalog.pg_attribute a
      ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    WHERE i.indrelid = TG_RELID AND i.indisprimary;

    IF v_pk_expr IS NULL THEN
        RAISE EXCEPTION 'iris.capture: table %.% has no primary key; capture needs one for row_pk',
            TG_TABLE_SCHEMA, TG_TABLE_NAME;
    END IF;

    -- Insert reads the after image (new_rows) and carries no pre-image; update and
    -- delete read the before image (old_rows) and carry the full pre-image.
    IF TG_OP = 'INSERT' THEN
        v_source := 'new_rows'; v_op := 'insert'; v_pre := 'NULL';
    ELSIF TG_OP = 'UPDATE' THEN
        v_source := 'old_rows'; v_op := 'update'; v_pre := 'to_json(r)';
    ELSE
        v_source := 'old_rows'; v_op := 'delete'; v_pre := 'to_json(r)';
    END IF;

    -- One INSERT...SELECT per fired statement: the transition table holds every row
    -- the statement changed, so the whole write set is stamped in a single insert.
    EXECUTE format(
        'INSERT INTO public.data_journal '
        '(pg_role, run_id, "schema", "table", row_pk, op, pre_image, undo, recorded_at) '
        'SELECT session_user, $1, $2, $3, concat_ws(''|'', %s), $4, %s, ''open'', $5 '
        'FROM %s AS r',
        v_pk_expr, v_pre, v_source)
    USING v_run_id, TG_TABLE_SCHEMA, TG_TABLE_NAME, v_op, clock_timestamp()::text;

    RETURN NULL;
END;
$iris_capture$;`
}
