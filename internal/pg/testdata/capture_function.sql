CREATE OR REPLACE FUNCTION iris.capture() RETURNS trigger
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
$iris_capture$;
