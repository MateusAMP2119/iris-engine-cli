CREATE TABLE IF NOT EXISTS public.data_journal (
    id bigint GENERATED ALWAYS AS IDENTITY,
    pg_role text NOT NULL,
    run_id bigint NOT NULL,
    "schema" text NOT NULL,
    "table" text NOT NULL,
    row_pk text NOT NULL,
    op text NOT NULL,
    pre_image json,
    undo text NOT NULL,
    recorded_at text NOT NULL,
    PRIMARY KEY (id),
    CHECK (op IN ('insert', 'update', 'delete')),
    CHECK (undo IN ('open', 'promoted', 'wiped', 'skipped'))
) PARTITION BY RANGE (id);

CREATE INDEX IF NOT EXISTS data_journal_provenance_idx ON public.data_journal ("schema", "table", row_pk, run_id);
