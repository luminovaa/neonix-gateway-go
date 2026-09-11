# Legacy account migration

`cmd/migrate` converts a JSON snapshot of legacy Neonix accounts into a versioned AES-256-GCM credential envelope. It is deliberately dry-run by default.

```sh
export NEONIX_CREDENTIAL_KEY="$(openssl rand -hex 32)"
go run ./cmd/migrate --source accounts.json
go run ./cmd/migrate --source accounts.json --apply --output normalized-accounts.json
```

The command accepts either an array or `{ "accounts": [...] }`. Reports contain counts and stable issue codes only; credential values are never printed. The output is an import artifact for the Go persistence layer, not a replacement for the PostgreSQL transaction used during cutover.
