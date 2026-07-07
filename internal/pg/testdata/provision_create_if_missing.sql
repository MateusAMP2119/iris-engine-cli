CREATE SCHEMA IF NOT EXISTS "analytics";

CREATE SCHEMA IF NOT EXISTS "raw";

CREATE TABLE "analytics"."orders" (
    "id"          uuid        PRIMARY KEY,
    "customer_id" uuid        NOT NULL,
    "amount"      numeric,
    "created_at"  timestamptz DEFAULT now()
);

CREATE TABLE "raw"."orders_staging" (
    "id"          uuid        PRIMARY KEY,
    "customer_id" uuid        NOT NULL,
    "amount"      numeric,
    "created_at"  timestamptz DEFAULT now()
);

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

DROP TRIGGER IF EXISTS "iris_capture_ins_analytics_orders" ON "analytics"."orders";
CREATE TRIGGER "iris_capture_ins_analytics_orders"
    AFTER INSERT ON "analytics"."orders"
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();

DROP TRIGGER IF EXISTS "iris_capture_upd_analytics_orders" ON "analytics"."orders";
CREATE TRIGGER "iris_capture_upd_analytics_orders"
    AFTER UPDATE ON "analytics"."orders"
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();

DROP TRIGGER IF EXISTS "iris_capture_del_analytics_orders" ON "analytics"."orders";
CREATE TRIGGER "iris_capture_del_analytics_orders"
    AFTER DELETE ON "analytics"."orders"
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();

DROP TRIGGER IF EXISTS "iris_capture_ins_raw_orders_staging" ON "raw"."orders_staging";
CREATE TRIGGER "iris_capture_ins_raw_orders_staging"
    AFTER INSERT ON "raw"."orders_staging"
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();

DROP TRIGGER IF EXISTS "iris_capture_upd_raw_orders_staging" ON "raw"."orders_staging";
CREATE TRIGGER "iris_capture_upd_raw_orders_staging"
    AFTER UPDATE ON "raw"."orders_staging"
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();

DROP TRIGGER IF EXISTS "iris_capture_del_raw_orders_staging" ON "raw"."orders_staging";
CREATE TRIGGER "iris_capture_del_raw_orders_staging"
    AFTER DELETE ON "raw"."orders_staging"
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();
