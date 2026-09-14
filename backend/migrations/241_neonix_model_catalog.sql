CREATE TABLE IF NOT EXISTS neonix_model_catalog (
    model_id          TEXT PRIMARY KEY,
    model_name        TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    provider          TEXT NOT NULL,
    source            TEXT NOT NULL,
    requires_pro      BOOLEAN NOT NULL DEFAULT FALSE,
    status            TEXT NOT NULL DEFAULT 'AVAILABLE',
    raw_data          JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by_admin  BOOLEAN NOT NULL DEFAULT FALSE,
    is_deleted        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_neonix_model_catalog_provider
    ON neonix_model_catalog(provider, is_deleted);
