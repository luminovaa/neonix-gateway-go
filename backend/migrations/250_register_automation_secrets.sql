-- Backend-only credentials used by maintenance automation. These values are
-- deliberately isolated from accounts.credentials so provider adapters and
-- operator API responses cannot expose a replayable login password.
CREATE TABLE IF NOT EXISTS account_register_automation_secrets (
    account_id        BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    password_envelope TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS account_register_automation_secrets_updated_at_idx
    ON account_register_automation_secrets (updated_at);
