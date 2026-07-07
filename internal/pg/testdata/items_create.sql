CREATE TABLE "catalog"."items" (
    "id"   uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    "name" text NOT NULL
);
