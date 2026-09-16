# Neonix Go migration runbook

The Go backend is the production target. The Next.js Neonix application remains the product UI, and Python automation remains a separate worker for browser and mailbox tasks.

Neonix always normalizes `run_mode` to `simple`. This is intentional for the
private single-operator deployment: API-key authentication and operational
usage logs remain active, while inherited balance, payment, subscription,
redeem, affiliate, and member-management enforcement is not part of the
production surface.
Historical `users.role` values do not gate the operator control plane: any
active local account with a valid password/session is treated as the single
operator, and compatibility responses normalize its role to `admin`. The first
active account is used for Admin API key attribution. The column remains only
until the planned schema cleanup.

The runtime router registers local operator authentication, the Neonix
compatibility control plane, and the gateway data plane. Inherited Sub2API
user/member routes and its generic admin panel routes are deliberately not
registered. Their source remains temporarily as a migration reference and can
be deleted with the dormant service and schema cleanup.
The API-key and Gemini authentication chains also receive no subscription
service. In simple mode they validate the key, operator record, group, and IP
policy without starting subscription enforcement on the request path.
The inherited local `GET /v1/sub2api/billing` endpoint is not registered. The
upstream billing probe remains enabled for remote Sub2API relays because it is
provider metadata collection, not a local customer-billing surface.
Production Wire also omits the inherited subscription-expiry and payment-order
expiry schedulers. The dormant subscription service can still satisfy legacy
constructor dependencies during migration, but simple mode does not start its
L1 invalidation subscriber or maintenance worker pool.
The production handler aggregate now contains only operator auth, account/group
control, register automation, API keys, settings, usage, and gateway handlers.
Operator authentication is wired without public registration, promo, redeem,
affiliate, captcha, or default-subscription collaborators. Payment, affiliate,
redeem, promo, and subscription services are no longer constructed by the
server injector. Their source remains only as migration reference until schema
cleanup.
Public registration, password recovery, passkey, optional TOTP,
promo/invitation validation, user-facing OAuth/SSO, and inherited page routes
are not registered. The local operator uses only login, refresh, current-user,
and logout endpoints. OAuth used to add provider accounts stays on the
admin-only `/api/accounts/*/oauth/*` surface.

## Credential migration

1. Export a consistent account snapshot from the current PostgreSQL database. Do not print the export or put it in a ticket.
2. Set `NEONIX_CREDENTIAL_KEY` to the 32-byte key that will be used by the Go deployment. Keep it in the deployment secret store.
3. Run `go run ./cmd/migrate --source migration-export.json` from `backend/`. The source may contain `accounts`, `apiKeys`, `apiKeyUsage`, `apiKeyAccessLogs`, `models`, and the allowlisted `settings` object. Model rows use the existing camelCase `models_cache` API shape. The default is a dry run; it emits only counts and redacted issue codes.
4. Resolve every `blocked` issue. Migration must not proceed with an active account missing credentials or with duplicate IDs.
5. Run `go run ./cmd/migrate --source migration-export.json --apply --output normalized-accounts.json` and verify the output file permissions are `0600` and its hash is recorded in the cutover checklist. The output artifact contains encrypted account envelopes only; API-key bearer values and settings values are never written to it.
6. Start the Go binary once (or apply migration `238_account_credential_envelopes.sql`) so the encrypted handoff table exists, then during the maintenance window run `go run ./cmd/migrate --source accounts.json --apply --dsn "$NEONIX_MIGRATION_DSN"`. The importer opens one PostgreSQL transaction, matches by legacy ID (email fallback), updates credentials only on existing rows, writes the encrypted envelope, and rolls back every row if any write fails. It reports `created`/`updated` counts without credential values. Start the Go service with the same `NEONIX_CREDENTIAL_KEY`; migrated account reads then decrypt the envelope before an adapter sees credentials. Keep the original snapshot read-only until smoke tests and rollback checks pass.

Gateway API keys need the same maintenance-window treatment: the Go schema
uses numeric IDs while Neonix's table uses text IDs. Apply migration
`239_neonix_legacy_api_key_ids.sql`, import each key into the Go `api_keys`
table under the single operator, and record its original ID in
`neonix_legacy_api_key_ids`. The compatibility routes accept either the native
numeric ID or that mapped legacy ID, so existing client configurations and
usage links keep working after cutover. Export `api_key_usage` and
`api_key_access_logs` in the same envelope; the importer stores them in the
append-only compatibility history tables and the Go usage endpoints merge
those rows with native Go history. Keep the original snapshot read-only until
the imported counts and latest access timestamps have been verified.

The converter preserves provider credential bytes before encryption. Deprecated `bai`, `bb`, and `codebuff` records are reported as skipped and are not copied into the active Go store. No access, refresh, or API token is included in reports or command errors.

Built-in Chat history is intentionally not migrated. Neonix keeps the gateway
protocol endpoint `/v1/chat/completions`, but no longer provides a conversation
UI or persistence API. Migration `249_drop_neonix_chat.sql` removes legacy chat
tables during the maintenance upgrade.

## Current compatibility slice

The migration branch exposes the operator-facing `/api` compatibility surface
behind the admin guard. Account CRUD, provider registry/coverage, readiness,
Codex and Grok device login, Kiro Google callback-paste OAuth, M365 callback-paste OAuth, and Antigravity OAuth
callback-paste login are available there; the middleware
unwraps the Go response envelope so the existing Neonix transport can use the
same direct JSON shape. The canonical Sub2API routes remain under `/api/v1`.

The same boundary now exposes `/api/api-keys`. It bootstraps one `default` key
for the operator when none exists, returns the legacy camelCase array and key
object shapes, and delegates key mutations to the Go API-key service. Usage
aggregates and access fingerprints are read from the Go usage-log store. Usage
counts distinguish all request rows from billed-success rows, and access
fingerprints cover the same rolling 24-hour window as the Neonix panel. The raw
key is returned only to the authenticated operator endpoints that need it for
client configuration.

Tray preferences use `/api/settings` in the same compatibility group. Values
are encoded as JSON in the existing settings table, so booleans, numbers, and
objects survive a Node/Go rolling cutover. The list endpoint only exposes the
small Neonix preference allowlist and initializes the response-footer defaults
idempotently; the typed admin settings surface remains under `/api/v1`.

`/api/proxy/config` now stores the UI-safe proxy options in the same settings
table and returns Go gateway host/port defaults. Credential-shaped fields are
discarded at the compatibility boundary; API keys continue to use
`/api/api-keys`. The status alias reports the Go process as the active gateway,
and request logs and statistics are derived from the Go usage/error stores.
The Go model catalog owns CRUD plus OpenCode Zen free-model synchronization;
remaining provider-specific catalog synchronizers remain cutover gates.

`/api/filters` is Go-owned. Mutations atomically refresh the compiled runtime
snapshot, and the `/v1` gateway filters only request message/system text. Model
IDs, tool schemas, URLs, credentials, and response bytes are not rewritten.
The account email filter is also Go-owned at
`POST /api/accounts/filter-registered`; it performs a bounded lookup against
the imported provider/email metadata and never returns account credentials.
The Models tab refresh action synchronizes only the authoritative OpenCode Zen
free catalog. Other provider rows remain preserved until their own complete
catalog source is migrated. The single-operator UI no longer exposes the
inherited `requiresPro` model flag.

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

Core Register orchestration is also Go-owned while browser automation remains
in the Python worker. The operator endpoints `/api/register/start`, `status`,
`logs`, `cancel`, `bfs-lockout`, and `python-status` use the admin guard. Worker
callbacks to `/api/register/result` and `/api/register/failure` use only the
shared `x-api-key` service credential. Go accepts one active job per process,
builds worker payloads from validated browser input plus backend-held mailbox,
SMS, subscription, and proxy secrets, and persists successful account callbacks
through the encrypted Go account repository. Callback retries are idempotent
within the active job; status and log responses omit result credentials and
redact secret-shaped text.
`/api/register/system-status` combines worker health, browser capabilities,
active proxy count, and Boolean backend-secret readiness without returning any
secret value. `/api/register/sms/prices` fetches the fixed 5SIM or HeroSMS
catalog server-side, applies an eight-second bound, normalizes only in-stock
countries, and keeps provider API keys outside browser responses.

Configure both processes with the same `PYAUTO_INTERNAL_API_KEY`, set
`PYAUTO_BASE_URL` on Go to the worker origin, and set
`PYAUTO_BACKEND_CALLBACK_URL` on Python to the Go origin. The supported core job
types are `register`, `register_subscribe`, Grok-only `relogin`, and Qoder-only
`inject`. The legacy public Qoder machine reset, local auth injection, and
updater patch actions are intentionally removed: they operate on the Python
worker host, which is not the operator's Qoder desktop in a remote/container
deployment. CodeBuddy China claim is also Go-owned at the control-plane
boundary. A Go restart
forgets the in-memory active-job lease; cancel the worker job before restart or
wait for it to finish, then start a new job. Rollback is process-level: stop new
starts, cancel the active worker job, deploy the previous Go image, and leave
persisted account rows untouched.

Grok re-login selects only credential-error, legacy expired/exhausted, or
automatically expired accounts. Manually paused accounts are excluded. The
legacy migration extracts the Grok password into
`account_register_automation_secrets.password_envelope`, encrypted with
`NEONIX_CREDENTIAL_KEY`; the password is absent from `accounts.credentials`
and all operator responses. After a successful worker callback, Go merges new
tokens with retained provider metadata, restores the account to active and
schedulable, clears expiry and runtime cooldown state, and publishes a scheduler
outbox event. `GET /api/register/grok-expired-count` returns aggregate counts.

Qoder inject candidate selection is also Go-owned.
`GET /api/register/qoder-injectable-count` evaluates the account records without
returning a PAT. `POST /api/register/start` with `type: "inject"` ignores any
browser-supplied proxy or account payload, selects Qoder accounts that have a
valid `pt-...` PAT and have not already received a trial, and sends only the PAT
plus bounded identity metadata over the internal worker connection. The worker
runs the existing Qoder inject implementation serially and posts its result to
the authenticated callback. Go merges the returned plan and usage metadata with
the durable credential document, preserves refresh tokens and the operator's
enabled/disabled state, and never exposes callback credentials through status,
logs, or result responses.

CodeBuddy GitHub linking is Go-owned at the control-plane boundary.
`GET /api/register/github-accounts` returns sanitized identity metadata only.
The browser starts the job with `github_account_ids`; Go locks eligible rows,
opens `account_github_identity_secrets.secret_envelope`, and sends passwords and
cookies only over the authenticated internal worker request. The worker callback
contains the resulting CodeBuddy account and stable GitHub identity ID. Go then
upserts the CodeBuddy account, refreshes returned GitHub session cookies, and
atomically marks the reservation active. Start failure, cancellation, worker
404, per-account failure, and terminal completion release any rows still in
`linking`; active links remain untouched.
`POST /api/accounts/:id/linked-identity/reveal` verifies the target is an active
CodeBuddy link, opens only the linked GitHub identity envelope, disables HTTP
caching, and returns only the password required by the operator detail dialog.
The endpoint never returns cookies or the encrypted envelope.

Migration `251_github_identity_linking.sql` creates the isolated identity-secret
and link tables. `NEONIX_CREDENTIAL_KEY` encrypts their envelopes. When the Node
export contains an encrypted `githubSecret`, also provide the legacy
`BYOK_ENCRYPTION_KEY` to `cmd/migrate`; the importer opens the Node AES-GCM
payload once, reseals it with the Go envelope, and strips password/cookie fields
from provider credentials. Back up both new tables with `accounts`. Rollback is
image-level plus database restore if migration 251 itself must be reverted; do
not drop the tables while an active link or identity secret depends on them.

BYOK account operations are served directly by Go. The compatibility API
accepts Neonix preset IDs, stores the original ID in `source_provider`, and
normalizes credentials into the Go scheduler model. Z.AI, DeepSeek, Moonshot,
and MiniMax use their native adaptive adapters so both [OI] and Anthropic
surfaces keep working; custom and MiMo endpoints use the [OI] upstream adapter.
Before cutover, smoke-test preset listing, temporary model discovery, a stored
account model fetch, and one model request for every imported BYOK account.
Custom URLs must remain HTTPS and must not resolve to loopback, private, link
local, or multicast addresses. Roll back the Go image if any existing BYOK
credential no longer routes with its original prefixed model ID.

The former Social Token donation tab and its `/api/social-token/*` browser
client are removed. This deployment has one local operator and gateway API
keys are the sole client credential surface. An old `?tab=social` deep link
falls back to Proxy Logs.

CodeBuddy China claim mode accepts only `cbcn_action: "claim"` from the
operator request. Go loads existing `codebuddy-china` accounts with a refresh
token from encrypted account storage and sends only account ID, email, access
token, and refresh token to the authenticated worker. SMS provider keys are not
loaded in claim mode. Worker callbacks merge the rotated token pair into the
same account and preserve all other credential and operator metadata. A partial
batch remains retryable: successfully rotated accounts are already durable, and
the next claim run selects every account that still has a refresh token.

The production Compose deployment uses the optional
`deploy/docker-compose.automation.yml` overlay. Set `PYAUTO_IMAGE` to a pinned
worker image which implements
`contracts/fixtures/python-automation-http-contract.v1.json`; the Go build does
not copy or mount the legacy repository. The overlay connects Go to the
always-lightweight runtime manager on port 7790 and the on-demand child API on
port 7788. Go acquires a job-scoped lease before starting Register, keeps it
through `pending` and `running`, and releases it on terminal status, start
failure, cancellation, or a worker 404. Mailbox polling uses a separate
request-scoped lease, so completing a mailbox request cannot stop an active
Register job.

`GET /api/register/python-status` reports manager reachability separately from
the child process: `manager_running` means the supervisor answered,
`child_running` reflects the supervisor process table, and `child_ready` means
Go reached the child `/health` endpoint. An idle child is expected to report
`manager_running=true`, `child_running=false`, and `child_ready=false`. A false
`manager_running` value is a deployment or service-key problem. Keep
`PYAUTO_INTERNAL_API_KEY` identical on Go and the worker; if
`RUNTIME_MANAGER_API_KEY` is set, it must also match on both sides.

The legacy Accounts `check` and `warmup` actions are available as bounded Go
usage probes. They return the sanitized account snapshot and usage result; a
provider-specific streaming test remains part of the provider-adapter slices.

### Kiro provider slice

Kiro requests are handled entirely by Go for `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses`. The adapter refreshes social or AWS OIDC credentials, decodes
AWS EventStream with CRC validation, discovers the paginated native model
catalog, and retries another Kiro endpoint only before semantic output starts.
The migration adds no schema and does not rewrite stored credentials.

Before directing live Kiro traffic to this slice, run the Kiro unit and contract
tests, the payload benchmark, and one social or OIDC credential smoke test. The
recorded local benchmark command is:

```sh
go test ./internal/pkg/kiro -run '^$' -bench BenchmarkBuildChatPayload -benchmem -count=5
```

On the September 2026 validation host (AMD Ryzen 5 7500F, Go linux/amd64),
five runs measured 1.055-1.082 microseconds per payload, 849 B/op, and 14
allocations/op. This benchmark covers local request translation only; the live
credential smoke test remains the latency gate for upstream TTFT.

Rollback is image-level: stop the Go instance, deploy the previous Go image or
revision without Kiro dispatch, and restore the previous routing configuration.
No database rollback is required for this slice. Keep the account rows and
encrypted credential envelope unchanged so the previous adapter can use the
same credentials. Do not retry or switch implementations after response bytes
have reached the client.

### Qoder provider slice

Qoder PAT accounts are handled entirely by Go for `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses`. The adapter exchanges the stored PAT for the short-lived Qoder
job token, reproduces the COSY request signing/encryption contract, decodes the
nested SSE stream, and preserves text, reasoning, tool calls, and usage. The
PAT, session encryption material, job token, refresh token, and upstream error
body never enter client responses or logs. Qoder remains a manual PAT or Qoder
CLI login; Neonix does not present it as OAuth.

Qoder Pro Trial inject remains a Python automation operation, with the Go
control plane owning eligibility, proxy selection, job concurrency, persistence,
and the public aggregate count. Run it only through the operator Register API;
the browser never supplies account credentials. If the slice must be rolled
back, stop new inject starts, cancel the active worker job, deploy the previous
Go image, and retain the Qoder account rows. A completed callback is an
idempotent metadata update and does not require a schema rollback.

Do not expose the worker's `qoder_reset`, `qoder_auth`, or
`qoder_disable_update` job types as browser APIs. Their filesystem mutations
target the machine running the worker and therefore cannot manage a Qoder
installation on the browser computer. In particular, do not send account
tokens or filesystem paths from the web client. Local desktop maintenance, if
needed, belongs in a separately installed local tool rather than the remote
Neonix control plane.

The catalog prefers live environment metadata, deduplicates model IDs, and falls
back to the built-in Qoder model IDs and internal `actualModelId` mapping when
the environment endpoints are unavailable. Price factors remain technical usage
metadata and do not create a Pro gate in this single-operator deployment. Account
check and warmup use `qr/Lite` through the same native adapter. Python register
automation stays separate.

Before routing live Qoder traffic, run `go test ./...`, the Qoder payload
benchmark, and a PAT smoke test covering streaming, cancellation, and one tool
call. Rollback is image-level and requires no schema rollback: deploy the prior
Go revision and keep the encrypted account rows unchanged. No retry or adapter
switch may happen after semantic response output starts.

On the September 2026 validation host (AMD Ryzen 5 7500F, Go linux/amd64), five
runs after the payload parity changes measured 2.950-5.803 microseconds per
operation, 5788-5790 B/op, and 64 allocations/op. This covers local payload
construction only; use the live PAT smoke test for upstream TTFT.

### CodeBuddy `.ai` provider slice

CodeBuddy `.ai` is handled entirely by Go for `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses`. The adapter accepts both CLI device-login
credentials and historical browser API-key/cookie credentials. CLI access
tokens refresh through the issuing `.ai` or WorkBuddy realm; a refresh response
that omits `refreshToken` preserves the existing grant. Refresh tokens are sent
only to the fixed refresh endpoint and never to chat, model-discovery, client
responses, or logs. Numeric token expiry values remain compatible in Unix
seconds and milliseconds.

The admin-only compatibility endpoints are
`/api/accounts/codebuddy/oauth/start|poll|cancel`, with the older
`device-auth` spelling retained as an alias. Login sessions are opaque,
single-flight, single-use, expire after 15 minutes, and keep the upstream state
server-side. Completion upserts by CodeBuddy user ID and preserves the existing
account metadata and refresh token during reconnect. A failed persistence claim
can be retried until the session expires.

The model catalog prefers the authenticated enterprise endpoint and falls back
to the built-in `cb/` catalog if discovery is unavailable or empty. Upstream
requests always stream; Go buffers only when the client selected a buffered
protocol. Text, reasoning, multimodal messages, tool-call fragments, usage, and
the historical `cb/` default-model behavior are preserved. Another account may
be tried only before semantic output reaches the client. HTTP 401 is credential
scoped, 429 carries a bounded account cooldown, 400/404 are request scoped, and
upstream bodies plus unsafe headers are discarded.

CodeBuddy China remains a separate provider and protocol. The `.ai` device
flow rejects CN realms, and the `.ai` gateway refuses credentials whose domain
resolves to `copilot.tencent.com` or `codebuddy.cn`; this prevents the regional
protocol from being silently routed through the wrong adapter.

Before routing live CodeBuddy `.ai` traffic, run the full Go test/vet/build
checks, the payload benchmark, and these smoke tests with an existing credential:

1. Start device login, approve it in the browser, reconnect the same identity,
   and verify that the account ID and refresh token remain stable.
2. Run buffered and streaming Chat Completions, one Anthropic Messages request,
   one Responses request, cancellation, and one fragmented tool call.
3. Fetch the live enterprise model catalog, then simulate an unavailable catalog
   and verify the built-in fallback remains usable.
4. Force a refresh and verify the resulting account still works without any
   token or cookie in API responses and logs.

The recorded benchmark command is:

```sh
go test ./internal/pkg/codebuddy -run '^$' -bench BenchmarkBuildPayload -benchmem -count=5
```

On the September 2026 validation host (AMD Ryzen 5 7500F, Go linux/amd64), five
runs measured 5.739-5.808 microseconds per payload, 5621-5622 B/op, and 77
allocations/op. This measures local payload translation only; the live device
credential smoke test remains the upstream TTFT gate.

Rollback is image-level: deploy the previous Go revision before CodeBuddy `.ai`
dispatch, keep the encrypted account records untouched, and restore the prior
routing configuration. This slice adds no database schema. Do not retry or
switch implementations after semantic response output starts.

### CodeBuddy China provider slice

CodeBuddy China inference is handled entirely by Go for
`/v1/chat/completions`, `/v1/messages`, and `/v1/responses`. It uses the fixed
regional `https://www.codebuddy.cn/v2/chat/completions` endpoint and accepts
the historical persisted bearer-token aliases without rewriting credential
records. The adapter strips the public `cbc/` prefix before sending the model
upstream and preserves text, reasoning, multimodal messages, tools, fragmented
tool calls, and usage across streaming and buffered protocols.

The model list is curated because the regional service does not expose a
stable public catalog contract. Public IDs retain the `cbc/` namespace while
`actual_model_id` stores the exact upstream wire value. The catalog and account
test paths have no billing or Pro gate. Unsupported models and content-safety
rejections are request scoped; credential failures, rate limits, and transient
upstream failures may rotate to another account only before semantic output
starts. Upstream error bodies and credential-shaped headers are never returned
to the client.

Registration, SMS/Keycloak automation, and token rotation remain in the Python
automation worker. The Go request path reads the resulting encrypted credential
record directly and never calls Node. Before routing live regional traffic:

1. Register or rotate one account through the Python worker and verify that the
   existing database row remains readable by Go.
2. Run buffered and streaming requests for Chat Completions, Anthropic Messages,
   and Responses, including cancellation and one fragmented tool call.
3. Run account check/warmup with the default `cbc/deepseek-v3` model and verify
   the curated model picker exposes only `cbc/` IDs.
4. Force 401, 429, unsupported-model, content-safety, and 5xx responses and
   verify their scope/cooldown behavior without token leakage.

The recorded payload benchmark command is:

```sh
go test ./internal/pkg/codebuddychina -run '^$' -bench BenchmarkBuildPayload -benchmem -count=5
```

On the September 2026 validation host (AMD Ryzen 5 7500F, Go linux/amd64), five
runs measured 4.835-4.865 microseconds per payload, 2949-2950 B/op, and 59
allocations/op. This covers local payload normalization only; use the live
regional credential smoke test for upstream TTFT.

Rollback is image-level: deploy the previous Go revision before CodeBuddy China
dispatch and keep both the encrypted account records and Python worker data
unchanged. This slice adds no database schema. Never replay through another
implementation after response output has started.

The compatibility surface preserves the Neonix browser contract while Go owns
the implementation. Remaining provider acceptance and source cleanup continue
as reviewed slices; production does not forward to Node.

### Canonical proxy inventory slice

The Neonix Proxy Pool page now reads and updates Go's canonical `proxies`
table through `/api/proxy/custom/*`. Adding a line creates a normal Go proxy;
removing a line disables the corresponding record so account references and
stored credentials remain intact. The global switch activates or disables the
saved records. Each inventory row can also be disabled or restored by canonical
proxy ID without returning or replacing its credentials. Connection tests use
the same bounded server-side prober as
the canonical admin API. List and status responses contain only scheme, host,
and port. Usernames and passwords never return to the browser.

This slice deliberately drops the old Node-only rotating current pointer and
provider bypass list. Go routes inference through the proxy explicitly bound to
an account, while registration automation receives the active proxy inventory.
Before cutover, verify one authenticated HTTP proxy and one SOCKS5 proxy, bind
an account to each, run a provider request and a registration dry run, then
inspect API responses and logs for credential leakage. Roll back by deploying
the previous Go image; no schema migration is introduced and disabled proxy
records retain their credential envelopes.

The active Next.js application no longer opens the legacy Node `/ws`
connection. Account mutations already consume their Go HTTP responses, while
dashboard and leaderboard data refresh through bounded, visibility-aware HTTP
polls that schedule the next request only after the previous one completes.
This avoids permanent reconnect traffic against a WebSocket route the Go
operator surface does not expose.

The API-key panel is an operator credential view. It no longer shows an
"anti-resale" badge or client IP/User-Agent diversity, because this private
single-operator deployment has no resale or member-abuse product policy. The
aggregate per-key usage route remains available for operational accounting.

The operator auth aliases at `/api/auth/*` accept the Neonix username payload,
issue the Go JWT, and keep `/api/auth/me` and logout behind the admin guard.
This is the canonical production authentication surface. The inherited
multi-user `/api/v1/auth/*` surface and its optional-JWT/step-up middleware are
kept only as dormant source while schema cleanup is pending.

## Cutover gates

- `go test ./...` passes on the exact commit deployed.
- A dry-run report has zero blocked active records.
- PostgreSQL and Redis backups are available.
- `/health` and `/ready` are green after starting the Go service.
- One request succeeds for each enabled provider, including a streaming request and cancellation.
- Production Compose contains no Node backend. Python worker processes remain independently managed.
