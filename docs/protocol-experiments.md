# Protocol experiments

The relay must preserve normal Anthropic API semantics. It will not add a full Claude Code
prompt unless evidence shows that a specific upstream requirement cannot be satisfied in a
smaller way.

## Standard API requirements

These are normal Anthropic API requirements, not subscription or Claude Code mimicry:

- `anthropic-version` is required. The relay supplies `2023-06-01` only when absent.
- `content-type: application/json` is required and supplied only when absent.
- Upstream authentication is supplied by the relay.

## Baseline

Replay a captured Claude Code request through the relay without changing its body. Confirm:

1. The upstream returns a successful response.
2. Streaming and non-streaming responses are relayed unchanged.
3. The subscription records usage as expected.

The Pretty-view copies are valid JSON but are not byte-identical to the wire request. Exact raw
bodies were later recovered directly from mitmweb and matched their captured `Content-Length`.
They confirm a Claude Desktop entrypoint, a five-digit CCH, and JSON-string metadata containing
`account_uuid`, `device_id`, and `session_id`.

## Official client captures after Opus 5 release

Fresh Claude Desktop 2.1.219 captures from 2026-07-30 show that both Sonnet 5 and Opus 5 still
send the attribution block as the first system item. Both use this shape:

```text
x-anthropic-billing-header: cc_version=2.1.219.<build>; cc_entrypoint=claude-desktop; cch=<5 hex>;
```

Both requests also include JSON-string `metadata.user_id` values with
`device_id/account_uuid/session_id`, four system blocks, 36 tools, adaptive summarized thinking,
context management, and `output_config.effort`. The new headers add the
`mid-conversation-system-2026-04-07` and `effort-2025-11-24` betas compared with the earlier
2.1.215 capture.

The client continuing to emit CCH is evidence that the field remains part of the official wire
format even though the controlled acceptance tests below show that the tested model paths do not
currently require it. Exact raw Sonnet and Opus bodies matched their captured byte lengths, but the
older seeded xxHash64 algorithm did not reproduce either value. Online rewriting was therefore
disabled. That earlier policy preserved a body carrying CCH byte-for-byte; it was superseded on
2026-08-01 when CCH was removed from classification, routing, pinning, and failover decisions.

Title generation has moved to a separate Claude Web endpoint under
`/api/organizations/{organization}/dust/generate_title_and_branch`. The observed request selects
Sonnet 5 and sends `first_session_message` plus `title_style`; the response contains `title` and
`branch_name`. A Chinese first message produced a Chinese title, while the branch name remained a
machine-friendly non-CJK string. This endpoint is a product-side Web API and is outside the relay's
native Anthropic Messages scope.

## Minimal request matrix

Start from one captured request and change one variable at a time. Early tests recomputed CCH
after every body change; later tests explicitly disabled signing to determine whether the upstream
validates it.

| Case | Billing block | CCH | `metadata.user_id` | Claude Code identity prompt |
| --- | --- | --- | --- | --- |
| A | present | valid | present | absent |
| B | present | valid | absent | absent |
| C | present | absent | present | absent |
| D | present | invalid | present | absent |
| E | absent | absent | present | absent |
| F | present | valid | present | present |
| G | absent | absent | absent | absent |

### Observations

- Case G returned HTTP 200 with Haiku 4.5 on 2026-07-30 after the relay supplied only the standard
  `anthropic-version`, `content-type`, and OAuth authorization headers. The request contained
  ordinary system and user text, with no billing block, CCH, metadata, or Claude Code identity
  prompt. Response usage reported 33 ordinary input tokens and no cache creation/read tokens.
- The same minimal shape returned `rate_limit_error` with tested Sonnet and Opus models. This
  proves the requirement is model-dependent: the billing attribution block is not universally
  required for successful inference.
- Adding only the captured `metadata.user_id` to the minimal Sonnet 5 request still returned
  HTTP 429 `rate_limit_error`. Metadata alone does not pass the model gate.
- Adding a billing block containing `cc_version`, `cc_entrypoint`, and a correctly signed `cch`
  changed Sonnet 5 from the opaque HTTP 429 to HTTP 529 `overloaded_error` in three attempts.
  A contemporaneous request without the block still returned the opaque HTTP 429.
- Adding both the billing block and `metadata.user_id` made Sonnet 5 succeed (HTTP 200). An
  interleaved `without metadata -> with random metadata -> without metadata` test returned
  `529 -> 200 -> 529`. On this request path, the 529 is therefore another opaque validation/gating
  response rather than evidence of actual model overload.
- Sonnet 5 accepted all of these `metadata.user_id` values when combined with the billing block:
  the real captured JSON string, a random JSON string with the same
  `device_id/account_uuid/session_id` shape, and CLIProxyAPI's generated
  `user_<64 hex>_account_<UUID>_session_<UUID>` shape. The value does not need to contain the real
  subscription account UUID. A short unstructured value reached 529 in an earlier attempt, but
  that attempt coincided with transient 529s for a structured value and is not conclusive.
- The same signed billing-only request succeeded with Opus 5 (HTTP 200). It contained no
  `metadata.user_id`, Claude Code identity prompt, or `anthropic-beta` header.
- With CCH signing disabled, Opus 5 also succeeded with literal `cch=00000`, and then succeeded
  after the `cch` field was removed entirely. Sonnet 5 likewise succeeded twice with CCH signing
  disabled and the `cch` field absent, provided that both the billing block and a structurally valid
  `metadata.user_id` were present. Current upstream behavior therefore does not require a CCH value
  for either tested model path.
- A block containing only the reserved `x-anthropic-billing-header:` prefix was rejected with
  HTTP 400. Blocks containing only `cc_version` or only `cc_entrypoint` were rejected the same
  way. A block containing both fields succeeded. The smallest structure demonstrated so far is:

  ```text
  x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop;
  ```
- `anthropic-beta: oauth-2025-04-20` and adding `claude-code-20250219` did not change the opaque
  Sonnet/Opus failure when the billing block was absent.

These observations describe behavior seen on 2026-07-30, not a documented Anthropic contract.
Subscription usage classification still needs separate verification.

Record both HTTP behavior and subscription usage classification. A successful HTTP response
alone does not prove that the request followed the intended subscription path.

## Implemented transformation rule

The current evidence supports an ordinary-client transformer with this idempotent policy:

- Preserve an existing billing block; otherwise prepend the demonstrated minimum block containing
  `cc_version` and `cc_entrypoint`.
- Preserve a `metadata.user_id`; otherwise synthesize the captured JSON-string shape. Account and
  device identities are stable; a downstream session header is mapped stably, while a stateless
  request without one receives a fresh session UUID.
- Preserve every other system block and message in its original order.
- Preserve existing billing text as caller-owned content, but do not give CCH any classification,
  routing, pinning, or failover semantics.
- Never add Claude Code identity or software-engineering instructions.

## 2026-10-24: tool-name classification and the extra-usage 400

### Scope and controls

Live probes ran on RN from 02:57 to 03:07 Asia/Shanghai through the existing relay,
deployed image revision `125e4edb31956858e22511f3188e011677e15f96`. No deployment,
configuration, routing-policy, or account-placement changes were made. The suite sent
115 serial synthetic Messages requests: 76 HTTP 200, 38 instances of the target
extra-usage HTTP 400, and one unrelated tool-choice HTTP 400.

The main matrix pinned one enabled account and `claude-opus-5-5`; selected name
contrasts were repeated on a second account. The `weather` / `mcp_weather` /
`mcp__weather` contrast was also checked on Sonnet 5.5 and Haiku 5.5 on the first
account. Exa-specific contrasts were tested on Opus 5.5 on both accounts.

Requests used an identical empty-object custom-tool schema and description, a short
ordinary system prompt, and a synthetic user message. Ordinary probes capped output
at 16 tokens and asked the model not to call tools. Dedicated synthetic tool-call
probes used larger output caps. Only names changed within each naming contrast.
The compatible ingress still supplied its normal minimum billing/metadata attribution;
these are not tests of a completely unmodified request sent directly to Anthropic.

Reproducible harness and JSONL results are under the ignored directory
`data/protocol-research-2026-10-24/`. Docker credentials were read only into remote
memory and were not printed, downloaded, or stored in results. No customer prompts
or database snapshots were exported. Each result records synthetic request overrides,
response bodies, upstream request IDs, and selected quota headers.

### Observed name boundaries

The target response was HTTP 400 `invalid_request_error` with this message:

```text
Third-party apps now draw from extra usage, not plan limits. Ask your workspace admin to add more and keep going.
```

| Custom tool name or list | Result on Opus 5.5 |
| --- | --- |
| `web_search_exa` | Target 400; repeated on both accounts |
| `get_code_context_exa` | Target 400 on both accounts |
| `Web_search_exa`, `WebSearchExa`, `web_search_Exa` | 200 |
| `web_search_exa2`, `web_search_exa_test`, `my_web_search_exa` | 200 |
| `web_search`, `web_fetch_exa`, `web_search_foo`, `web_search_google` | 200 |
| `exa_web_search`, `code_search_exa` | 200 |
| `weather`, `get_weather`, `getWeather`, `foo`, `read_file` | 200 |
| `mcp_weather`, `mcp_get_weather`, `mcp_web_search_exa` | Target 400 |
| `mcp__weather`, `mcp__get_weather`, `mcp__web_search_exa` | 200 |
| `mcp`, `mcp-weather`, `Mcp_weather`, `mcpX_weather`, `my_mcp_weather` | 200 |
| `mcp___weather`, `mcp__` | 200; no claim that these are canonical MCP names |
| `sessions_list`, `cc_sess_list`, `fetch_wea00` | 200 |
| 1, 2, 5, 6, or 12 tools named `get_weather0`, `get_weather1`, ... | 200 |
| A passing `weather` plus a failing `mcp_weather` | Target 400 |

The spelling/case contrasts support a case-sensitive name-matching gate. The two
rejected Exa names behave like exact-name matches within the tested neighborhood;
the tested single-underscore `mcp_` names suggest another prefix-sensitive rule.
This is an inference from responses, not a recovered blacklist, regular expression,
or official contract. It does not establish a rule for every name or request shape.
Neither lowercase names nor underscores nor having more than five custom tools were
sufficient to cause rejection in these probes.

### Header and system-feature controls

For both failing names `mcp_weather` and `web_search_exa`, the target 400 persisted
after adding each of these, and after combining them:

- The beta set returned by sub2api's `FullClaudeCodeMimicryBetas` at the inspected revision.
- CLI-style User-Agent, `x-app`, and Stainless headers.
- Claude Code identity text.
- A three-block system with a CLI billing entrypoint, version/fingerprint suffix,
  identity, and a short generic expansion. The User-Agent and billing versions agreed.

Changing the name to an `mcp__` name made the combined-feature control succeed.
An explicit OpenCode identity sentence with the passing `weather` name also succeeded.
These controls do not execute sub2api or reproduce its complete system expansion,
cache policy, transport, and identity implementation. They show that the tested
header/system changes did not rescue these failing names, and that a full CLI
header set was not required for the passing baseline requests in this matrix.

### What sub2api contributes

Source inspected: `Wei-Shaw/sub2api` revision
`5c185c5c5d4f5e409e2139806e0964ae944e03e8` (2026-10-23), especially
[`gateway_tool_rewrite.go`](https://github.com/Wei-Shaw/sub2api/blob/5c185c5c5d4f5e409e2139806e0964ae944e03e8/backend/internal/service/gateway_tool_rewrite.go)
and
[`gateway_forward.go`](https://github.com/Wei-Shaw/sub2api/blob/5c185c5c5d4f5e409e2139806e0964ae944e03e8/backend/internal/service/gateway_forward.go).

Its OAuth mimic path dynamically renames custom tools when there are more than five,
using ordinary lowercase names in a prefix + first-three-characters + index format.
With five or fewer, only its static `sessions_` / `session_` substitutions apply;
the two Exa names are unchanged by that name mapper.

A six-tool synthetic list containing both rejected Exa names failed. Replacing only
the six names with representative names of sub2api's dynamic format succeeded;
restoring the original list failed again. This tests the naming shape, not its exact
FNV-seeded Go RNG output or the complete sub2api service. It supports dynamic renaming
as a relevant mechanism; it does not prove every other mimicry step is necessary,
nor that sub2api's unchanged small-tool path currently succeeds for these names.

### Existing experimental ingress and usage evidence

The existing experimental ingress turned failing `mcp_weather` and `web_search_exa`
requests into successes. On a synthetic tool-call exchange, the downstream received
the original `mcp_weather` name, returned its tool result using that original name,
and received a completed HTTP 200 assistant response. A separate SSE exchange
returned the original tool name in `content_block_start` and ended with `message_stop`.
No real diagnostic tool was executed; the tool result was synthetic.

Every HTTP 200 response reported `Anthropic-Ratelimit-Unified-Representative-Claim:
five_hour`. Both accounts' extra usage was disabled, reported used credits remained
zero, and successful responses reported overage rejected with `org_level_disabled`.
Fresh usage snapshots retained rounded five-hour readings of 0% and 7% respectively.
These headers support the subscription path for successful probes. Coarse utilization
and concurrent service traffic do not permit an exact marginal quota-cost estimate.

Two harness calibration outcomes are retained in the raw results: Opus 5.5 rejected
forced `tool_choice` type `tool` with a different, explicit 400; a 32-token tool-result
continuation ended at `max_tokens`. The completed exchange used `auto` and a 256-token
continuation cap. Neither calibration result is evidence for the extra-usage gate.

No production transformation was changed during this probe run. The supported next step is to retain the
existing experimental name adaptation, preserve original request semantics elsewhere,
and diagnose additional failures from a concrete failing request before broadening it.

### Follow-up: explicit custom tool types and implementation

Five additional RN probes confirmed that `type: "custom"` is accepted with `weather`
and with `mcp__web_search_exa`, while raw `web_search_exa` still receives the target
400 on both compatible and experimental ingress. The experimental normalizer treated
every explicit `type` as a server tool, accidentally skipping this supported custom
form. `type: "function"` received a separate upstream schema-validation error and
must not be inferred to be a supported native Anthropic custom-tool type from
sub2api's name-mapper condition.

The experimental normalizer now includes explicit `custom` tools; other typed tools
remain unchanged for upstream validation. Regression fixtures cover the three observed
failing names, omitted/explicit custom type, declarations and references, and response
name restoration. No blacklist or tool-count threshold was added.

Known extra-usage HTTP 400 responses are categorized as
`third_party_extra_usage_required` in request records and counted in the process and
recent-window summaries. Classification reads a bounded 8 KiB error preview and
retains only a fixed diagnostic, not the upstream body or message. Error responses
bypass tool-name restoration and are forwarded unchanged, including malformed JSON.
The console shows the category and counts. This diagnostic applies across ingresses
and does not change routing, retries, billing, or request semantics.

## 2026-10-25: generated attribution consistency and identity controls

### RN identity control matrix

Nine serial synthetic Opus 5.5 Messages requests ran through the deployed experimental
ingress on one pinned account at 18:46 Asia/Shanghai. The request body explicitly supplied
the billing block and JSON-string metadata to isolate each identity/header contrast.
The new local alignment implementation was not deployed for this matrix.

| Case | Result |
| --- | --- |
| Actual selected-account UUID and stored device ID, matching session header (four interleaved controls) | 200 |
| Only account UUID replaced with a random valid UUID | 200 |
| Only device ID replaced with another 64-hex ID | 200 |
| Only outgoing session header changed to disagree with metadata | 200 |
| CLI 2.1.295 User-Agent with billing version 2.1.280 | 200 |
| Same CLI User-Agent with billing version 2.1.295 | 200 |

All nine responses declared `five_hour`; extra usage was disabled and reported credits
remained zero. Total reported usage was 234 ordinary input and 36 output tokens. The
rounded five-hour reading remained 0%. This one-account, one-model matrix does not
prove that mismatches are ignored on every upstream path or that identity rewrites
reduce risk. It provides no present acceptance justification for overriding existing
caller identities. Missing metadata still receives the existing stable account identity.

The harness and results are under ignored `data/attribution-research-2026-10-25/`.
It read only account/device IDs through a remote read-only SQLite connection, never
exported those identities or credentials, and made no account/configuration changes.

### Implemented experimental policy

- Only newly generated billing follows a recognizable outgoing CLI release. The existing
  `claude-desktop-3p` entrypoint is retained; no CLI UA, version suffix, or CCH is invented.
  Non-CLI/unrecognized UAs retain the existing default attribution. Old recognized CLI
  releases are not silently upgraded; upstream version requirements still apply.
- If the relay generates missing Messages metadata and a Claude Code session header
  already exists, only that outgoing header is set to the generated metadata session.
  Absent headers and generic session headers are not synthesized or redefined.
- Incoming headers and routing keys remain caller-owned. Each account attempt derives
  its own identity from the original request, so failover does not hash an already
  transformed session or carry the previous account's identity into the next attempt.
- Existing metadata identities, billing text, CCH, and unknown fields are preserved.
  Count-tokens requests still receive no metadata. Compatible and official ingress and
  relay-generated account checks retain their prior attribution policy.

Local regression tests capture outgoing header/body agreement and stable repeated
requests, preserve the caller's input and opaque CCH, and exercise a two-account 503
failover. These changes remove known relay-generated contradictions; this matrix did
not establish an increase in real upstream acceptance or a reduction in risk scoring.
