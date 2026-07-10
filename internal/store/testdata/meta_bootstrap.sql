CREATE DATABASE meta;

CREATE TABLE IF NOT EXISTS pipelines (
    name text,
    folder text NOT NULL,
    run json NOT NULL,
    artifact text NOT NULL,
    data_mode text NOT NULL,
    PRIMARY KEY (name),
    CHECK (artifact IN ('source', 'built')),
    CHECK (data_mode IN ('disposable', 'permanent'))
);

CREATE TABLE IF NOT EXISTS dependencies (
    from_pipeline text,
    to_pipeline text,
    PRIMARY KEY (from_pipeline, to_pipeline),
    FOREIGN KEY (from_pipeline) REFERENCES pipelines (name),
    FOREIGN KEY (to_pipeline) REFERENCES pipelines (name)
);

CREATE INDEX IF NOT EXISTS dependencies_to_pipeline_idx ON dependencies (to_pipeline);

CREATE TABLE IF NOT EXISTS lanes (
    lane text,
    pipeline text,
    pos bigint NOT NULL,
    PRIMARY KEY (lane, pipeline),
    UNIQUE (pipeline),
    UNIQUE (lane, pos)
);

CREATE TABLE IF NOT EXISTS artifacts (
    hash text,
    pipeline text NOT NULL,
    size_bytes bigint NOT NULL,
    recorded_at text NOT NULL,
    PRIMARY KEY (hash),
    FOREIGN KEY (pipeline) REFERENCES pipelines (name)
);

CREATE TABLE IF NOT EXISTS runs (
    id bigint GENERATED ALWAYS AS IDENTITY,
    pipeline text NOT NULL,
    state text NOT NULL,
    cause text NOT NULL,
    replayed_from bigint,
    exit_code integer,
    handle bigint,
    artifact_hash text,
    declaration_checksum text NOT NULL,
    log_ref text,
    snapshot_lsn text,
    journal_floor bigint,
    journal_ceiling bigint,
    recorded_at text NOT NULL,
    PRIMARY KEY (id),
    FOREIGN KEY (pipeline) REFERENCES pipelines (name),
    FOREIGN KEY (replayed_from) REFERENCES runs (id),
    FOREIGN KEY (artifact_hash) REFERENCES artifacts (hash),
    CHECK (state IN ('queued', 'running', 'succeeded', 'dead_lettered')),
    CHECK (cause IN ('manual', 'loop', 'replay', 'propagated'))
);

CREATE INDEX IF NOT EXISTS runs_pipeline_id_idx ON runs (pipeline, id);

CREATE TABLE IF NOT EXISTS run_inputs (
    run_id bigint,
    upstream_run_id bigint,
    PRIMARY KEY (run_id, upstream_run_id),
    FOREIGN KEY (run_id) REFERENCES runs (id)
);

CREATE INDEX IF NOT EXISTS run_inputs_upstream_run_id_idx ON run_inputs (upstream_run_id);

CREATE TABLE IF NOT EXISTS dead_letters (
    run_id bigint,
    reason text NOT NULL,
    error text,
    failed_upstream text,
    PRIMARY KEY (run_id),
    FOREIGN KEY (run_id) REFERENCES runs (id),
    FOREIGN KEY (failed_upstream) REFERENCES pipelines (name),
    CHECK (reason IN ('failed', 'stopped', 'upstream_dead_lettered'))
);

CREATE TABLE IF NOT EXISTS run_summaries (
    run_id bigint,
    pipeline text NOT NULL,
    state text NOT NULL,
    artifact_hash text,
    declaration_checksum text NOT NULL,
    consumed_upstream_run_ids json NOT NULL,
    snapshot_lsn text,
    journal_floor bigint,
    journal_ceiling bigint,
    recorded_at text NOT NULL,
    PRIMARY KEY (run_id)
);

CREATE TABLE IF NOT EXISTS journal_checkpoints (
    seq bigint GENERATED ALWAYS AS IDENTITY,
    id_from bigint NOT NULL,
    id_to bigint NOT NULL,
    digest bytea NOT NULL,
    parent_digest bytea,
    signature bytea NOT NULL,
    location text NOT NULL,
    recorded_at text NOT NULL,
    PRIMARY KEY (seq),
    CHECK (location IN ('resident', 'archived'))
);

CREATE TABLE IF NOT EXISTS engine_key (
    id bigint,
    private_key bytea NOT NULL,
    created_at text NOT NULL,
    PRIMARY KEY (id),
    CHECK (id = 1)
);

CREATE TABLE IF NOT EXISTS pats (
    id text,
    hash text NOT NULL,
    label text NOT NULL,
    revoked boolean NOT NULL,
    PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS pat_scopes (
    pat_id text,
    scope text,
    PRIMARY KEY (pat_id, scope),
    FOREIGN KEY (pat_id) REFERENCES pats (id),
    CHECK (scope IN ('control', 'read', 'data'))
);

CREATE TABLE IF NOT EXISTS endpoints (
    name text,
    source text NOT NULL,
    fields json NOT NULL,
    sort text NOT NULL,
    PRIMARY KEY (name)
);

CREATE TABLE IF NOT EXISTS endpoint_filters (
    endpoint text,
    param text,
    op text NOT NULL,
    PRIMARY KEY (endpoint, param),
    FOREIGN KEY (endpoint) REFERENCES endpoints (name),
    CHECK (op IN ('eq', 'range'))
);

CREATE TABLE IF NOT EXISTS roles (
    pg_role text,
    pipeline text,
    pat text,
    PRIMARY KEY (pg_role),
    FOREIGN KEY (pipeline) REFERENCES pipelines (name),
    FOREIGN KEY (pat) REFERENCES pats (id),
    CHECK ((pipeline IS NULL) <> (pat IS NULL))
);

CREATE TABLE IF NOT EXISTS grants (
    pg_role text,
    "schema" text,
    "table" text,
    field text,
    access text,
    PRIMARY KEY (pg_role, "schema", "table", field, access),
    FOREIGN KEY (pg_role) REFERENCES roles (pg_role)
);

CREATE TABLE IF NOT EXISTS credentials (
    pg_role text,
    secret text NOT NULL,
    PRIMARY KEY (pg_role),
    FOREIGN KEY (pg_role) REFERENCES roles (pg_role)
);

CREATE TABLE IF NOT EXISTS migrations (
    "schema" text,
    "table" text,
    migration_id text,
    parent text,
    checksum text NOT NULL,
    applied_seq bigint GENERATED ALWAYS AS IDENTITY,
    PRIMARY KEY ("schema", "table", migration_id)
);
