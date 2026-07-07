SELECT id, customer_id, amount, created_at
FROM analytics.orders
WHERE ($1::uuid IS NULL OR customer_id = $1::uuid)
  AND ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
  AND ($3::timestamptz IS NULL OR created_at <= $3::timestamptz)
  AND ($4::uuid IS NULL OR id > $4::uuid)
ORDER BY id ASC
LIMIT $5;
