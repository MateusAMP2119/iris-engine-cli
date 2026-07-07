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
