CREATE TABLE shop.widgets (
    id    uuid          PRIMARY KEY,
    sku   varchar(32)   NOT NULL UNIQUE,
    price numeric(10,2) DEFAULT 0,
    label text          NOT NULL DEFAULT 'unnamed' UNIQUE,
    notes text
);
