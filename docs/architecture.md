# Architecture decisions

This file records decisions that shape the service beyond the dated Anthropic protocol findings.

## 2026-10-12: the model list is a cached upstream read

Claude Code fills its model picker from a gateway's `/v1/models` when gateway model discovery is
enabled, and other Anthropic clients list models the same way. The relay answers from Anthropic's
Models API rather than a maintained list, so new models appear without a build. The read uses the
first healthy accounts the caller's policy may select, keeping the pool fence and each account's
exit, and one result is shared per policy for an hour. Model reads always send
`anthropic-beta: oauth-2025-04-20` with the subscription bearer token.

It is not model traffic. Discovery sends no `X-App` or session header, so the official Claude Code
shape check does not apply; no admission slot, session binding, or request record is involved.
Without a successful upstream list, discovery returns `503 api_error` after at most two seconds
of waiting; the admin view shows an empty unavailable catalogue with any read error. An expired
upstream list remains available with source `stale` while refreshing, and failed refreshes are logged.
The list does not restrict Messages: requests for other models are forwarded unchanged.

There is no background polling. Reads happen only when a caller finds the list missing or expired,
or when an operator forces one from the console after a release; the forced read skips the cache age
and failure delay but uses the same accounts and exits.

## 2026-09-23: compatibility policies are separate from account placement

The relay exposes three model ingress policies: `compatible` preserves third-party request
semantics, `experimental` applies explicitly selected compatibility transforms, and `official`
admits only recognized Claude Code-shaped traffic. Tool-name normalization belongs only to the
experimental policy; the relay does not silently fall back from compatible to experimental
behavior.

Experimental tool-name adaptation is bidirectional and request-scoped. The request transform
records only names it actually changed, and response handling uses that exact map for non-streaming
`tool_use` blocks and streaming `content_block_start` events. It never strips an untracked prefix.
Declarations whose transformed names would collide are rejected before account selection because
there is no reversible mapping for them.

Ingress policy and account placement are independent axes. Both compatible policies may select
only accounts in the shared `compatible` pool, while the official policy may select accounts from
either stored pool. Adding an ingress transform therefore does not add a database account pool.
The storage selector receives one of two account-access policies—compatible-only or all—rather than
an ingress name.

Routing keys remain scoped by ingress policy. Identical client session IDs and cache prefixes on
the compatible and experimental keys cannot share sticky state because one request body may be
transformed and the other preserved. Metrics and availability keep the three ingress names so
operators can observe each contract independently.

## 2026-09-16: optional quota-aware routing for workspace-shared caches

Some enterprise workspaces have been observed to share prompt-cache entries across their member
accounts. Those deployments gain no cache-locality benefit from binding a prefix to one account;
the binding can instead strand most of an account's five-hour allowance just before its reset.
This is a deployment property, not a universal Anthropic contract, so quota-aware routing is a
runtime policy rather than a replacement for the default behavior.

The administration console switches between `legacy` and `quota_aware`. The value lives only in
process memory and starts as `legacy` after every restart. This deliberately avoids a schema
migration and guarantees that an operator can recover the established policy by restarting an
unhealthy rollout.

Quota-aware Messages selection keeps an explicit `X-Claude-Relay-Account`, pool fences, cooldowns,
per-account in-flight limits, and bounded failover. Automatic account UUIDs, pending sessions,
persisted session/cache bindings, active-session admission, and new affinity writes are bypassed.
The existing prefix hash remains only a deterministic tie-breaker. Native `count_tokens` uses its
separate least-loaded admission pool because it does not consume a Messages slot or create quota
affinity.

For every account with a live five-hour reading, the selector calculates
`remaining_percent / seconds_until_reset` and chooses the greatest value. This represents unused
allowance expiring per second rather than equal request counts. Accounts without a current reading
are selected before known accounts until a successful Messages response supplies their actual
window; missing data is never invented as zero or full allowance. In-flight load breaks equal
quota priorities and the hard limit prevents one urgent account from accepting unbounded
concurrency. Selection logs retain the remaining percentage, reset epoch, and calculated urgency
needed to explain routing decisions.

## 2026-07-30: multi-account without round robin

The service is server-first and supports multiple Claude subscription accounts from the storage
layer upward. It does not implement user billing, quota accounting, weighted distribution, account
groups, or a general scheduling platform.

Account selection uses the following precedence:

1. `X-Claude-Relay-Account` explicit alias override.
2. `account_uuid` from an official client's structured `metadata.user_id` when it matches an
   imported account.
3. A successful explicit-session or declared-cache binding while its respective lifetime remains.
4. The least-loaded enabled, non-cooling account that can admit a new session; rendezvous hashing
   of the cache-prefix key breaks load ties.

The cache-prefix key reads existing Anthropic request-level or block-level `cache_control` without
rewriting it. When no cache declaration exists, tools, system, and the first user message form a
deterministic selection anchor but are not persisted as a binding. Route keys stored in SQLite are
hashes and do not contain raw prompts or client session identifiers.

Each account owns its OAuth tokens, stable account/device attribution identity, enabled state, and
model-specific cooldowns. Selection therefore occurs before minimum subscription attribution is
added. Caller metadata account UUIDs are soft affinity hints and fall through to the remaining
selection stages when unavailable.

Transient failures permit at most one alternate account. Explicit account overrides do not fail over.

## 2026-08-13: routing affinity is not session activity

The console distinguishes three account signals that answer different operational questions:

- Sticky bindings are successful routing affinities. Explicit session IDs have a sliding one-hour
  lifetime; declared cache prefixes follow their five-minute or one-hour cache lifetime.
- Active sessions are only explicit session-ID bindings whose last successful request was within
  five minutes. A shared prompt prefix is useful affinity evidence, not proof of a user session.
- In-flight requests are the process-local reservations currently held through the end of the
  upstream response stream.

The five-minute view filters the existing `session_bindings` route-key namespace and reuses
`updated_at`; it adds no session table, write, or background worker. It is the persisted signal used
for explicit new-session admission, while prefix-only requests retain cache affinity without
consuming session capacity. In-flight requests remain a separate process-local control.

## 2026-08-24: declared cache lifetime bounds prefix affinity

Session identity and cache evidence have different lifetimes. An explicit session ID remains bound
for one sliding hour so a paused client can resume on the same account. Without a session ID, only
an actual request-level or block-level `cache_control` declaration earns a persisted binding. An
omitted cache `ttl` means five minutes; any included `ttl: "1h"` extends the account-level prefix
affinity to one hour because switching accounts would lose every cached segment on the original
account.

Tools, system, and the first user message still form a stable fallback selection hash when no cache
breakpoint exists. That hash also seeds relay-generated upstream session metadata, but it is never
read from or written to `session_bindings`: equal account loads remain deterministic, while real
capacity differences can rebalance the request. Cache affinity starts when upstream response
headers arrive rather than when a possibly long SSE stream finishes, so the binding cannot outlive
the declared upstream cache merely because generation was slow.

## 2026-08-24: strict session admission without five-hour pacing

The relay applies independent per-account controls. New explicitly identified sessions are
admitted while the account has fewer than five active sessions. A process-local provisional
reservation covers the first upstream request until its successful SQLite binding, so simultaneous
new sessions cannot all pass a stale count. Concurrent first requests for one route share its
reservation and account affinity. Existing bindings remain usable even when the account is at the
session limit; prefix-only cache affinities never enter session admission.

In-process Messages requests have a hard configurable limit (`max_inflight_per_account`, six by
default). The WebUI can persist a positive override in runtime settings; selectors share its atomic
value. A decrease preserves existing reservations and applies to subsequent admissions. No
database migration is needed. Native `count_tokens` calls use a separate per-account short-request limit
(`max_count_tokens_inflight_per_account`, 32 by default) and do not consume Messages or
active-session slots or create session bindings. New sessions may try another eligible account, but
an existing sticky session never
switches accounts because of either local control. A local limit returns `429`; `503` is reserved
for unavailable account capacity such as disabled or cooling accounts.

The downstream contract groups failures by caller action rather than scheduler internals. Every
normal local admission failure returns `429 rate_limit_error` with one stable capacity message;
whether an in-flight slot, a session slot, or all eligible accounts caused it remains in trusted
diagnostics. This avoids exposing account identity and prevents clients from depending on routing
implementation details.

Under the default legacy policy, Anthropic's unified five-hour utilization remains sampled from
successful response headers for the console and usage estimates rather than used as a routing
input. The optional workspace-specific policy in the 2026-09-16 decision supersedes that choice
only while an operator explicitly enables it.

Failover eligibility and persisted account health are deliberately separate. A `529` or generic
`5xx` may be request-scoped or service-wide, so it can cause the single bounded failover but never
writes an account cooldown. A `429` writes a window-length cooldown only when the corresponding
Anthropic headers explicitly report `rejected`, a surpassed threshold, or full utilization and
provide a plausible reset. Five-hour and general seven-day exhaustion apply to the whole account;
the `7d_oi` window applies only to the requested model. Otherwise an explicit `Retry-After` applies
to that model, and an ambiguous `429` avoids it for five seconds.

Any successful response proves that a short ambiguous or `Retry-After` avoidance for the same model
is stale. An explicit window status of `allowed` also clears the matching hard-window cooldown.
Cooldown observations are ordered in SQLite so an older concurrent success cannot erase a newer
decision. This recovery path is what returns a restored five-hour account to scheduling without a
manual cooldown release.

## 2026-07-30: activation owns OAuth refresh

The intended end state is full migration away from CLIProxyAPI rather than permanent shared
credential management. There is therefore one account switch instead of separate traffic-owner and
refresh-owner state:

- Imports, reimports, and OAuth logins always leave an account disabled.
- Disabled accounts neither receive traffic nor perform refresh operations.
- Enabling an account explicitly makes this relay the sole owner of that refresh-token chain.
- Enabled accounts refresh on demand within five minutes of access-token expiry and atomically
  persist both the new access token and the rotated refresh token.
- `auto_refresh_enabled` is a global emergency stop, not a second ownership model.

Schema version 2 intentionally disables all accounts once during upgrade so merely starting a new
binary cannot take over a credential that another process may still be refreshing.

The PKCE OAuth flow stores its state and verifier in memory for 30 minutes, accepts a copied
authorization code or callback URL, imports the credential as disabled, and never exposes tokens
through management responses. It deliberately excludes cookie-based login.

SQLite in WAL mode is the single-instance persistence boundary. The database holds OAuth secrets
and must live on a private server volume; the service requests owner-only file permissions on
platforms that support POSIX modes. Redis and horizontal multi-instance coordination are deferred
until there is a demonstrated need.

## 2026-07-30: single-container deployment boundary

The supported production topology is one relay process backed by one private SQLite volume. Docker
Compose is a packaging and lifecycle layer, not a horizontal scaling mechanism. The container runs
without root privileges or Linux capabilities, exposes a local health check, and publishes port
8567 on the host loopback interface by default. Public access belongs behind an HTTPS reverse
proxy; the application does not duplicate TLS certificate management.

The Docker build context is allowlisted to Go source and the non-secret container configuration so
local databases, OAuth credentials, captures, and developer configuration cannot enter the image.
Runtime secrets are supplied through environment variables. Windows tray integration is deferred
because it would add a second platform-specific lifecycle without improving the server-first goal.

## 2026-07-30: model ingress and administration keys are separate

Model callers and account operators have different authority. The compatible, experimental, and
official ingress keys authenticate native Anthropic message and token-count requests, including the private
`X-Claude-Relay-Account` override within the pools that ingress may reach. The administration key
authenticates
WebUI data, OAuth flows, account placement, and activation. No ingress key grants administration
authority, the administration key cannot call models, and configuration rejects identical values.

This remains a small private deployment rather than a user/role system. There is one key per ingress policy,
no billing identity, and no per-caller quota. Splitting the keys prevents an API consumer from
taking ownership of OAuth refresh tokens without introducing a general access-control platform.

## 2026-07-31: SQLite remains the single-instance state store

Redis is not part of the supported single-server topology. Adding it would introduce another
authenticated service, backup policy, expiry model, and consistency boundary without removing the
need for durable OAuth credential storage. SQLite in WAL mode already covers the current account,
cooldown, and sticky-binding workload with one private volume.

If horizontal replicas become a demonstrated requirement, persistence and refresh ownership must
be redesigned together. A durable database such as PostgreSQL would own accounts and OAuth token
rotation; Redis could then be considered only for short-lived routing state and coordination. It
must not be added independently as a partial multi-instance workaround.

## 2026-07-31: the console reports routing state, not just stored state

The first WebUI only rendered what the `accounts` table stores, so `enabled` was presented as if it
meant "receiving traffic". It does not: an enabled account can be excluded by an active cooldown,
and an account holding sticky bindings behaves differently from an idle one. The console now reads
the live routing inputs — cooldowns, sticky binding counts, and token expiry — and derives a single
status per account. Operator actions cover the full lifecycle that previously required SQLite or
shell access: paste-import, rename, delete, forced refresh, cooldown release, and a connectivity
check.

Two ownership rules stay enforced on the server rather than in the interface. A forced refresh
requires an enabled account and an active global refresh switch, because refreshing is exactly what
account activation grants. A connectivity check never refreshes, so a disabled account can be
verified without this relay taking over its refresh-token chain.

Deleting an account removes its cooldowns and sticky bindings through the existing foreign keys. It
does not revoke the Anthropic authorization, and the console says so at the confirmation step.
Pasted imports reject an existing alias or account identity by default. Replacement requires an
explicit API flag and a second console confirmation because it changes credentials and disables the
account immediately.

Schema version 3 adds `accounts.last_refresh_at` so a healthy account is distinguishable from one
whose refresh chain silently stopped rotating. The upgrade only adds a column and changes no
account state.

## 2026-07-31: request records are bounded, in-memory, and metadata only

Routing was previously unobservable: cache affinity, sticky sessions, and bounded failover decided
which subscription served a request, and nothing surfaced those decisions. A fixed-size ring of
request records now backs the console, holding request ID, timestamp, path, model, selected account,
selection source, status, duration, and the account a request failed over from.

The ring stores no prompts, response bodies, headers, or credentials, is never persisted, and is
cleared on restart. `request_log_size` bounds it and `0` disables it, so the feature cannot grow
into an unbounded local log. Container logs remain the retained record; this is a live view.

A failed attempt is attributed to the account that failed even when the retry succeeded on another
account. Without that, the account being rate-limited would be invisible in every per-account total
while a healthy account absorbed its traffic.

## 2026-07-31: client-shape classification is observable and policy-scoped

The relay classifies the raw incoming request before transforming it. Version 5 first requires a
versioned `claude-cli` User-Agent together with `X-Claude-Code-Session-Id`, `X-App: cli` or `cli-bg`,
`anthropic-beta`, and `anthropic-version`. It then distinguishes ordinary Messages, native
`count_tokens`, the non-streaming `max_tokens: 1` Messages fallback observed after a missing count
endpoint, and the one-token Haiku connectivity probe. Ordinary Messages additionally require valid
Claude Code metadata and either a complete billing attribution block or a recognized official
system prompt. Partial evidence is `ambiguous`; no evidence is `compatible`.

The in-memory request record stores the request kind, presence booleans, and the relay action only.
It never stores the raw billing values, User-Agent, account/device/session identities, prompts, or
response content.
Classification does not authenticate an official client. It is enforced only as the admission
policy for the official ingress; compatible ingress forwarding remains independent of the result.

Response content is not retained. This section's earlier decision to defer usage accounting was
superseded on 2026-08-09 by a byte-preserving response usage observer described below.

## 2026-08-06: account pool permeability is one way

Supersedes the 2026-07-31 decision that made the two pools mutually isolated.
The 2026-09-23 decision later split compatible request handling into stable and experimental
policies without changing the two account pools or their one-way permeability.

At the time of this decision the deployment served two traffic policies without turning account
selection into a general group scheduler. Format admission belonged to the key: the compatible
ingress accepted ordinary native Anthropic requests, and the optional official ingress accepted
only requests classified as `cc_candidate`. The classification is a policy signal, not
authentication of the Claude Code executable; possession of an ingress key remains the actual
authentication boundary.

Account placement is a separate axis, and the two traffic shapes carry asymmetric risk.
Claude Code-shaped traffic is the shape a subscription is expected to produce, so the official
ingress may select from every pool. The compatible ingress is fenced to the compatible pool.
An official-pool account therefore never serves a non-Claude-Code request, while the official
ingress keeps the full account set for load spreading and bounded failover instead of stranding
idle capacity behind a partition.

Two-way isolation was rejected because it bought nothing the fence does not already buy. It halved
the accounts available to the dominant traffic class, capped the load-aware routing added on
2026-08-02, and returned an explicit unavailable error whenever the official pool was empty — which
was the default state after an import.

The fence is enforced in every account lookup, so explicit aliases, structured account UUIDs,
sticky bindings, rendezvous candidates, and bounded failover all respect it. Routing keys stay
scoped per ingress so different key holders are treated as different clients and cannot collide on a
sticky binding. Moving an account clears its bindings in the same SQLite transaction, while
preserving its enabled state, cooldowns, and OAuth tokens. Existing and newly imported accounts
default to `compatible`, which is now the shared placement; the stored column and its two values are
unchanged, so this decision needs no schema migration.

There is one API key per ingress rather than arbitrary named groups. The later experimental ingress
keeps this property while avoiding group administration, per-user ACLs, quota logic, or weighted
routing.

## 2026-07-31: subscription usage is a cached management observation

Account capacity is read from Anthropic's private OAuth usage endpoint rather than estimated from
relayed token counts. Only named windows actually returned upstream are exposed; absent weekly or
model-specific fields carry no inferred meaning. The optional profile request may add a plan label
but cannot make an otherwise valid usage reading fail.

Successful results live in a two-minute in-memory cache, but the console never reads or refreshes
them automatically. Only an explicit per-account action calls the cache-bypassing refresh endpoint,
so opening the console and its five-second operations poll generate no Anthropic usage requests.
Ordinary cached reads are not persisted. An explicit refresh writes one quota observation as
described in the 2026-08-26 decision. Legacy routing does not consume it; the optional quota-aware
policy may use the live sampler reading populated by that explicit refresh.

Usage reads share the configured outbound proxy. Enabled accounts may use the existing synchronized
OAuth refresh path when their access token is near expiry. Disabled accounts are read only while
their current access token remains valid, preserving the rule that observation cannot take over a
refresh-token chain.

## 2026-08-26: five-hour capacity becomes a long-term event observation

This supersedes the five-hour cumulative-snapshot estimator in the 2026-08-09 decision. The old
format attached a response-header utilization reading to relay-wide cumulative totals captured only
after a streaming body completed. Concurrent responses could therefore share counters, finish out
of header order, or cross a reset boundary. Grouping by reset identity kept ordinary windows
separate, but it could not make those counter deltas request-exact, so linear full-window
extrapolation was more precise-looking than the evidence allowed.

Each successful Messages response now produces one content-free event after its usage observer
finishes. The event retains its request key for idempotence, serving account, normalized upstream
reset identity, header and completion times, HTTP status, model, utilization reading, ordinary
input, five-minute and one-hour cache creation, cache read, output, usage presence, and stream
completeness. Events are batched with the existing five-second accounting worker. Missing usage or
missing quota headers remain explicit quality facts instead of causing an event to disappear.

A five-hour window is identified directly by `(account_id, resets_at)`. It is marked exhausted only
from explicit upstream evidence: utilization at 100%, a rejected status, or a surpassed-threshold
flag on the five-hour headers. This mark is recorded before bounded failover can hide the account
that produced the `429`. Opening a new reset window does not imply that the previous window was
exhausted. Manual OAuth refreshes add quota-reading events but no synthetic token totals.

The console values the events actually observed in current windows and explicitly exhausted
windows using versioned API prices. It does not extrapolate partially used windows and does not
assume that API price ratios are subscription quota weights. Non-exhausted historical windows stay
in the dataset but are excluded from exhausted-window analysis by default.

Observation data has no automatic retention horizon. The operator can export a versioned ZIP with
window, event, and model-price CSV files, then clear this dataset independently of hourly usage.
Exports contain account aliases and billing metadata only, never account UUIDs, emails,
credentials, prompts, or response content. Schema version 8 drops the obsolete snapshot table; its
rows were live estimates and were explicitly declared disposable before this migration.

## 2026-08-09: response usage is observed and persisted as aggregates

The relay now measures API-price-equivalent value without treating dollars as Anthropic's internal
subscription unit. It observes only billing metadata from successful Messages responses. The relay
does not forward the caller's `Accept-Encoding` upstream: Go's HTTP transport negotiates and
decodes gzip so the observer never mistakes compressed SSE for a stream without usage. The decoded
response continues through the existing streaming copy; the observer parses `message_start`, the
cumulative `message_delta`, and `message_stop`, skipping content deltas without decoding them.
Non-streaming JSON is inspected incrementally with a bounded two-megabyte tail. Neither path stores
prompt or response content. A successful Messages response without recognizable usage emits a
rate-limited diagnostic warning.

The hot path adds completed usage to an in-memory map keyed by UTC hour, account ID, and serving
model. Every five seconds a background worker swaps that map and performs one short SQLite
transaction. Failed writes are merged back for retry, shutdown performs a final flush, and clearing
statistics is serialized against flushing. This preserves the Store's single-connection invariant
and avoids a database commit per request. Incomplete streams may retain partial usage but are
counted explicitly; samples with no upstream usage are not invented.

Hourly rows retain raw token categories rather than only a calculated dollar value. Model prices
are separate, versioned rules with effective timestamps; exact IDs take precedence over `*` suffix
prefix rules. Unknown models remain unpriced while their raw usage stays available. Default prices
seed a fresh schema but UI-added versions are data, so new models and price changes need no build.

The response observer also made five-hour analysis possible. Its original cumulative-snapshot
estimator has since been removed; request-level attribution, explicit exhaustion criteria, export,
and retention are defined by the 2026-08-26 decision above.

## 2026-09-23: policy refusals are account observations, not routing failures

The response observer recognizes Anthropic refusals only from `stop_reason == "refusal"`, including
the `delta.stop_reason` field in streaming `message_delta` events. Output-token count is deliberately
not part of detection because a streaming refusal may follow partial output. The in-memory request
record marks the refusal separately from HTTP/transport failure.

Durable data is limited to UTC-hour counts keyed by account and category plus the last observation
time. The upstream explanation, prompts, and response content are not stored. Account summaries use
a rolling 24-hour query over these hourly rows, so the boundary category bucket can include up to
one earlier hour. A refusal adds an account to the console's attention view, but it never changes
selection, failover, cooldown, or account-enabled state.

## 2026-08-01: CCH has no relay semantics

The supported official path is Claude Code configured with a third-party API URL. Captures of that
path showed the complete three-header identity shape and no CCH. Model acceptance tests also did
not require CCH. The relay therefore does not generate, calculate, validate, record, or route on
CCH, and the obsolete offline signer has been removed.

Caller-owned billing text remains opaque request content: existing fields are preserved, but they
cannot grant official-ingress admission, bind an account UUID, pin a request, or disable bounded
failover. This keeps the native Anthropic body transparent without retaining a second, unverified
client protocol inside the routing layer.
