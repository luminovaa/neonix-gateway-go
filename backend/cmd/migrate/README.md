# Legacy account migration

`cmd/migrate` converts a JSON snapshot of legacy Neonix accounts into a versioned AES-256-GCM credential envelope. It is deliberately dry-run by default. The same command can apply the verified rows to PostgreSQL in one transaction during the maintenance window.

```sh
export NEONIX_CREDENTIAL_KEY="$(openssl rand -hex 32)"
go run ./cmd/migrate --source accounts.json
go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json
go run ./cmd/migrate --source accounts.json --apply --dsn "$NEONIX_MIGRATION_DSN"
```

The command accepts either an array or `{ "accounts": [...] }`. Reports contain counts and stable issue codes only; credential values are never printed. `--output` keeps a restricted encrypted artifact for review. `--dsn` performs an idempotent PostgreSQL import: existing rows are matched by the legacy account marker and then provider plus email, while local names, groups, proxies, status, scheduling state, and runtime metadata are preserved. `NEONIX_MIGRATION_DSN` is used when `--dsn` is omitted.

The importer decrypts each envelope only in process memory to populate the current Sub2API-compatible `accounts.credentials` JSONB column, because the active Go adapters still consume that compatibility column. The envelope remains the reviewable handoff artifact; a later credential-store slice will move runtime reads to encrypted storage before the legacy column is removed.
