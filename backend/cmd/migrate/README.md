# Legacy account migration

`cmd/migrate` converts a JSON snapshot of legacy Neonix accounts for the Go
gateway. It is deliberately dry-run by default. The same command can apply the
verified rows to PostgreSQL in one transaction during the maintenance window.

```sh
export NEONIX_CREDENTIAL_KEY="$(openssl rand -hex 32)"
go run ./cmd/migrate --source accounts.json
go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json
go run ./cmd/migrate --source accounts.json --apply --dsn "$NEONIX_MIGRATION_DSN"
```

The command accepts either an array or `{ "accounts": [...] }`. Reports contain
counts and stable issue codes only; credential values are never printed.
`--output` writes a redacted review artifact: provider credentials are excluded.
`--dsn` performs an idempotent PostgreSQL import: existing rows are matched by
the legacy account marker and then provider plus email, while local names,
groups, proxies, status, scheduling state, and runtime metadata are preserved.
`NEONIX_MIGRATION_DSN` is used when `--dsn` is omitted.

Provider credentials remain in memory only until the importer writes the current
Sub2API-compatible `accounts.credentials` JSONB column, which the Go adapters
consume directly. The importer never writes provider credentials to
`account_credential_envelopes`; the final cutover migration
`252_drop_account_credential_envelopes.sql` removes that retired table. Do not
edit migration 238: deploy a bridge image that omits 252, complete and verify
the import, then deploy the final image containing 252. `NEONIX_CREDENTIAL_KEY`
is retained only to encrypt automation-only Grok and GitHub identity secrets.
