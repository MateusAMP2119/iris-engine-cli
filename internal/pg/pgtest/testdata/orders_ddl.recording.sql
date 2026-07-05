CREATE TABLE analytics.orders (
    id          uuid        PRIMARY KEY,
    customer_id uuid        NOT NULL,
    amount      numeric,
    created_at  timestamptz DEFAULT now()
);

ALTER TABLE analytics.orders ADD COLUMN status text DEFAULT 'pending';

GRANT SELECT ON analytics.orders TO iris_read_analytics_orders;

CREATE TRIGGER iris_capture_analytics_orders
    AFTER INSERT OR UPDATE OR DELETE ON analytics.orders
    FOR EACH STATEMENT EXECUTE FUNCTION iris.capture();
