-- +goose Up
-- ADR-022: the charge snapshot. All-or-none enforced by CHECK:
-- converted orders carry ALL five fields; base-currency checkouts carry none.
ALTER TABLE orders
    ADD COLUMN charge_currency         TEXT NULL,
    ADD COLUMN charge_amount_cents     BIGINT NULL CHECK (charge_amount_cents > 0),
    ADD COLUMN exchange_rate_used      TEXT NULL,
    ADD COLUMN exchange_rate_source    TEXT NULL,
    ADD COLUMN exchange_rate_fetched_at TIMESTAMPTZ NULL;

ALTER TABLE orders
    ADD CONSTRAINT order_charge_snapshot_all_or_none CHECK (
        (charge_amount_cents IS NULL)
        = (charge_currency IS NULL AND exchange_rate_used IS NULL
           AND exchange_rate_source IS NULL AND exchange_rate_fetched_at IS NULL)
    );

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT IF EXISTS order_charge_snapshot_all_or_none;
ALTER TABLE orders
    DROP COLUMN IF EXISTS exchange_rate_fetched_at,
    DROP COLUMN IF EXISTS exchange_rate_source,
    DROP COLUMN IF EXISTS exchange_rate_used,
    DROP COLUMN IF EXISTS charge_amount_cents,
    DROP COLUMN IF EXISTS charge_currency;
