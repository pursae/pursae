-- Stock held for unpaid checkout orders (WooCommerce's wc_reserved_stock table, ReserveStock): placing
-- an order reserves its quantities for "Hold stock (minutes)", so two checkouts cannot both pay for the
-- last unit. One row per order and stock-owning product (the product, the parent managing a
-- variation's stock, or the canonical translation). A reservation counts only until expires_at; it is
-- dropped when the order is paid, put on hold, cancelled or failed (the stock is then reduced or given
-- back) and by the hourly job once expired. Rows are written under a lock on the product's
-- goo_products row (internal/server/goo_reserved_stock.go). Expand-only: earlier releases ignore it.
CREATE TABLE IF NOT EXISTS goo_reserved_stock (
	order_id       bigint      NOT NULL REFERENCES goo_orders(id) ON DELETE CASCADE,
	product_id     bigint      NOT NULL REFERENCES goo_products(post_id) ON DELETE CASCADE,
	stock_quantity integer     NOT NULL,
	created_at     timestamptz NOT NULL DEFAULT now(),
	expires_at     timestamptz NOT NULL,
	PRIMARY KEY (order_id, product_id)
);
CREATE INDEX IF NOT EXISTS goo_reserved_stock_product ON goo_reserved_stock (product_id, expires_at);
