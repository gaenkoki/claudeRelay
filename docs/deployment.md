# Server deployment

The supported production shape is one Claude Relay container, one private SQLite volume, and an
HTTPS reverse proxy. Do not scale the relay to multiple replicas: account ownership, refresh-token
rotation, sticky routing, and SQLite are intentionally single-instance boundaries.

## Start with Docker Compose

Copy the environment template and generate five different API keys:

```powershell
Copy-Item .env.example .env
$bytes = New-Object byte[] 32
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
-join ($bytes | ForEach-Object { $_.ToString("x2") })
```

Run the generation command five times and put different values in `CLAUDE_RELAY_API_KEY`,
`CLAUDE_RELAY_EXPERIMENTAL_API_KEY`, `CLAUDE_RELAY_OFFICIAL_API_KEY`, `CLAUDE_RELAY_ADMIN_API_KEY`, and
`CLAUDE_RELAY_AVAILABILITY_API_KEY` in `.env`, then start the service:

```powershell
docker compose up -d --build
docker compose ps
```

## Deploy the private GHCR image

GitHub Actions tests every push and pull request and builds Linux images for AMD64 and ARM64. A
successful push to `main` publishes `ghcr.io/paopaoandlingyia/clauderelay:latest`; a `v*` Git tag
also publishes semantic-version tags. Action dependencies are pinned to full commit hashes.

Because the repository and package are private, authenticate the server to GHCR with a GitHub token
that can read packages:

```powershell
$env:GHCR_TOKEN | docker login ghcr.io -u paopaoandlingyia --password-stdin
```

Add this line to the server's `.env`:

```text
CLAUDE_RELAY_IMAGE=ghcr.io/paopaoandlingyia/clauderelay:latest
```

Then deploy or update without compiling on the server:

```powershell
docker compose pull
docker compose up -d --no-build
```

The token is needed only by Docker to pull the private image; do not put it in `.env` or
`compose.yaml`. If repository policy prevents `GITHUB_TOKEN` from publishing packages, allow this
workflow read/write package permission in the repository Actions settings.

The default port mapping is `127.0.0.1:8567:8567`, so the API and WebUI are not directly exposed
to the network. Open `http://127.0.0.1:8567/` on the server or publish it through an HTTPS reverse
proxy. A minimal Caddy site is:

```caddyfile
relay.example.com {
    reverse_proxy 127.0.0.1:8567
}
```

The compatible, experimental, and official relay keys can call Anthropic endpoints and choose explicit aliases
among the accounts their ingress may reach, but cannot read or change account administration. The
official key also rejects requests that do not match the observed Claude Code shape. The separate
administration key controls the WebUI, OAuth, account placement, and activation, but cannot call
model endpoints.
Treat the administration key as privileged access. Do not publish port 8567 to the internet without
TLS and an appropriate network boundary.

## Configuration

The image contains only non-secret defaults. Compose passes these supported runtime settings:

| Environment variable | Purpose | Compose default |
| --- | --- | --- |
| `CLAUDE_RELAY_API_KEY` | Relay key for messages, token counting, and account override | Required |
| `CLAUDE_RELAY_EXPERIMENTAL_API_KEY` | Optional compatible ingress with experimental request transforms | Empty |
| `CLAUDE_RELAY_OFFICIAL_API_KEY` | Optional Claude Code-shaped ingress with access to every account | Empty |
| `CLAUDE_RELAY_ADMIN_API_KEY` | Administration key for WebUI, OAuth, and account state | Required and must differ |
| `CLAUDE_RELAY_AVAILABILITY_API_KEY` | Read-only key for `GET /ops/v1/availability` | Empty (endpoint disabled) |
| `CLAUDE_RELAY_UPSTREAM_PROXY` | HTTP(S) proxy for accounts set to follow global configuration | Empty |
| `CLAUDE_RELAY_MAX_REQUEST_BYTES` | Maximum request body size | `33554432` |
| `CLAUDE_RELAY_MAX_INFLIGHT_PER_ACCOUNT` | Hard per-account Messages limit before a WebUI override | `3` |
| `CLAUDE_RELAY_MAX_ACTIVE_SESSIONS_PER_ACCOUNT` | New-session admission limit per account | `5` |
| `CLAUDE_RELAY_AUTO_REFRESH_ENABLED` | Emergency global OAuth refresh switch | `true` |
| `CLAUDE_RELAY_BIND_ADDRESS` | Host-side published address | `127.0.0.1` |

The WebUI's `接入 → 账号并发上限` displays and changes the effective per-account Messages limit.
A saved positive value overrides the config/environment startup limit and persists across restarts.
Lowering it leaves existing requests running; new requests are admitted only when a slot is free.
The application default is 6; the Compose template retains its separate startup default of 3.

The Official Claude Code version lower and upper bounds are configured from the WebUI under
`接入 → Official 版本限制`. They are persisted in the relay database and take effect immediately;
leaving either field empty disables that bound. The check uses the downstream
`User-Agent: claude-cli/x.y.z` version and applies only to the official ingress.

`CLAUDE_RELAY_BIND_ADDRESS` affects Docker's host port only. The process listens on
`0.0.0.0:8567` inside its isolated container. On Docker Desktop, a host-side Clash proxy can
usually be addressed as `http://host.docker.internal:7890`. On a Linux server, use a reachable
proxy address rather than assuming that hostname exists.

Per-account exits are managed in the WebUI through **账号 → 出口设置**; routine changes
do not require editing `.env` or restarting the container. Choose direct, custom HTTP(S)
proxy, or follow global configuration. New accounts default to direct; existing accounts
retain global inheritance after the schema v15 migration. OAuth setup also supports choosing
an exit before token exchange. HTTP proxies tunnel HTTPS destinations using CONNECT.
SOCKS5 URLs are not supported. Settings and proxy credentials are stored in the private
SQLite volume, so protect backups as carefully as OAuth tokens. Upgrade automatically adds
the account exit columns; older builds cannot open the upgraded schema.

The container runs as UID/GID `10001`, drops Linux capabilities, uses a read-only root filesystem,
and writes only to `/data` and a small temporary filesystem. The health check calls `/healthz`
inside the container and does not require the API key.

## Logs and request tracing

Follow the service log with:

```powershell
docker compose logs -f --tail=100 claude-relay
```

Compose uses Docker's `json-file` driver with three 10 MiB files, limiting retained container logs
to approximately 30 MiB. Logs contain paths, ingress names, selected account aliases, routing
sources, upstream status, duration, and errors. They do not contain prompts, request bodies, API/OAuth tokens,
metadata identities, email addresses, or usage token counts.

Authentication diagnostics additionally record OAuth authorization, account activation,
and manual/automatic refresh attempts, including token expiry and the last successful refresh
timestamp (Unix seconds). Refresh success is logged only after both rotated tokens are saved.
OAuth endpoint failures include HTTP status and allowlisted error codes/reasons; model endpoint
`401`/`403` responses generate `upstream authentication rejected` with the account and request ID.
Reasons distinguish `revoked`, `expired`, `token_reused`, `invalid_refresh_token`, and
`invalid_credentials`. Unknown errors remain `unknown`; malformed or incomplete JSON is marked
`non_json_or_incomplete`. Free-form upstream error text and raw bodies are never logged. Model
error previews are capped at 8 KiB and report `body_truncated` without changing the forwarded response.
The console's recent request records also include these safe authentication categories.

For an authentication incident, filter retained logs without rotating any credentials:

```powershell
docker compose logs --since 24h claude-relay | Select-String 'OAuth|upstream authentication rejected|account activation changed'
```

Every response carries `X-Claude-Relay-Request-ID`, and the same value appears as `request_id` in
request-related logs. Callers may supply a value of at most 64 characters using letters, digits,
dots, colons, underscores, and hyphens; invalid or missing values are replaced with a random ID.
Relay request IDs are deliberately removed before forwarding upstream.

The console shows the same information without shell access, reading the last
`CLAUDE_RELAY_REQUEST_LOG_SIZE` requests from an in-memory ring (500 by default, `0` disables it).
That ring holds the same metadata fields as the container log, is never written to the volume, and
is cleared whenever the container restarts. It is a live operations view, not an audit trail —
container logs remain the retained record.

## Connect through New API

Attach Claude Relay to the same Docker network as New API and use the relay container name and
port (for example `http://claude-relay:8567`) as the channel base URL. `localhost:8567` inside the
New API container refers to New API itself, not this relay.

Create three Anthropic channels:

1. A stable compatible channel using `CLAUDE_RELAY_API_KEY`, assigned only to the compatible token group.
2. An experimental compatible channel using `CLAUDE_RELAY_EXPERIMENTAL_API_KEY`, assigned only to
   the experimental token group.
3. An official channel using `CLAUDE_RELAY_OFFICIAL_API_KEY`, assigned only to the official token
   group.

Enable request-body pass-through on all three. The official channel must also copy the original client
headers explicitly:

```json
{
  "User-Agent": "{client_header:User-Agent}",
  "X-Claude-Code-Session-Id": "{client_header:X-Claude-Code-Session-Id}",
  "X-App": "{client_header:X-App}"
}
```

Enable at least one relay account in the WebUI. Imported accounts start shared, so the official
ingress can use them immediately and no placement step is required. Mark an account `official` only
to keep compatible traffic off it; if every account is marked that way, the compatible ingress has
nothing to select and returns an explicit unavailable error.

## Move the existing database

Fully stop the native `claude-relay.exe` before copying SQLite. From the repository directory,
create the named volume and copy the stopped database into it:

```powershell
docker volume create claude-relay-data
docker run --rm `
  -v claude-relay-data:/target `
  -v "${PWD}/data:/source:ro" `
  alpine:3.23 `
  sh -c "cp /source/claude-relay.db* /target/ && chown 10001:10001 /target/claude-relay.db*"
```

Then run `docker compose up -d --build`. Imported account state is preserved exactly; disabled
accounts remain disabled. Never run the native executable and container against the same database
or refresh-token chain at the same time.

## Backup and upgrade

OAuth refresh tokens live in the SQLite volume, so backups must be private. Stop the container to
obtain a simple consistent archive:

```powershell
docker compose stop
docker run --rm `
  -v claude-relay-data:/data:ro `
  -v "${PWD}:/backup" `
  alpine:3.23 `
  tar czf /backup/claude-relay-data.tar.gz -C /data .
docker compose start
```

To upgrade the locally built image without deleting the named volume:

```powershell
git pull --ff-only
docker compose up -d --build
```

`docker compose down` retains the named volume. Do not use `docker compose down -v` unless the
intent is to delete all imported credentials and routing state.
