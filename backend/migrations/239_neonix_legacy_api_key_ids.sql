-- Preserve the text/UUID identifiers used by the legacy Neonix Node API-key
-- table while the Go schema uses BIGSERIAL ids. The mapping is metadata only;
-- the secret key remains in api_keys and is never copied into this table.
CREATE TABLE IF NOT EXISTS neonix_legacy_api_key_ids (
    legacy_id      TEXT PRIMARY KEY,
    api_key_id     BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    legacy_user_id TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (api_key_id)
);

CREATE INDEX IF NOT EXISTS idx_neonix_legacy_api_key_ids_api_key_id
    ON neonix_legacy_api_key_ids(api_key_id);
