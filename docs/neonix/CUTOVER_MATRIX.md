# Neonix Go cutover matrix

This matrix is the source of truth for route ownership while the migration
branch is reviewed. A row moves to **Go-owned** only after its contract,
provider behavior, persistence, observability, and rollback test pass on the
same commit.

| Capability | Current owner | Go status | Cutover gate |
| --- | --- | --- | --- |
| `/health`, `/ready` | Go | Go-owned | health/readiness tests |
| Operator auth aliases (`/api/health`, `/api/auth/*`) | Node compatibility | Go compatibility slice available | login/refresh/current-user smoke test and session-binding checks |
| `/v1/*` streaming gateway | Go/Sub2API baseline | Go-owned for migrated adapters | streaming, cancellation, pre-first-byte failover |
| Accounts CRUD/list/IDs/groups | Node compatibility + Go `/api` slice | Go compatibility available | persistence parity and UI smoke test |
| API keys (`/api/api-keys`) | Node compatibility | Go compatibility slice available; legacy ID, usage, and access-history import added | production dry-run, imported-count reconciliation, default-key bootstrap, create/delete, usage/access smoke test |
| Tray settings (`/api/settings`) | Node compatibility | Go compatibility slice available | flat JSON read/write and cache invalidation smoke test |
| Antigravity OAuth/gateway | Node + Go compatibility slice | Go provider slice available | live OAuth and Gemini/Claude request smoke tests |
| Codex device OAuth | Node + Go compatibility slice | Go provider slice available | device approval, refresh, auth.json import |
| Grok device OAuth | Node + Go compatibility slice | Go provider slice available | pending/slow-down/denied and refresh smoke tests |
| M365 provider OAuth | Node + Go compatibility slice | Go control-plane slice available | callback, `oid/tid`, reconnect fallback |
| Outlook mailbox OAuth/poll | Node + Go compatibility slice, Python IMAP worker | Go control-plane slice available | worker key, single-flight poll, OTP/link sanitization |
| Credential migration/envelope reads | Node export + Go importer | Go migration slice available | backup/restore, envelope key, provider smoke tests |
| OpenCode Zen catalogue/warmup | Go | Go-owned | live sync and retired-model smoke test |
| Proxy model catalog | Go | Go persistence plus list/create/update/delete and OpenCode sync available | catalog export/import reconciliation and remaining provider sync parity |
| Proxy panel/config/resilience | Go compatibility | config/status, usage-derived stats/logs, and database-derived resilience coverage available | persisted config and UI smoke test |
| Register and browser automation orchestration | Node + Python | Node-owned | job lifecycle and worker lease parity |
| Settings, dashboard, filters | Go compatibility | settings, usage analytics, and request filter runtime are Go-owned; chat and remaining admin pages are pending | web contract and visual regression parity |
| SaaS payment/subscription/member surfaces | Disabled | Production routes removed; runtime forced to simple single-operator mode | remove dormant services, generated schema, and frontend artifacts |
| Kiro/Qoder/CodeBuddy provider adapters | Node + Python | Node-owned | provider adapter feature slices |

The Go service must not proxy a request to Node to satisfy an unfinished row.
During a maintenance cutover, take a PostgreSQL/Redis backup, stop Node only
after every required row is Go-owned, and restore the previous image and backup
if any provider smoke test fails.
