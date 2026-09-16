-- Server-owned credentials and reservation state for linking a stored GitHub
-- identity to a CodeBuddy account. Secrets stay outside accounts.credentials so
-- they can never be returned by the operator account APIs.
CREATE TABLE IF NOT EXISTS account_github_identity_secrets (
    account_id      BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    secret_envelope TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS github_codebuddy_links (
    id                   BIGSERIAL PRIMARY KEY,
    github_account_id    BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    codebuddy_account_id BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    job_id               VARCHAR(128),
    status               VARCHAR(16) NOT NULL DEFAULT 'linking'
        CHECK (status IN ('linking', 'active', 'failed', 'revoked')),
    last_error_code      VARCHAR(80),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS github_codebuddy_links_github_active_idx
    ON github_codebuddy_links (github_account_id)
    WHERE status IN ('linking', 'active');

CREATE UNIQUE INDEX IF NOT EXISTS github_codebuddy_links_codebuddy_active_idx
    ON github_codebuddy_links (codebuddy_account_id)
    WHERE codebuddy_account_id IS NOT NULL AND status IN ('linking', 'active');

CREATE INDEX IF NOT EXISTS github_codebuddy_links_job_idx
    ON github_codebuddy_links (job_id)
    WHERE job_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS account_github_identity_secrets_updated_at_idx
    ON account_github_identity_secrets (updated_at);
