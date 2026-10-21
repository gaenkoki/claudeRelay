# AI Gateway entry trial

Status: 2026-10-22. This is an isolated experiment, not the production API entry.

- User hostname: `new.etgpao.de`, DNS-only CNAME to `saas.sin.fan`.
- SaaS custom origin: `new-o.etgpao.de`, proxied to RN. Keep this distinct from the user hostname to avoid routing loops.
- Worker: `newapi-aigateway-entry`, with only route `new.etgpao.de/__aig_probe*`.
- Gateway: existing `gate-llm`; new provider `custom-newapi-entry-test` points to `https://new-o.etgpao.de`.
- Existing provider `custom-clauderelay-test` points to the relay directly and is a separate experiment.

The deployed Worker is currently the dashboard's Hello World template. Both its
workers.dev address and the optimized hostname's `/__aig_probe` return `Hello World!`.
This verifies Worker route matching without changing DNS or ordinary API paths.

Prepared source: `deploy/ai-gateway-entry.mjs`. It has passed syntax and focused
forwarding checks locally but has **not been uploaded**: the Cloudflare dashboard's
embedded code editor remains loading. It permits only probe endpoints and streams
request/response bodies without JSON parsing, buffering, caching, or retries.

Next: publish the prepared source and verify `/__aig_probe/api/status`, then use
synthetic authenticated Messages requests. Before adopting the entry design,
restore any trial new-api channels to their internal relay URL; otherwise requests
would traverse Gateway both before new-api and between new-api and the relay.
Do not expand the probe route to production API paths without reviewing the result.
