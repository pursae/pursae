-- Hosted payment attempts of the provider bridge (goo_gateway_pay.go): one row per time a customer is
-- sent to a payment provider. ref is the reference the provider echoes back in its callbacks.
CREATE TABLE IF NOT EXISTS goo_payment_attempts (
	id               bigserial PRIMARY KEY,
	order_id         bigint NOT NULL,
	gateway          text NOT NULL,
	ref              text NOT NULL UNIQUE,
	provider_ref     text NOT NULL DEFAULT '',
	amount_minor     bigint NOT NULL,
	currency         text NOT NULL,
	installment      integer NOT NULL DEFAULT 0,
	test             boolean NOT NULL DEFAULT false,
	status           text NOT NULL DEFAULT 'started', -- started | paid | failed | expired
	txn_id           text NOT NULL DEFAULT '',
	paid_minor       bigint NOT NULL DEFAULT 0,
	refunded_minor   bigint NOT NULL DEFAULT 0,
	code             text NOT NULL DEFAULT '',
	provider_code    text NOT NULL DEFAULT '',
	provider_message text NOT NULL DEFAULT '',
	start            jsonb, -- the form / iframe to render; cleared when the attempt ends
	created_at       timestamptz NOT NULL DEFAULT now(),
	updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS goo_payment_attempts_order ON goo_payment_attempts (order_id);
CREATE INDEX IF NOT EXISTS goo_payment_attempts_status ON goo_payment_attempts (status, created_at);
