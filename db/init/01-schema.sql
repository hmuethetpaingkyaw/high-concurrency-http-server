-- Runs once, when the Postgres volume is first created.
-- Reset with `make db-reset` to re-seed.

CREATE TABLE products (
    id          int  PRIMARY KEY,
    name        text NOT NULL,
    category    text NOT NULL,
    price_cents int  NOT NULL,
    description text NOT NULL
);
CREATE INDEX products_category_id_idx ON products (category, id);

-- Stock lives in its own table so the list endpoint has something to N+1 on.
CREATE TABLE inventory (
    product_id int PRIMARY KEY REFERENCES products (id),
    stock      int NOT NULL
);

CREATE TABLE orders (
    id         bigserial   PRIMARY KEY,
    product_id int         NOT NULL REFERENCES products (id),
    qty        int         NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 100,000 products across 50 categories (~2,000 each), ~240-byte descriptions.
INSERT INTO products (id, name, category, price_cents, description)
SELECT g,
       'Product ' || g,
       'cat-' || (g % 50 + 1),
       100 + (g * 37) % 10000,
       repeat('Lorem ipsum ', 20)
FROM generate_series(1, 100000) AS g;

-- Plenty of stock so order tests never run out.
INSERT INTO inventory (product_id, stock)
SELECT g, 1000000 FROM generate_series(1, 100000) AS g;

ANALYZE;
