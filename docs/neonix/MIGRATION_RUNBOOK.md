# Neonix Go migration runbook

The Go backend is the production target. The Next.js Neonix application remains the product UI, and Python automation remains a separate worker for browser and mailbox tasks.

## Credential migration

1. Export a consistent account snapshot from the current PostgreSQL database. Do not print the export or put it in a ticket.
2. Set `NEONIX_CREDENTIAL_KEY` to the 32-byte key that will be used by the Go deployment. Keep it in the deployment secret store.
3. Run `go run ./cmd/migrate --source accounts.json` from `backend/`. The default is a dry run. It emits only counts and redacted issue codes.
4. Resolve every `blocked` issue. Migration must not proceed with an active account missing credentials or with duplicate IDs.
5. Run `go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json` and verify the output file permissions are `0600` and its hash is recorded in the cutover checklist.
6. Import the normalized rows inside a PostgreSQL transaction using the Go account repository. Keep the original snapshot read-only until smoke tests and rollback checks pass.

The converter preserves provider credential bytes before encryption. Deprecated `bai`, `bb`, and `codebuff` records are reported as skipped and are not copied into the active Go store. No access, refresh, or API token is included in reports or command errors.

## Cutover gates

- `go test ./...` passes on the exact commit deployed.
- A dry-run report has zero blocked active records.
- PostgreSQL and Redis backups are available.
- `/health` and `/ready` are green after starting the Go service.
- One request succeeds for each enabled provider, including a streaming request and cancellation.
- The Node backend is stopped only after these checks pass. Python worker processes remain independently managed.
