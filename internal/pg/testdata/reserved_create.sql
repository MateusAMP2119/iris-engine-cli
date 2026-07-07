CREATE TABLE "order"."select" (
    "user"    uuid PRIMARY KEY,
    "group"   text NOT NULL,
    "default" text DEFAULT 'x' UNIQUE
);
