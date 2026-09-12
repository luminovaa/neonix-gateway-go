-- Preserve legacy Neonix API-key usage and access history without fabricating
-- provider account references in the canonical Go usage_logs table. These
-- append-only compatibility tables are read alongside native Go usage rows.
CREATE TABLE IF NOT EXISTS neonix_legacy_api_key_usage (
    legacy_id      BIGINT PRIMARY KEY,
    api_key_id     BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    occurred_at    TIMESTAMPTZ NOT NULL,
    model          TEXT NOT NULL DEFAULT '',
    input_tokens   BIGINT NOT NULL DEFAULT 0,
    output_tokens  BIGINT NOT NULL DEFAULT 0,
    credits        DOUBLE PRECISION NOT NULL DEFAULT 0,
    success        BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS idx_neonix_legacy_api_key_usage_key_time
    ON neonix_legacy_api_key_usage(api_key_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS neonix_legacy_api_key_access_logs (
    legacy_id      BIGINT PRIMARY KEY,
    api_key_id     BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    ip_address     TEXT NOT NULL,
    user_agent     TEXT,
    accessed_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_neonix_legacy_api_key_access_key_time
    ON neonix_legacy_api_key_access_logs(api_key_id, accessed_at DESC);
