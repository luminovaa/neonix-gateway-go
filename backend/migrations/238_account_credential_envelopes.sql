-- Encrypted provider credential handoff for the Go migration.
-- The compatibility accounts.credentials column remains populated while
-- adapters are migrated; this table is the durable source for the encrypted
-- credential-store slice and is keyed by the local account identity.
CREATE TABLE IF NOT EXISTS account_credential_envelopes (
    account_id  BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    envelope    TEXT NOT NULL,
    key_version SMALLINT NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS account_credential_envelopes_updated_at_idx
    ON account_credential_envelopes (updated_at);
