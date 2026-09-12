# Neonix Go migration runbook

The Go backend is the production target. The Next.js Neonix application remains the product UI, and Python automation remains a separate worker for browser and mailbox tasks.

## Credential migration

1. Export a consistent account snapshot from the current PostgreSQL database. Do not print the export or put it in a ticket.
2. Set `NEONIX_CREDENTIAL_KEY` to the 32-byte key that will be used by the Go deployment. Keep it in the deployment secret store.
3. Run `go run ./cmd/migrate --source accounts.json` from `backend/`. The default is a dry run. It emits only counts and redacted issue codes.
4. Resolve every `blocked` issue. Migration must not proceed with an active account missing credentials or with duplicate IDs.
5. Run `go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json` and verify the output file permissions are `0600` and its hash is recorded in the cutover checklist.
6. Start the Go binary once (or apply migration `238_account_credential_envelopes.sql`) so the encrypted handoff table exists, then during the maintenance window run `go run ./cmd/migrate --source accounts.json --apply --dsn "$NEONIX_MIGRATION_DSN"`. The importer opens one PostgreSQL transaction, matches by legacy ID (email fallback), updates credentials only on existing rows, writes the encrypted envelope, and rolls back every row if any write fails. It reports `created`/`updated` counts without credential values. Start the Go service with the same `NEONIX_CREDENTIAL_KEY`; migrated account reads then decrypt the envelope before an adapter sees credentials. Keep the original snapshot read-only until smoke tests and rollback checks pass.

The converter preserves provider credential bytes before encryption. Deprecated `bai`, `bb`, and `codebuff` records are reported as skipped and are not copied into the active Go store. No access, refresh, or API token is included in reports or command errors.

## Current compatibility slice

The migration branch exposes the operator-facing `/api` compatibility surface
behind the admin guard. Account CRUD, provider registry/coverage, readiness,
Codex and Grok device login, M365 callback-paste OAuth, and Antigravity OAuth
callback-paste login are available there; the middleware
unwraps the Go response envelope so the existing Neonix transport can use the
same direct JSON shape. The canonical Sub2API routes remain under `/api/v1`.

The same boundary now exposes `/api/api-keys`. It bootstraps one `default` key
for the operator when none exists, returns the legacy camelCase array and key
object shapes, and delegates key mutations to the Go API-key service. Usage
aggregates and access fingerprints are read from the Go usage-log store; the
raw key is returned only to the authenticated operator endpoint that needs it
for client configuration.

Antigravity OAuth sessions use the existing Go PKCE service. The callback must
be the exact `http://localhost:8080/callback` URL returned by `start`; the
complete endpoint upserts by email and never returns credential values. A
missing project identifier is persisted with a warning so the account can be
checked again later.

Mailbox routes are admin-only and keep Python as the IMAP/browser boundary.
`/api/mailbox/oauth/*` creates or reconnects an Outlook mailbox account using
Microsoft PKCE and `IMAP.AccessAsUser.All`; `/api/mailbox/poll` sends only the
selected account's email, client ID, refresh token, and bounded filters to the
Python worker's `/api/mailbox/poll` route. Go rejects overlapping polls per
mailbox, maps worker failures to stable codes, and strips non-HTTPS result URLs.

The legacy Accounts `check` and `warmup` actions are available as bounded Go
usage probes. They return the sanitized account snapshot and usage result; a
provider-specific streaming test remains part of the provider-adapter slices.

The compatibility surface is a staged cutover boundary. Remaining provider-
specific account adapters and full control-plane route parity still need their
own reviewed slices before the Node backend is removed.

## Cutover gates

- `go test ./...` passes on the exact commit deployed.
- A dry-run report has zero blocked active records.
- PostgreSQL and Redis backups are available.
- `/health` and `/ready` are green after starting the Go service.
- One request succeeds for each enabled provider, including a streaming request and cancellation.
- The Node backend is stopped only after these checks pass. Python worker processes remain independently managed.
