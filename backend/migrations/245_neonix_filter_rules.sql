CREATE TABLE IF NOT EXISTS filter_rules (
    id          BIGSERIAL PRIMARY KEY,
    rule_id     VARCHAR(160) NOT NULL UNIQUE,
    pattern     TEXT NOT NULL,
    replacement TEXT NOT NULL DEFAULT '',
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    is_regex    BOOLEAN NOT NULL DEFAULT FALSE,
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_filter_rules_runtime
    ON filter_rules (is_active, sort_order, id);
