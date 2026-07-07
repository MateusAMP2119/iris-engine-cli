GRANT SELECT ("id") ON "analytics"."orders" TO "iris_load_orders";

GRANT SELECT ("amount") ON "analytics"."orders" TO "iris_load_orders";

GRANT INSERT ("id"), UPDATE ("id") ON "raw"."orders_staging" TO "iris_load_orders";
