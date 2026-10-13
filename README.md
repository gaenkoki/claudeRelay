# claude-relay

Minimal multi-account Anthropic Messages relay for Claude subscription credentials. It accepts the
native Anthropic request format only; there is no OpenAI compatibility layer, user billing,
round-robin rotation, or Claude Code prompt injection.

## Current scope

- Multiple imported Claude OAuth credentials, optionally fenced so chosen accounts serve official
  traffic only
- Separate compatible, official, and administration API keys
- `POST /v1/messages` and `POST /v1/messages/count_tokens`
- `GET /v1/models` and `GET /v1/models/{model_id}`, read from Anthropic through an account and
  cached
- Transparent JSON and SSE responses unless an optional response transformation is enabled
- Minimum subscription attribution for ordinary Anthropic requests
- Caller-owned billing and metadata fields preserved without interpreting unknown fields
- Sticky-first account selection, in-process load-aware routing for new sessions, strict local
  admission limits, and one bounded transient failover
- Disabled-by-default account imports and manual activation
- Embedded management console with PKCE OAuth login, credential paste-import, and account removal
- Bounded in-memory request records exposing routing, failover, and cooldown state
- On-demand rotating OAuth refresh for enabled accounts

## Refusal billing usage

The management console's connection panel offers an optional **Claude 拒绝计费倍率** setting.
It defaults to disabled, with `cyber` at 2, `reasoning_extraction` at 3, and `bio`,
`frontier_llm`, and `general_harms` at 5. Multipliers accept values from 1 to 100, including
decimals; 1 leaves that category unchanged. Saving persists the settings in SQLite and applies
them to new requests immediately. In-flight requests keep the settings they started with.

For successful `/v1/messages` responses explicitly marked `stop_reason: refusal` with a known
category, the relay multiplies downstream input, output, and cache token counts, rounding each
upward. SSE responses are transformed one event at a time when the final refusal arrives.
Unknown or empty categories and `count_tokens` responses remain unchanged. Local account usage
always records the original upstream counts; request records show the original and adjusted
counts and multiplier. Malformed or overflowing counts fail response adaptation rather than
inventing usage.

This changes the usage consumed by downstream token billing; it does not deduct a balance in
this relay. Downstream pricing still determines the charge: inflated input can cross a long
context price threshold, and a client disconnect before the final usage event can prevent the
adjustment from reaching downstream. The new-api and sub2api validation covers their usage and
price calculations, not a complete production balance/refund transaction.

The dated protocol conclusions and unresolved questions are centralized in
[`docs/protocol-findings.md`](docs/protocol-findings.md). Raw experiment notes remain in
[`docs/protocol-experiments.md`](docs/protocol-experiments.md).

The subscription-consumption analysis goal and reporting conventions are recorded in
[`docs/subscription-observation.md`](docs/subscription-observation.md).

## Build

```powershell
go build -o claude-relay.exe ./cmd/claude-relay
```

For the recommended single-instance server deployment, use the included `compose.yaml`. The
container runs as a non-root user, keeps SQLite in a private named volume, and binds only to the
host loopback interface by default. See [`docs/deployment.md`](docs/deployment.md) for migration,
HTTPS, private GHCR images, proxy, backup, and upgrade instructions.

## Import a CLIProxyAPI credential

```powershell
.\claude-relay.exe import `
  -from F:\path\to\cliproxy\auths\claude-user.json `
  -alias personal `
  -db data\claude-relay.db
```

The importer preserves the real account UUID when available and creates a persistent random
device identity when absent. Import each account under a unique, stable ASCII alias using letters,
digits, dots, underscores, or hyphens. Every import and reimport leaves the account disabled;
activation is a separate explicit action. Reimporting the same account preserves its database
identity. Unknown source fields are retained under `extra`; tokens are never printed.

Existing single-account installations that still set `credentials_file` are migrated into the
SQLite database as alias `default` the first time the new server starts with an empty database.
The old JSON file is retained as a backup and is no longer read after the database has an account.
The schema upgrade that introduced activation also disables every pre-existing account once.

## Configure and run

```powershell
Copy-Item config.example.json config.json
$env:CLAUDE_RELAY_API_KEY = "replace-with-a-long-random-key"
$env:CLAUDE_RELAY_EXPERIMENTAL_API_KEY = "replace-with-a-different-long-random-key"
$env:CLAUDE_RELAY_OFFICIAL_API_KEY = "replace-with-a-different-long-random-key"
$env:CLAUDE_RELAY_ADMIN_API_KEY = "replace-with-a-different-long-random-key"
.\claude-relay.exe serve -config config.json
```

Point an Anthropic client to `http://127.0.0.1:8567`. In the WebUI, open an account's
**出口设置** to choose direct, a custom HTTP(S) proxy, or inheritance of the global
`upstream_proxy` / `CLAUDE_RELAY_UPSTREAM_PROXY` setting. New accounts default to direct;
accounts migrated from older releases inherit the global setting to preserve their exit.
Messages, token counting, account checks, token refresh, and usage/profile queries all use
the account's exit. OAuth setup also lets you choose the exit before exchanging the code.
Browser authorization traffic uses the browser's network.

Proxy settings persist in SQLite and apply to subsequent requests without restarting. Direct
ignores proxy environment variables. A failed proxy never falls back to direct for that account.
Custom proxies accept `http://user:password@host:port` or an address with credentials entered
separately in the WebUI; credentials are omitted from administration responses and redacted in
transport errors. To keep existing custom credentials, leave all three proxy inputs blank.
Entering a new address replaces the entire custom proxy configuration, including authentication.

`max_inflight_per_account` is a per-account hard in-process Messages request limit, set to `8` by
default. Native `/v1/messages/count_tokens` calls use a separate short-request limit,
`max_count_tokens_inflight_per_account`, set to `32` by default; they consume neither a Messages
slot nor an active-session slot, and do not create or refresh session bindings.
`max_active_sessions_per_account` admits at most `5` recently active, explicitly identified client
sessions per account by default. Cache-prefix affinity without a session ID does not consume a
session slot. Explicit client sessions retain sliding one-hour affinity; declared cache prefixes
retain affinity for their five-minute default or explicit one-hour lifetime. A new session may
select another eligible account when either limit is full; an
existing sticky session never switches accounts because of local pressure. If its account is full,
or no account can admit a new session, the relay returns `429`. Under the default `legacy` policy,
Anthropic's five-hour utilization is sampled for the console and usage estimates only; the optional
quota-aware policy described below also uses it for routing. Both
limits can be overridden with the matching `CLAUDE_RELAY_*` environment variables.

## Model list

`GET /v1/models` and `GET /v1/models/{model_id}` accept the same model keys as Messages and return
the native Models API shape (`data`, `has_more`, `first_id`, `last_id`). `limit` (default `20`, at
most `1000`), `after_id`, and `before_id` are applied by the relay. These are metadata reads: the
official ingress does not require a Claude Code-shaped request for them, and they take no admission
slot and create no request record.

The relay reads Anthropic's own `GET /v1/models` with the first healthy account the key's policy may
use, through that account's exit, with `anthropic-beta: oauth-2025-04-20`, and shares the result
for one hour per policy. A newly released
model therefore appears without a new build. Without any list yet, a request waits up to two seconds
for that read; an expired list is answered at once while one shared refresh runs. A failed read is
not repeated for five minutes. If no upstream list is available after the wait, the endpoint returns
`503 api_error`; no built-in model list is served. `X-Claude-Relay-Models-Source` reports
`upstream`, `stale`, or `unavailable`. Upstream error details remain in logs and the admin status.

The console's connection panel shows each policy's list as an expandable table of model IDs,
names, release dates, and token limits. Opening it calls `GET /admin/v1/models`, which reports
the cached lists without reading upstream. After a model release, **刷新模型列表**, or
`POST /admin/v1/models/refresh`, reads the list again at once without a restart. It ignores the
cache age and the five-minute failure delay, refreshes the official policy only when an official key
is configured, and reports each policy's source, model count, read time, and last read error.

Claude Code can fill its `/model` picker from this endpoint:

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8567"
$env:ANTHROPIC_AUTH_TOKEN = "your-relay-key"
$env:CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY = "1"
claude
```

It requests `/v1/models?limit=1000`, keeps IDs containing `claude` or `anthropic`, and caches them
in `~/.claude/cache/gateway-models.json`.

## Downstream errors

Errors generated by the relay use Anthropic's JSON error envelope. For normal, non-debug traffic,
callers only need the following categories:

| HTTP status | Error type | Meaning and caller action |
| --- | --- | --- |
| `400` / `413` | `invalid_request_error` | The request is malformed or too large; fix it before retrying. |
| `401` | `authentication_error` | The relay API key is invalid. |
| `403` | `permission_error` | The official ingress rejected a request that was not Claude Code-shaped. |
| `429` | `rate_limit_error` | Relay capacity is temporarily full; back off and retry. Existing affinity is not bypassed merely to escape a local limit. |
| `502` | `api_error` | The relay could not complete an upstream network request; retry with backoff. |
| `503` | `api_error` or `authentication_error` | No account is currently usable, or the selected account could not refresh authentication; retry later. |

The normal local `429` message is deliberately generic: `relay capacity limit reached; retry later`.
Whether the internal cause was an in-flight limit, a new-session admission limit, or exhausted
capacity across all eligible accounts remains in the trusted request log instead of becoming part
of the downstream protocol. The private `X-Claude-Relay-Account` debugging override may return a
more specific error, but never includes the account alias in the response body.

An HTTP response received from Anthropic is forwarded with its upstream status and body. Before
returning an upstream `429`, `529`, or `5xx`, an unpinned request may try one other account. By
contrast, hitting a relay-local affinity limit does not migrate the request: the downstream client
receives the local `429` and should retry after backing off.

## Console

Open `http://127.0.0.1:8567/` and sign in with the administration API key from the configuration.
The console is a single dense screen with four sections:

- **账号** — every account with its pool and real routing state: enabled, cooling down (with the reason and
  remaining time), token expiry, last successful refresh, traffic totals, live sticky bindings, and
  rolling 24-hour refusal activity. Accounts with a recent refusal enter the attention filter.
  Successful Messages response headers update the available five-hour window automatically without
  another upstream request. A manual refresh remains available for optional weekly/model-specific
  OAuth usage windows and the plan label. Per-account actions cover usage refresh, enable/disable,
  connectivity check, forced token refresh, cooldown release, rename, and deletion.
- **请求** — the recent request records described below, filterable by account and by failures only.
  A policy refusal is marked separately even when Anthropic returns HTTP 200; it is not counted as a
  transport failure.
- **用量** — hourly token/API-price totals plus current and explicitly exhausted five-hour windows.
  The usage view also compares Token cache coverage by API ingress group (`compatible`,
  `experimental`, and `official`). Ingress-group rows begin at the schema migration boundary;
  older hourly rows cannot be attributed to an API key group.
  Window rows expand into per-model input, 5m/1h cache creation, cache-read, output, and API-price
  composition. The long-term observation data can be exported as ZIP/CSV or cleared independently.
- **接入** — the relay endpoint, all configured ingress API keys, copy-ready Claude Code / PowerShell / curl
  snippets, and the effective runtime parameters.

It polls every five seconds while the tab is visible, checks `/healthz` for the status indicator,
and follows the system light/dark preference with a manual override. The management key lives in
the current tab's `sessionStorage`; it is not written to the server or to persistent browser
storage. The static login page is public, while every management API remains authenticated.

The console shows all configured ingress API keys so working client configurations can be copied in one step.
The administration key already controls account placement and credentials, so this does not widen
the trust boundary — but it does mean anyone who signs in to the console can obtain every model role.
Put the service behind an HTTPS reverse proxy before exposing it over a network.

## Account management

Management endpoints accept only the administration API key and never return access or refresh
tokens. Neither ingress API key can access console data, OAuth operations, or account activation.
Every timestamp in an administration response is epoch milliseconds.

```http
GET    /admin/v1/overview
POST   /admin/v1/routing/policy
GET    /admin/v1/accounts
POST   /admin/v1/accounts/import
DELETE /admin/v1/accounts/{alias}
POST   /admin/v1/accounts/{alias}/enable
POST   /admin/v1/accounts/{alias}/disable
POST   /admin/v1/accounts/{alias}/rename
POST   /admin/v1/accounts/{alias}/pool
POST   /admin/v1/accounts/{alias}/refresh
POST   /admin/v1/accounts/{alias}/cooldown/clear
POST   /admin/v1/accounts/{alias}/check
GET    /admin/v1/accounts/{alias}/usage
POST   /admin/v1/accounts/{alias}/usage/refresh
GET    /admin/v1/usage?from={epoch_milliseconds}
DELETE /admin/v1/usage
GET    /admin/v1/usage/prices
POST   /admin/v1/usage/prices
GET    /admin/v1/usage/five-hour/export
DELETE /admin/v1/usage/five-hour
GET    /admin/v1/models
POST   /admin/v1/models/refresh
```

`import` accepts a pasted CLIProxyAPI credential document and leaves the account disabled. It
rejects an existing alias or account identity unless the caller explicitly sends `replace: true`;
the console requires an additional confirmation before doing so. `check` counts tokens for a trivial prompt to
prove the account still reaches upstream; it never refreshes, so a disabled account can be verified
without this relay taking ownership of its refresh-token chain. `refresh` obeys the same ownership
rules as automatic refresh and is rejected for a disabled account or while the global emergency
stop is set. `delete` removes the account with its cooldowns and sticky bindings but does not
revoke the authorization at Anthropic.

`POST /admin/v1/routing/policy` accepts `{"policy":"legacy"}` or
`{"policy":"quota_aware"}`. The selection is process-local by design and always returns to
`legacy` after a restart. The console exposes the same switch under the connection panel; no
configuration file, environment variable, or database migration is involved.

The usage endpoint reads Anthropic's private OAuth usage surface and returns only windows actually
present in the upstream response. A missing weekly or model-specific limit is omitted rather than
invented as zero or unlimited. Successful readings are cached in memory for two minutes; the
refresh endpoint bypasses that cache. Profile lookup is optional, so an unavailable plan label does
not hide valid quota windows. Enabled accounts follow the normal token-refresh ownership rules,
while viewing a disabled account never rotates its refresh token. A forced refresh saves one quota
reading in the same long-term observation dataset as response-derived readings.

Anthropic reports the serving account's five-hour window in the rate-limit headers of Messages
responses. Each completed successful request is stored as a content-free event tied directly to
that reset identity, with its account, model, response timing, utilization reading, ordinary input,
5m/1h cache creation, cache read, output, and completeness. A `429` is marked exhausted only when
the headers explicitly identify the five-hour window as exhausted; a new reset alone is not treated
as proof. The console shows actual observed API-price-equivalent value and never extrapolates a
partially used window. Non-exhausted windows remain available for export but are not presented as
complete-window calibration samples.

`GET /admin/v1/usage` also exposes `api_value_by_type_usd` for totals, model/account totals,
and five-hour windows, separating ordinary input, output, 5m writes, 1h writes, and reads.
Five-hour values apply the model price effective at each request's quota-observation time,
not the price at the end of the window. `observed_used_percent_delta` is the observed maximum
minus the first utilization reading, with no extrapolation to 100%. The optional
`api_value_usd_per_used_percent` is API-equivalent value per actual observed percentage point;
it is omitted for zero/missing utilization, mid-window starts, missing or incomplete usage,
or missing prices. `data_quality` flags these conditions and mixed-model windows. These are
observation metrics, not per-action subscription weights or guaranteed profit: actions in a
mixed window share one utilization reading and cannot be charged separate quota by this endpoint.

The export is a ZIP containing `manifest.json`, `windows.csv`, `events.csv`, and
`model_prices.csv`. Times are included as epoch values and UTC text. It contains account aliases and
billing metadata but no UUIDs, email addresses, credentials, prompts, or response content. Five-hour
events are retained until explicitly cleared; the console exposes export and clear as separate
actions so an operator can periodically archive a dataset for local model/cache-weight analysis.

Successful Messages responses are observed after transport-level compression is decoded. The
relay records only Anthropic's response `usage`, the serving account, model, and hour; it never
stores prompts or response content. Streaming `message_start` and cumulative `message_delta`
usage are combined, while incomplete streams remain visible as incomplete samples. Request
goroutines update an in-memory aggregate only. A background worker writes all pending account/model
hour buckets and request-level five-hour events in short SQLite transactions every five seconds and
retries failed batches, so SQLite work does not block model streaming.

Anthropic policy refusals are detected from `stop_reason: "refusal"` in either top-level JSON or an
SSE `message_delta`; output-token count is not used as a proxy because a stream can be refused after
partial output. The relay persists only hourly account/category counts and the latest observation
time—never `stop_details.explanation`, prompts, or response content. The account view reports a
rolling 24-hour hourly aggregate (the boundary bucket may extend by up to one hour). Refusals are
observational only: they do not cool down accounts, disable them, trigger failover, or alter routing.

The usage console values raw input, output, five-minute/one-hour cache creation, and cache reads
against versioned per-model prices. It derives token cache coverage as cache reads divided by all
reported input categories, and cache reuse as reads divided by cache creation for the selected time
range. Reuse is range-sensitive because a read may refer to a cache created before the range, or a
creation may be reused after it. These are traffic-flow metrics, not a live inventory of upstream
cache objects. Built-in prices are only defaults. Add an exact model ID or a
prefix ending in `*` through the console or `POST /admin/v1/usage/prices`; exact matches win, and a
new effective version preserves historical valuation. Unknown models keep their raw usage and are
reported as unpriced instead of silently contributing zero dollars. API value is an estimate at
published API prices, not evidence that Anthropic deducts subscription capacity in dollars.

The 5.5 defaults follow [Anthropic's published pricing](https://platform.claude.com/docs/en/about-claude/pricing)
as of 2026-10-08. Sonnet 5.5 defaults to $0.10/MTok for cache reads (5% of its $2/MTok input price).
Haiku 5.5 price entries contain lower-tier rates: $0.10 input, $0.50 output, $0.125
5m writes, $0.20 1h writes, and $0.01 reads per MTok. For each request whose total
input exceeds 100,000 tokens (ordinary input + both cache-write durations + reads),
all five rates are multiplied by 5, including output; exactly 100,000 uses the lower
tier. This published model rule also applies to operator-entered Haiku 5.5 base
price versions. The price-list API and `model_prices.csv` expose
`long_context_input_tokens` and `long_context_multiplier` for this rule.
Hourly statistics retain the long-prompt subset before aggregation, so clearing
five-hour observations does not erase new hourly pricing information. On upgrade,
old Haiku hours are reconstructed only when retained observations match every
hourly token/request counter; unmatched hours remain explicitly unpriced and are
reported in the startup log. Five-hour windows always classify individual requests
before aggregation. The usage API reports `unclassified_context_requests` alongside
`unpriced` for hourly totals affected by unknown prompt sizes. Price seeding preserves
operator-entered versions and never changes relay behavior, scheduling, or downstream
customer billing.

Disabled accounts cannot be selected automatically or through `X-Claude-Relay-Account`, and they
never trigger token refresh. Enabling an account means this relay becomes the sole owner of its
refresh-token chain. Stop managing that account in CLIProxyAPI before enabling it here.

Enabled accounts refresh on demand when their access token has less than five minutes remaining.
The access token and rotated refresh token are persisted together before the model request is sent.
Set `auto_refresh_enabled` to `false` for an emergency global refresh stop. Still-valid access
tokens continue to work; expired tokens return an explicit error.

## OAuth login API

The server-oriented PKCE flow uses authorization-code copy/paste so it does not depend on a callback
to the user's local computer:

```http
POST /admin/v1/oauth/claude/start
Content-Type: application/json

{"alias":"personal"}
```

Open the returned `authorization_url`, complete authorization, then submit the resulting code (or
callback URL):

```http
POST /admin/v1/oauth/claude/exchange
Content-Type: application/json

{"session_id":"...","code":"..."}
```

The OAuth session and PKCE verifier live in memory for 30 minutes. A completed login is imported as
disabled and must be enabled separately. Anthropic does not document this subscription OAuth flow
as a stable public API, so the real browser exchange still needs an interactive integration test
whenever its endpoints or client behavior change.

## Account selection

Two things are marked separately. The **key** decides which request policy is applied: requests
authenticated by `official_api_key` must have a recognized Claude Code shape or are rejected with
`403`; `relay_api_key` preserves third-party request semantics; and the optional
`experimental_api_key` applies experimental compatibility transforms. Every accepted official request has a
versioned `claude-cli/...` User-Agent, `X-Claude-Code-Session-Id`, `X-App: cli` or `cli-bg`, `anthropic-beta`,
and `anthropic-version`. Ordinary Messages additionally require structured Claude Code metadata and
either a billing attribution block or a recognized official system prompt. Native `count_tokens`,
the one-token Messages fallback used after a missing count endpoint, and the one-token Haiku probe
have narrower body requirements because the real client omits ordinary attribution there. The live
request view records the resulting request kind and presence booleans, never the identifying values.
This is a traffic policy, not proof that the caller is an authentic Anthropic binary.

The **account** carries a pool that decides which traffic may reach it, and permeability is one way:

| Ingress | May select |
| --- | --- |
| `official_api_key` | every enabled account, in either pool |
| `relay_api_key` | `compatible` accounts only |
| `experimental_api_key` | `compatible` accounts only |

New and upgraded accounts start in `compatible`, the shared placement, so the official ingress can
use them without any placement step. Move an account to `official` only to keep compatible traffic
off it. Claude Code-shaped traffic is what a subscription is expected to produce, so letting it use
every account costs nothing while the fence still keeps chosen accounts clean, and the official
ingress keeps the full account set for load spreading and failover.

The compatible ingress performs only relay-required authentication and attribution changes. It does
not rewrite third-party tool names. On the experimental ingress, custom tool names without the
subscription upstream's `mcp__` prefix are normalized before forwarding. The relay updates the
matching declaration, an explicit `tool_choice`, and prior `tool_use` blocks together. Typed
Anthropic server tools (such as `web_search_20250305`) and already-prefixed names are unchanged.
Tool names returned in non-streaming `tool_use` blocks and streaming `content_block_start` events
are restored through the exact request mapping; the relay never strips an untracked `mcp__`
prefix. A request that declares both `name` and `mcp__name` is rejected because the transformed
names would collide.
The two compatible policies use separate sticky-routing namespaces, so a conversation cannot cross
between transformed and untransformed traffic.

Changing an account's pool immediately clears its sticky bindings. Cooldowns and OAuth ownership
state remain with the account. The fence applies to every selection path, so `X-Claude-Relay-Account`
cannot be used to reach an `official` account from the compatible ingress.

The relay first honors an explicit private account alias, then an account UUID already present in
official-client metadata, then a persisted sticky binding. New explicitly identified sessions use
the healthy account with the lowest in-process active-request count that still has both an in-flight
slot and an active-session slot; the existing cache-affinity hash breaks ties. Cache affinity is
derived from the caller's existing request-level cache declaration or block-level breakpoint;
requests without one use tools, system, and the first user message as a stable selection anchor. An
explicit session binding has a sliding one-hour lifetime. A cache-prefix binding follows the longest
included `cache_control` lifetime: five minutes when `ttl` is omitted and one hour for
`ttl: "1h"`. A content-only fallback is not
persisted, so it remains deterministic when account loads tie but can rebalance when capacity
differs. Cache-prefix affinity is never treated as proof of a distinct client session.

The console can temporarily enable **quota-aware** routing for deployments whose Anthropic prompt
cache is shared by every account in the same workspace. An explicit `X-Claude-Relay-Account`
continues to win, but automatic account UUID, session, and cache-prefix affinities are bypassed.
The selector ranks live five-hour readings by `remaining percentage / seconds until reset`, so
unused quota that will expire soon receives traffic first. An account without a current reading is
sampled before known accounts; successful Messages response headers then supply its real balance.
The prefix hash only breaks equal-priority ties. Per-account in-flight limits, account pools,
cooldowns, and bounded failover remain active, while active-session admission and affinity writes
are skipped. Native `count_tokens` requests continue to use their independent least-loaded pool.
Switching back to `legacy` immediately restores the behavior above, and a process restart always
does the same.

An overloaded sticky request returns `429` without deleting, migrating, or temporarily bypassing
its binding. The in-flight counter and provisional new-session admissions are process-local because
the supported deployment is a single relay instance; successful bindings and their last activity
remain in SQLite.
The relay does not add, remove, or relocate caller cache controls.

Private deployments can force an account for one request:

```http
X-Claude-Relay-Account: personal
```

The alias is available to callers authenticated with either ingress API key, resolved only among the
accounts that key may reach, and stripped before forwarding upstream. A missing, disabled, or cooling
account produces an error instead of silently selecting another account. Successful responses include
`X-Claude-Relay-Account` so a trusted caller can inspect the selected alias. Anyone holding the
corresponding ingress API key can use this override, so aliases should not contain email addresses or
other sensitive data.

Transient `429`, `529`, network, upstream `5xx`, and token-refresh failures may move an unpinned
request to one other account at most once. Failover and account health are separate decisions:
`529` and generic `5xx` do not persist an account cooldown. A `429` follows an explicit exhausted
Anthropic window or `Retry-After`; without either, only that model is avoided for five seconds.
Successful responses clear matching stale cooldowns when their headers prove that the window is
allowed. Existing sticky sessions and explicitly selected requests never fail over to another
account because of a local admission limit.

## Request transformation

- An existing billing block or `metadata.user_id` is preserved.
- Unknown caller-owned billing fields are passed through without validation and have no effect on
  classification, account selection, pinning, or failover.
- Otherwise the relay prepends the smallest billing block demonstrated by the current tests and
  adds a JSON-string user identity with stable account/device identifiers.
- Token-count requests receive the same billing block but no metadata, because that endpoint's
  schema rejects metadata. The returned count therefore includes the billing text sent to models.
- `X-Claude-Code-Session-Id`, `X-Claude-Session-Id`, `X-Session-Id`, or `Session-Id` produces a
  stable pseudonymous session UUID. The official Claude Code header takes precedence. Without one,
  the cache-affinity routing key still preserves cache locality but does not count as an active
  client session.
- No Claude Code identity or software-engineering system prompt is added.

The relay supplies `anthropic-version: 2023-06-01` and `content-type: application/json` only when
the client omits them. It replaces downstream authentication with the imported OAuth token and
adds `?beta=true` to the upstream endpoint.

Each response includes `X-Claude-Relay-Request-ID`. A caller may supply a short ID containing only
letters, digits, dots, colons, underscores, or hyphens; otherwise the relay generates one. The ID
appears in request logs for correlation and is not forwarded to Anthropic.

## Request records

```http
GET /admin/v1/requests?limit=100
```

The relay keeps the last `request_log_size` requests in a fixed-size in-memory ring, 500 by default.
Each record holds only metadata: request ID, timestamp, ingress key, path, model, selected account, why that
account was selected, status, duration, the account a request failed over from, and a versioned
client-shape observation. That observation stores booleans for billing-block, structured
metadata, known entrypoint, version, Claude User-Agent, Claude Code session header, and `X-App: cli` or `cli-bg`
presence plus whether the relay passed the body through or added minimal attribution. The console
also shows whether the request used `messages` or `count_tokens`. Prompts, response bodies, raw
headers, billing values, metadata identities, and credentials are never recorded, nothing is written
to disk, and restarting the process clears the history. Set `request_log_size` to `0` to disable it
entirely.

When another gateway sits in front of the relay, pass the three Claude Code identification headers
without trusting them as authentication. New API channel header overrides can copy them explicitly:

```json
{
  "User-Agent": "{client_header:User-Agent}",
  "X-Claude-Code-Session-Id": "{client_header:X-Claude-Code-Session-Id}",
  "X-App": "{client_header:X-App}"
}
```

A failed attempt counts against the account that failed even when the retry succeeded elsewhere,
so a rate-limited account is visible in its own totals rather than hidden behind the failover.

## New API grouping

Create three Anthropic channels pointing at the same relay URL. Use `relay_api_key` for the stable
compatible group, `experimental_api_key` for the experimental compatibility group, and
`official_api_key` for the official group. On the official channel, preserve the three client
identification headers with the override shown above. Request-body pass-through alone does not
preserve them.

The official New API group should be issued only to Claude Code users. The relay rejects a request
that reaches the official key without the required shape before selecting an account, and does not
fall back to the compatible behaviour — assigning callers to the right group is the operator's job.
Conversely, neither compatible policy consumes an `official` account even when it supplies an
account alias. The relay never falls back from the stable compatible policy to experimental
transforms.

For temporary A6API platform health probes, toggle **A6API 探测放行** in the WebUI's
**接入** panel. Changes apply immediately and persist across restarts; the default is
disabled. On the authenticated official ingress only, a nonblank
`X-A6API-Probe-Id` or `X-A6API-Self-Test-Kind: pelican` skips Claude Code shape and CLI version checks. Classification remains
unchanged, each exception emits a warning without credentials or raw header values, and the
request still calls the real upstream using the official ingress account pool. The marker
does not authenticate the platform: any caller with an official ingress key can supply it.
Turn the switch off when the temporary exception is no longer needed.
In the New API channel header overrides, add
`"X-A6API-Probe-Id": "{client_header:X-A6API-Probe-Id}"` and
`"X-A6API-Self-Test-Kind": "{client_header:X-A6API-Self-Test-Kind}"` to forward the original markers.
When callers use `/v1/chat/completions`, New API must convert the request to the relay's
`/v1/messages` format (via its Anthropic channel).

The WebUI's `接入` panel can optionally set minimum and maximum Claude Code CLI versions for the
official ingress. The bounds are persisted in SQLite and apply immediately; an empty bound means
unrestricted. The relay compares the version in the downstream `claude-cli/x.y.z` User-Agent and
leaves compatible and experimental ingress behavior unchanged.
