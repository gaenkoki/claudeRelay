# Claude Code identification research

Observed on 2026-10-17. The initial exploration did not change admission or cooldown policies.
The subsequently authorized consistency checks are described below.

## Objective and boundary

Improve identification of fabricated Claude Code requests on the official ingress, with third-party
API-key traffic as the primary target. OAuth captures alone do not establish this contract.

The current deployment receives requests through downstream gateways, including new-api. HTTP
fields are caller-controlled; the relay does not receive a cryptographic attestation of the
original executable. A client can copy a real request or use the official executable to generate
its preferred request shape. No request-shape heuristic should be described as proof of origin.

## Method

- Reused Desktop's bundled native Claude Code executables, versions 2.1.295 and 2.1.286. No CLI
  installation or global configuration changes were needed. macOS verified the 2.1.295 app's
  code signature as Anthropic PBC, team Q6L2SF6YDW. This verifies the tested local executable,
  not the origin of unrelated requests received by our server.
- Started a separate PrismCat instance on loopback port 18711 with separate configuration and
  storage. It forwarded only to a synthetic loopback Messages server on port 18712. The existing
  PrismCat instance on port 8711 and its routes were left unchanged.
- Used dedicated `CLAUDE_CONFIG_DIR` directories, dummy credentials, disabled nonessential
  traffic/updates, an empty MCP configuration, and synthetic prompts. The one tool exercise
  permitted only Read against a synthetic fixture. No real account credentials or paid provider
  calls were needed.
- Captured 21 Messages requests: print mode, resumed print conversation, custom system prompt,
  disabled tools, bare mode, a Read/tool-result exchange, varied English/Chinese prompts,
  third-party bearer-token auth, the older executable, and interactive CLI main/helper requests.
  Interactive tests used a pseudoterminal; a two-turn exchange completed against the mock.
- PrismCat persisted all 21 Messages requests with HTTP 200, no request/header overrides, and no
  truncation. Two separate `HEAD /api/hello` probes received the mock's unimplemented-method 501;
  interactive Messages still proceeded. Mock usage values are fabricated and are **not token
  measurements or evidence of upstream model acceptance**.

Local artifacts and the synthetic harness are under ignored `data/cc-identification-2026-10-17/`:
`captures.json`, `prismcat.db`, `summary.json`, `probe.py`, `extra_probes.py`, and
`interactive_probe.py`. Authentication headers are redacted. Artifacts still contain local paths
and synthetic conversation/environment text and should not be published as production fixtures.
The overlay Go probe has a `cc_identification_research` build tag so ordinary `go test ./...`
does not compile it as a separate package beneath `data/`. Replay it with:

```sh
go test -tags cc_identification_research -overlay data/cc-identification-2026-10-17/overlay.json ./internal/proxy -run '^TestCCIdentificationResearch$' -v
```

## Observations

Approximate system-text characters below exclude tools and the separate environment messages.
They are not token counts.

| Official invocation | Entrypoint | System characters | Tools |
| --- | --- | ---: | ---: |
| Interactive main, 2.1.295 | cli | 6,100 | 23 |
| Interactive Haiku helper, 2.1.295 | cli | 3,190 | 0 |
| Default print mode, 2.1.295 | sdk-cli | 5,850 | 20 |
| Custom system, print mode | sdk-cli | 166 | 20 |
| Default system, tools disabled | sdk-cli | 5,680 | 0 |
| Bare print mode | sdk-cli | 233 | 3 |
| Bare, custom system, tools disabled | sdk-cli | 166 | 0 |
| Custom system, tools disabled, 2.1.286 | sdk-cli | 166 | 0 |

Consequences:

- A short system prompt, no tools, or a small context cannot by itself prove a fabricated client.
  The actual signed CLI supports these variants. Accepting every official CLI invocation and
  requiring a full default coding prompt/tool set are different product policies.
- These captures consistently had matching `X-Claude-Code-Session-Id` and
  `metadata.user_id.session_id`, 64-character hex device IDs, and empty metadata account UUIDs.
  Empty account UUIDs are valid in this API-key setup. The resumed print conversation retained
  both its device and session identity.
- UA and billing attribution consistently agreed on version and entrypoint. Interactive requests
  identified as `cli`; print mode identified as `sdk-cli`. Both API-key and third-party bearer-token
  tests worked. The latter is not evidence about subscription OAuth authentication.
- All 21 API-route requests omitted `cch`. Native source inspection conditions that field and
  previous-request/prompt attribution on provider/auth mode. Requiring them based on OAuth
  captures would reject the API traffic tested here.
- Recent clients use `role: system` inside the Messages history for environment and per-turn
  context, as well as context-management, thinking, safeguards, and output configuration fields.
  Safeguards appeared on an initial tool request and disappeared on its continuation. A fixed
  field checklist is not stable even within one normal interaction.

## A reproducible version suffix, not authentication

Native source inspection located the calculation for the three hex digits appended to
`cc_version`. It is a truncated SHA-256 checksum derived from a few characters of the first
non-meta user text, a client-embedded constant, and the CLI release version.

The calculation reproduced **21/21 captured suffixes**, across the two releases, interactive
helpers, Chinese and short inputs, resume, and the tool continuation. Wire user content can contain
injected reminder blocks before the actual prompt, so simply hashing the first wire text block
does not reproduce the native calculation. The source's notion of a non-meta user prompt matters.

This checksum is a useful consistency candidate for careless synthetic traffic, but it has only
12 bits and no client-held secret unavailable to callers. It is not a MAC, a signature, a complete
body integrity check, or proof of the official executable. Do not silently replace the existing
opaque-attribution policy with enforcement based on this two-version experiment.

## Current classifier experiment

An isolated Go overlay test invoked the unmodified `classifyClient` function against all captured
Messages requests. It classified all 21 as `cc_candidate`.

A benign synthetic request carrying only billing-style system attribution, arbitrary nonempty
device/session strings, an incorrect version suffix, and a metadata session different from the
header also classified as `cc_candidate`. The current classifier does not check these relationships.
The overlay and test source are local research artifacts; no production source file was added.

## Candidate checks and recommended scope

| Candidate | Benefit | Limit |
| --- | --- | --- |
| Header/metadata session agreement | Detects inconsistent stitching | Gateways may rewrite either side; both can be forged together |
| UA/billing version and entrypoint agreement | Detects mixed client templates | Legitimate gateway transforms must be characterized; easy to copy |
| Native version-suffix consistency | Detects some arbitrary billing text | Short public checksum; prompt selection and versions need validation |
| Version-aware default prompt/tool profile | Raises the cost of minimal imitation | Excludes some genuine CLI modes; can still be copied |
| Device and message-component continuity | Helps explain session churn after refusals | Resettable identity, changed text, and shared templates remain ambiguous |
| Client TLS fingerprint at claudeRelay | Little value in this deployment | TLS peers are gateways/reverse proxies, not necessarily the CLI |

First use additional checks in observation mode, retaining only reasons, counts, and keyed
component fingerprints. Keep ordinary conversations and explicitly identified auxiliary requests
separate. Do not automatically promote token size or device/session churn to proof of abuse.

Before any hardening, decide whether official ingress promises compatibility with all supported
CLI customization/headless modes or only the default interactive/Desktop coding profile. A
default-profile restriction can be enforced as an access policy, but should not be called authentic
client verification. Repeated refusal behavior still needs independent controls.

## Remaining validation

- New CLI releases, other operating systems, Desktop's own API-key surface, forks/background
  sessions, compaction, Agent SDK customizations, and auxiliary one-token/count-token requests.
- End-to-end preservation through the deployed new-api/channel settings. Local mock captures
  do not establish which fields downstream site gateways preserve.
- Real provider acceptance, feature-flag variations, and false-positive rates. None were measured.
- Which components differed in the earlier production burst. Historical whole-chain hashes
  cannot recover the missing request bodies, device values, or individual component changes.

## SDK consistency experiment — 2026-10-20

Goal: find version-specific contradictions that can reject imitation while admitting genuine
Claude Code modes. Cross-session refusal-content cooldown is deferred at the user's request;
context-free matching can also affect legitimate retries after a false refusal.

Thirteen additional Messages requests were captured against an isolated localhost mock, with
dummy credentials and separate `CLAUDE_CONFIG_DIR` directories. Both existing native apps passed
`codesign --verify --strict`. The older npm tarball came from the official
`@anthropic-ai/claude-code` registry package and its SHA-512 matched the registry integrity value;
only its JS entrypoint and package metadata were extracted, without running installation scripts.

| CLI version / installation | Default SDK package header | Default runtime header |
| --- | --- | --- |
| 2.1.100 / npm JS, local Node | 0.81.0 | node / v24.21.0 |
| 2.1.286 / Desktop-bundled native | 0.127.0 | node / v26.3.0 |
| 2.1.295 / Desktop-bundled native | 0.128.0 | node / v26.3.0 |

Default and custom-system/no-tools requests retained these values. However, **all three official
versions also emitted `x-stainless-lang: python`, `x-stainless-runtime: CPython`, and SDK package
`9.9.9` when configured through `ANTHROPIC_CUSTOM_HEADERS`**, while retaining their actual CC
User-Agent. The two native versions additionally emitted an overridden `bun` runtime and empty
SDK fields in separate cases. Every invocation exited successfully with exactly one mock Messages
request; this establishes client behavior, not real Anthropic acceptance or production gateway
preservation. These examples deliberately use unusual settings to disprove an invariant, not to
estimate how often ordinary users configure them.

The official [environment variable reference](https://code.claude.com/docs/en/env-vars) documents
custom request headers; the actual override behavior above was independently captured. A minimum
CLI version narrows the version matrix but cannot make mutable headers authoritative. In
particular, a Python SDK header is not proof that the request bypassed CC. Enforcing default SDK
headers would be a new configuration restriction requiring an explicit policy choice. No SDK
rejection rule or production setting was added by this experiment.

The inspected local New API implementation constructs a new HTTP request and does not copy
`x-stainless-*` in its common/Claude header setup. Explicit channel header overrides can forward
them. This is source evidence only; reseller gateways and the deployed configuration were not
inspected in this experiment. Missing or rewritten SDK fields cannot identify the original client.

Reproduction files remain in ignored `data/cc-sdk-research-2026-10-20/`: `run_matrix.py` tests the
two native apps, `run_npm.py` adds the verified older npm entrypoint, and `analyze.py` checks all
13 captures and redaction. Raw captures, isolated configs, and downloaded third-party code stay
there rather than in Git. The local mock was stopped; no global CLI was installed or replaced.
Local 2.1.299 contains only a partial Desktop download and was not executed. More versions are
not needed to disprove SDK-header immutability; OS/auth/gateway coverage is still required before
promoting any narrower candidate rule to enforcement.

## Consistency implementation — 2026-10-17

Classifier v6 adds official-ingress rejection for contradictory session IDs, UA/billing release
versions, and comparable UA/billing entrypoints. It retains the existing request-shape rules and
does not require default coding prompts or tools. The original metadata parser now serves both
cooldown fingerprinting and consistency checks, including legacy session strings. Compatible
ingresses only observe these comparisons.

The version suffix remains observational, limited to releases 2.1.286 and 2.1.295; unknown releases
are explicitly marked unsupported for this diagnostic. CCH presence is recorded without its value
and is never rejected by itself. Both choices preserve the user's requirement to support genuine
CC across modes, rather than enforce a default-profile restriction. Raw identifiers and prompts
are not added to logs. See the README for the current admission contract.

References: [CLI reference](https://code.claude.com/docs/en/cli-reference),
[authentication](https://code.claude.com/docs/en/authentication),
[setup](https://code.claude.com/docs/en/setup). The local CLI help and captures establish the
specific tested options and behavior; documentation alone is not treated as capture evidence.
