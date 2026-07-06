CREATE TRIGGER "iris_capture_analytics_orders"
    AFTER INSERT OR UPDATE OR DELETE ON "analytics"."orders"
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();
