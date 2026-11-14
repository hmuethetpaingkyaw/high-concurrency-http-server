-- Level 7: idempotency key for POST /orders.
-- For a database that already exists (fresh databases get this from
-- db/init/01-schema.sql). Safe to run more than once: `make migrate`.

ALTER TABLE orders ADD COLUMN IF NOT EXISTS idempotency_key text;

-- A unique index allows many NULLs, so orders without a key never conflict.
CREATE UNIQUE INDEX IF NOT EXISTS orders_idempotency_key_idx ON orders (idempotency_key);
