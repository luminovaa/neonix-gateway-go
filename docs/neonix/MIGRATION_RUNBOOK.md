# Neonix Go migration runbook

The Go backend is the production target. The Next.js Neonix application remains the product UI, and Python automation remains a separate worker for browser and mailbox tasks.

## Credential migration

1. Export a consistent account snapshot from the current PostgreSQL database. Do not print the export or put it in a ticket.
2. Set `NEONIX_CREDENTIAL_KEY` to the 32-byte key that will be used by the Go deployment. Keep it in the deployment secret store.
3. Run `go run ./cmd/migrate --source accounts.json` from `backend/`. The default is a dry run. It emits only counts and redacted issue codes.
4. Resolve every `blocked` issue. Migration must not proceed with an active account missing credentials or with duplicate IDs.
5. Run `go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json` and verify the output file permissions are `0600` and its hash is recorded in the cutover checklist.
6. During the maintenance window, run `go run ./cmd/migrate --source accounts.json --apply --dsn "$NEONIX_MIGRATION_DSN"`. The importer opens one PostgreSQL transaction, matches by legacy ID (email fallback), updates credentials only on existing rows, and rolls back every row if any write fails. It reports `created`/`updated` counts without credential values. Keep the original snapshot read-only until smoke tests and rollback checks pass.

The converter preserves provider credential bytes before encryption. Deprecated `bai`, `bb`, and `codebuff` records are reported as skipped and are not copied into the active Go store. No access, refresh, or API token is included in reports or command errors.

## Current compatibility slice

The migration branch exposes the operator-facing `/api` compatibility surface
behind the admin guard. Account CRUD, provider registry/coverage, readiness,
and Antigravity OAuth callback-paste login are available there; the middleware
unwraps the Go response envelope so the existing Neonix transport can use the
same direct JSON shape. The canonical Sub2API routes remain under `/api/v1`.

Antigravity OAuth sessions use the existing Go PKCE service. The callback must
be the exact `http://localhost:8080/callback` URL returned by `start`; the
complete endpoint upserts by email and never returns credential values. A
missing project identifier is persisted with a warning so the account can be
checked again later.

The compatibility surface is a staged cutover boundary. Codex, Grok, M365,
remaining provider-specific account adapters, and encrypted runtime credential
reads still need their own reviewed slices before the Node backend is removed.

## Cutover gates

- `go test ./...` passes on the exact commit deployed.
- A dry-run report has zero blocked active records.
- PostgreSQL and Redis backups are available.
- `/health` and `/ready` are green after starting the Go service.
- One request succeeds for each enabled provider, including a streaming request and cancellation.
- The Node backend is stopped only after these checks pass. Python worker processes remain independently managed.
