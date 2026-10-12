# Current protocol findings

Last reviewed: 2026-07-31

This is the compact handoff document for future work. It records what was observed, what was
experimentally accepted, and what remains an inference. Recheck it whenever Claude Code or the
available model generation changes.

## Evidence labels

- **Capture:** observed in an exact or visually inspected official-client request.
- **Acceptance test:** changed one request field and recorded the upstream response.
- **Binary inspection:** observed in the official packaged Claude Code executable.
- **Source inspection:** observed in current third-party client source but not yet confirmed by our
  own live request.
- **Inference:** best current explanation, not an Anthropic contract.

## Stable native API behavior

- **Acceptance test:** `anthropic-version` is required by the normal Messages API. The relay adds
  `2023-06-01` only when absent.
- **Acceptance test:** `content-type: application/json` is required and added only when absent.
- **Acceptance test:** a Haiku 4.5 token-count request reported 8 input tokens without attribution
  and 41 with the current minimum billing block, so that block contributes 33 input tokens.
- **Acceptance test:** `/v1/messages/count_tokens` rejects `metadata` with
  `metadata: Extra inputs are not permitted`. The relay therefore adds billing attribution but not
  metadata on that endpoint.
- The relay supports native Anthropic request and response shapes only.

## Model acceptance matrix

Observed on 2026-07-30 using one Claude subscription OAuth credential:

| Model | Minimum successful subscription attribution observed |
| --- | --- |
| Haiku 4.5 | Native API request; no billing block or metadata required |
| Sonnet 5 | Billing block with `cc_version` + `cc_entrypoint`, plus structured `metadata.user_id` |
| Opus 5 | Billing block with `cc_version` + `cc_entrypoint`; metadata not required |

These are acceptance observations, not guaranteed model contracts. A successful HTTP response also
does not by itself prove how Anthropic classified subscription usage.

## Billing attribution

- **Capture:** official Claude Desktop 2.1.219 Sonnet 5 and Opus 5 requests used:

  ```text
  x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=<5 hex>;
  ```

- **Acceptance test:** the smallest successful synthetic block contained only `cc_version` and
  `cc_entrypoint`. The reserved prefix or either field alone was rejected.
- **Acceptance test:** Sonnet 5 and Opus 5 both accepted the minimum block with
  `cc_entrypoint=claude-desktop-3p` through the deployed third-party API path.
- **Upstream rejection observed 2026-09-23:** Opus 5.5 rejects synthetic attribution identifying
  Claude Code 2.1.219 and requires version 2.1.280 or newer.
- The relay therefore uses the minimum supported `2.1.280` identifier and
  `cc_entrypoint=claude-desktop-3p` for synthetic requests. Unlike the historical capture above,
  this is a compatibility floor rather than a captured full build identifier and must be revisited
  after later client or model changes.
- A third-party algorithm claiming to derive the final three hex characters from message content
  produced `68d` for both exact captured requests, while the official value was `0a7`. It is not
  used here.

## Claude Code with a third-party API URL

- **Capture:** Claude Code 2.1.219 using a New API URL sent 26 successful `/v1/messages` requests
  for one interaction. Every request carried a `claude-cli/2.1.219` User-Agent,
  `X-Claude-Code-Session-Id`, and `X-App: cli`.
- **Capture:** only 2 of those 26 request bodies contained a billing block. Both were large main
  requests and used `cc_entrypoint=claude-desktop-3p`; none of the 26 bodies contained `cch=`.
- **Capture:** a small Haiku helper request contained a JSON-string `metadata.user_id` with
  `device_id` and `session_id`, but an empty `account_uuid`, and no billing block.
- **Capture 2026-10-04:** Claude Code 2.1.289 used `X-App: cli-bg` for the main `/fork`
  background-session request and its status-classification helper; foreground requests used `cli`.
  The background main request retained structured metadata, billing attribution, and the official
  system prompt. PrismCat captured these before New API without header or body overrides.
- **Current classification policy:** classifier v5 accepts `X-App: cli` or `cli-bg` and requires
  the User-Agent and session headers above plus non-empty
  `anthropic-beta` and `anthropic-version`, then applies request-type-specific body checks. Ordinary
  Messages require structured metadata and a billing block or recognized official system prompt.
  Native `count_tokens`, the observed one-token Messages count fallback, and the one-token Haiku
  probe have deliberately narrower body checks. These are observable, spoofable request-shape
  signals, not client authentication.
- **Capture:** the tested New API deployment returned 404 for
  `/v1/messages/count_tokens?beta=true`. Claude Code continued, but native count-token
  compatibility through that gateway remains unresolved.
- **Capture:** Claude Code 2.1.247 followed those 404 responses with non-streaming Sonnet Messages
  requests carrying `max_tokens: 1`, structured metadata, the complete official header set, and no
  system or cache declaration. Classifier v4 records these separately as `token_count_fallback`.

## `metadata.user_id`

- **Capture:** the official Desktop value is a JSON string containing `device_id`, `account_uuid`,
  and `session_id`.
- **Acceptance test:** Sonnet 5 accepted the real value, a random value with the same JSON shape,
  and CLIProxyAPI's legacy structured string. The embedded account UUID did not have to equal the
  subscription account UUID in the tested request.
- **Current policy:** preserve a caller value. Otherwise use the imported real account UUID when
  available, a persistent random device ID, and either a stable header-derived session UUID or a
  per-request random session UUID.
- The fields contain pseudonymous identifiers only; never derive them from an access token, email,
  or request text.

## CCH

- **Capture:** official Desktop 2.1.219 still emits a five-hex CCH for both Sonnet 5 and Opus 5.
- **Binary inspection:** the 2.1.219 attribution builder still creates `cch=00000` for qualifying
  first-party OAuth or Vertex paths. The condition is based on provider/auth path, not the
  `cli` versus `claude-desktop` entrypoint label.
- **Acceptance test:** Haiku 4.5, Sonnet 5, and Opus 5 all succeeded without CCH when their other
  model-specific requirements were met.
- Exact raw bodies recovered from mitmweb matched captured `Content-Length`, yet the older seeded
  xxHash64 algorithm did not reproduce either official CCH. The algorithm changed by 2.1.219.
- sub2api removed CCH based on an unrelated issue reference and has not introduced a replacement
  algorithm as of its 2026-07-29 main branch.
- **Current policy:** CCH is an opaque caller-owned billing field. The relay does not generate,
  calculate, validate, record, classify, bind accounts, or alter failover based on it. Existing
  billing text is preserved as ordinary request content.
- **Inference:** CCH may currently be a soft signal for client attribution, telemetry, staged
  validation, or abuse/risk scoring rather than a hard admission check. Accepting a missing or
  incorrect value is consistent with those uses and with backward compatibility, but no available
  evidence identifies Anthropic's actual purpose. Public adoption of third-party relays is not
  proof of this intent.

## Official prompt and title behavior

- **Capture:** official Desktop requests include their own Claude Code prompt, tools, cache
  breakpoints, thinking, and context-management fields. The relay does not recreate or replace
  those fields.
- **Capture:** title generation uses a separate Claude Web endpoint under
  `/api/organizations/{organization}/dust/generate_title_and_branch`, currently selecting Sonnet 5.
  Chinese input can produce a Chinese title. This endpoint is outside the relay scope.

## Implemented transformation contract

1. Preserve existing billing and metadata fields, including unknown billing fields.
2. Add only a missing minimum billing block and missing structured metadata.
3. Preserve all original system instructions and messages; never inject Claude Code behavioral
   instructions.
4. Keep the transformer idempotent.

For an ordinary request, `system` is normalized as follows:

| Incoming `system` | Upstream `system` |
| --- | --- |
| Missing, `null`, or empty string | `[billing block]` |
| String | `[billing block, original text block]` |
| Array | `[billing block, ...original blocks]` |

This normalization is not applied when a billing block already exists. Unknown fields in that
block are not interpreted by the relay.

For `/v1/messages/count_tokens`, the same system normalization is applied so its result includes
the billing block that the corresponding Messages request would send. Metadata is not added because
the count-tokens schema rejects it, and metadata does not contribute prompt tokens.

## OAuth subscription usage surface

- **Source inspection:** the current CLIProxyAPI Management Center requests
  `GET /api/oauth/usage` with the account bearer token and
  `anthropic-beta: oauth-2025-04-20`. It separately requests `GET /api/oauth/profile` for the plan
  label. These are private OAuth surfaces, not documented public Anthropic APIs.
- The observed usage schema has optional named windows: `five_hour`, `seven_day`,
  `seven_day_oauth_apps`, `seven_day_opus`, `seven_day_sonnet`, `seven_day_cowork`, and the internal
  `iguana_necktie` field used for Fable. Each named window carries utilization and an optional reset
  time. `extra_usage` may carry its enabled state, monthly limit, used credits, and utilization.
- **Current policy:** expose only fields actually returned. Absence of a weekly or model-specific
  window is not interpreted as either zero capacity or unlimited capacity. Profile failure does not
  invalidate a successful usage response.
- The relay keeps successful readings in memory for two minutes and never uses them for routing,
  billing, cooldowns, or account activation.
- **Plan label:** `has_claude_max` and `has_claude_pro` identify personal plans. Organization plans
  leave both false and are identified by `organization_type` together with an active
  `subscription_status`, so the organization check must precede the free check. Only `claude_team`
  has been confirmed against a live account; an unrecognized `organization_type` is reported under
  its own name rather than discarded, because a plan this build has not seen is still worth showing.
  An Enterprise account was observed in the desktop client on 2026-08-04 while the relay reported an
  empty plan, which is what prompted the passthrough.

## Model discovery

- **Binary inspection:** Claude Code 2.1.295 gateway model discovery runs only when
  `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY` is set, the provider is first-party, and
  `ANTHROPIC_BASE_URL` is not an Anthropic host. It sends `GET /v1/models?limit=1000` with
  `Authorization: Bearer` (`ANTHROPIC_AUTH_TOKEN` or `apiKeyHelper`) and/or `x-api-key`, plus
  `anthropic-version` and the CLI User-Agent, but no `X-App` or session header. The default timeout
  is 3000 ms (`CLAUDE_CODE_GATEWAY_MODEL_DISCOVERY_TIMEOUT_MS`).
- **Binary inspection:** the response must contain `data` entries with a string `id` and optional
  `display_name` and `description`. IDs not matching `/(claude|anthropic)/i` are dropped, and the
  result is cached in `~/.claude/cache/gateway-models.json` together with the base URL.
- **Binary inspection:** subscription sessions receive extra picker entries from the private
  `GET /api/claude_cli/bootstrap` (`additional_model_options`). A capability fetch that calls the
  Models API with `anthropic-beta: oauth-2025-04-20` exists in 2.1.295 but is switched off.
- **Acceptance test 2026-10-12:** a Claude subscription OAuth access token with
  `anthropic-beta: oauth-2025-04-20` read the public `GET /v1/models?limit=1000` in one page:
  14 models from `claude-haiku-5-5` (newest) to `claude-sonnet-4-5-20250929`, each with
  `capabilities`, `lifecycle`, `line`, `max_input_tokens`, and `max_tokens`. No Mythos model was
  listed. Claude Code 2.1.295 with discovery enabled cached all 14 through the relay.

## Deferred questions

- Recheck the model matrix after client/model releases and distinguish validation responses from
  genuine overload/rate-limit responses.
- Verify subscription-side usage classification in addition to HTTP success.
- Revisit the captured billing version constant when the official client changes.
- Decide whether to add native Anthropic count-token routing to the New API deployment.
- Confirm the private usage/profile response shapes against a live subscription and recheck them
  whenever the management UI or Anthropic client changes.
- Capture the exact `organization_type` an Enterprise account returns. The current passthrough makes
  it visible in the console without a guess, so record the observed value here once seen.
