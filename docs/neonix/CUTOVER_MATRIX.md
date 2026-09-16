# Neonix Go cutover matrix

This matrix is the source of truth for production route ownership. The
full-Go Compose topology is now canonical; remaining gates are live acceptance
and cleanup work, not permission to forward traffic to Node.

| Capability | Current owner | Go status | Cutover gate |
| --- | --- | --- | --- |
| `/health`, `/ready` | Go | Go-owned | health/readiness tests |
| Operator auth (`/api/health`, `/api/auth/*`) | Go | Go-owned canonical single-operator surface; inherited `/api/v1/auth/*`, passkey/TOTP, recovery, and page routes are not registered | login/refresh/current-user/logout smoke test and session-binding checks |
| `/v1/*` streaming gateway | Go/Sub2API baseline | Go-owned for migrated adapters | streaming, cancellation, pre-first-byte failover |
| Accounts CRUD/list/IDs/groups | Go | Go-owned, including legacy provider translation, provider-aware filtering, safe account projection, direct-array batch import, and server-side reveal of the password for an active linked GitHub identity | persistence parity and UI smoke test |
| BYOK presets, model discovery, and model probe | Go | Go-owned; preset accounts use normalized upstream credentials and native CN adaptive adapters where available | live preset/custom URL discovery, model probe, and routing smoke tests |
| Manual provider validation | Go | Go-owned; required credential checks plus bounded OpenCode live validation | live Kiro/Qoder/OpenCode/CodeBuddy China add-account smoke tests |
| API keys (`/api/api-keys`) | Go | Go-owned; legacy ID and aggregate usage import added. The single-operator UI no longer collects or labels client IP/User-Agent diversity as resale behavior | production dry-run, imported-count reconciliation, default-key bootstrap, create/delete, and usage smoke test |
| Tray settings (`/api/settings`) | Go | Go-owned | flat JSON read/write and cache invalidation smoke test |
| Antigravity OAuth/gateway | Go | Go-owned provider slice | live OAuth and Gemini/Claude request smoke tests |
| Codex device OAuth | Go | Go-owned provider slice | device approval, refresh, auth.json import |
| Grok device OAuth | Go | Go-owned provider slice | pending/slow-down/denied and refresh smoke tests |
| M365 provider OAuth | Go | Go-owned control-plane slice | callback, `oid/tid`, reconnect fallback |
| Kiro Google OAuth | Go | Go-owned two-stage callback-paste PKCE flow; manual credential import remains available | Google selection, both localhost callbacks, reconnect, refresh |
| Outlook mailbox OAuth/poll | Go + Python IMAP worker | Go-owned control-plane slice | worker key, single-flight poll, OTP/link sanitization |
| Credential migration/envelope reads | Node export + Go importer | Go migration slice available | backup/restore, envelope key, provider smoke tests |
| OpenCode Zen catalogue/warmup | Go | Go-owned | live sync and retired-model smoke test |
| Kiro provider adapter | Go | Go-owned for Chat Completions, Anthropic Messages, Responses, Google OAuth, token refresh, native model discovery, and account check/warmup | live Google OAuth and manual credential smoke test |
| Qoder provider adapter | Go | Go-owned for Chat Completions, Anthropic Messages, Responses, PAT job-token exchange, live/fallback catalog, and account check/warmup | live PAT streaming/cancellation/tool-call smoke test |
| Proxy model catalog | Go | Go persistence plus list/create/update/delete and OpenCode sync available | catalog export/import reconciliation and remaining provider sync parity |
| Proxy panel/config/resilience | Go | config/status, usage-derived stats/logs, and database-derived resilience coverage are Go-owned | persisted config and UI smoke test |
| Proxy inventory/pool UI | Go canonical `proxies` table | Go-owned compatibility routes for status, bounded import, global and per-record enable/disable, safe list projection, and connectivity probes; the obsolete Node global rotation/bypass semantics are not reproduced | live authenticated proxy, account binding, automation, and credential-redaction smoke test |
| Core Register orchestration (`register`, `register_subscribe`) | Go control plane + Python worker | Go-owned for the single-process maintenance deployment; versioned worker contract, runtime-manager lease lifecycle, system capability status, and server-side 5SIM/HeroSMS price catalogs implemented | live callback/persistence, cancellation, restart-loss, worker-auth, SMS catalog, and external worker-image smoke tests |
| Grok re-login maintenance | Go control plane + Python worker | Go-owned; Go selects eligible accounts and decrypts the isolated password envelope only for the internal worker request | live expired/error account, callback recovery, cancellation, and worker restart smoke tests |
| Qoder inject maintenance | Go control plane + Python worker | Go-owned; Go selects eligible accounts and sends only the required PAT through the internal worker contract while preserving durable credentials and operator state on callback | live local-UMID inject, callback retry, cancellation, and worker restart smoke tests |
| Worker-host Qoder desktop maintenance | Removed | Public machine reset, local auth injection, and updater patch routes were removed because the remote worker container is not the operator's Qoder desktop; Qoder registration/claim and trial inject remain supported jobs | verify the Register UI exposes no local-desktop action and no browser credential payload |
| CodeBuddy GitHub linking maintenance | Go control plane + Python worker | Go-owned; Go selects and reserves eligible GitHub identities, opens isolated automation envelopes only for the worker request, completes links from authenticated callbacks, and releases incomplete reservations | live linking, partial batch failure, cancellation, callback retry, and worker restart smoke tests |
| CodeBuddy China claim maintenance | Go control plane + Python worker | Go-owned; Go selects accounts with refresh tokens and sends only the minimum token pair through the authenticated worker contract; callbacks merge rotated tokens | live claim, token rotation, partial failure, cancellation, and worker restart smoke tests |
| Settings, dashboard, filters | Go compatibility | settings, single-operator usage analytics, model ranking, and request filter runtime are Go-owned; built-in Chat, Social Token donation management, member management, and user ranking were removed | web contract and visual regression parity |
| Public landing statistics | Go | Go-owned aggregate request/token totals and top-model projection with bounded output and short public caching; fabricated pool and throughput claims removed from the web UI | empty database, populated database, and public access smoke test |
| Operator live refresh | Go HTTP compatibility | active Next.js dashboard and model leaderboard use visibility-aware single-flight polling; the unused Node `/ws` event client and account-store subscriptions were removed | browser visibility, timer cleanup, and refresh smoke test |
| SaaS payment/subscription/member surfaces | Disabled | Production router and handler aggregate expose only operator auth, Neonix compatibility APIs, and gateway APIs; inherited user/member, generic Sub2API admin routes, and local `/v1/sub2api/billing` remain unregistered. Payment, affiliate, redeem, promo, subscription, and their expiry services are absent from production Wire. Operator auth receives none of their collaborators, and historical member/admin role values no longer gate the active local operator. The remote-relay billing probe remains provider metadata infrastructure | remove dormant services, generated schema, and frontend artifacts |
| CodeBuddy `.ai` provider adapter | Go | Go-owned for Chat Completions, Anthropic Messages, Responses, device OAuth, token refresh, live/fallback model discovery, and account check/warmup | live `.ai` device-login, reconnect, streaming/cancellation/tool-call smoke test |
| CodeBuddy China provider adapter | Go gateway + Python automation | Go-owned for Chat Completions, Anthropic Messages, Responses, curated catalog, and account check/warmup; Python retains registration/token rotation | live regional credential, rotation, streaming/cancellation, and tool-call smoke test |

The production topology has no Node service and the Go service must never proxy
a request to Node. Keep a PostgreSQL/Redis backup and the previous complete
deployment image until live provider acceptance passes.
