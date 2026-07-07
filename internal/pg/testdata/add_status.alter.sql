ALTER TABLE "analytics"."orders" ADD COLUMN IF NOT EXISTS "status" text DEFAULT 'pending';
