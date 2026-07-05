CREATE TABLE analytics.orders (
    id          uuid        PRIMARY KEY,
    customer_id uuid        NOT NULL,
    amount      numeric,
    created_at  timestamptz DEFAULT now()
);
