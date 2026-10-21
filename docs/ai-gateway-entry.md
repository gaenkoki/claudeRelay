# AI Gateway entry trial

Status: 2026-10-22. This is an isolated experiment, not the production API entry.

- User hostname: `new.etgpao.de`, DNS-only CNAME to `saas.sin.fan`.
- SaaS custom origin: `new-o.etgpao.de`, proxied to RN. Keep this distinct from the user hostname to avoid routing loops.
- Worker: `newapi-aigateway-entry`, with only route `new.etgpao.de/__aig_probe*`.
- Gateway: existing `gate-llm`; new provider `custom-newapi-entry-test` points to `https://new-o.etgpao.de`.
- Existing provider `custom-clauderelay-test` points to the relay directly and is a separate experiment.

Deployed source: `deploy/ai-gateway-entry.mjs`, Worker version `271fb498`. It permits
only probe endpoints and streams request/response bodies without JSON parsing,
buffering, caching, or retries. The optimized hostname reaches this Worker without
changing DNS or ordinary API paths.

Verified from RN with synthetic requests:

- `/__aig_probe`: 200, `X-NewAPI-Edge-Probe: matched`.
- `/__aig_probe/api/status`: 200 through Gateway, `X-NewAPI-Edge-Probe: gateway` and a Gateway log ID.
- `/api/status`: 200 without the probe header, confirming ordinary paths stay unchanged.
- Probe Messages with an invalid key: new-api returns 401.
- Authenticated probe Messages pinned to compatible channel 150004: SSE 200 with `message_start` and `message_stop`, no error event.
- Authenticated probe token counting: 200 with `input_tokens: 53`.

Channels 150004 and 180007 still use `custom-clauderelay-test`; they were not changed
by this trial. Consequently the model smoke test traversed Gateway both before
new-api and between new-api and relay. Before adopting the entry design, restore
those channels to their internal relay URL in the same reviewed rollout. The
entry trial alone does not eliminate the existing server round trip.

CPU overview initially reported 421 microseconds across the small mixed-version
sample; the percentile chart had not populated useful values. This is not a
production CPU bound. Long streams, large requests, and peak load remain untested
on the entry Worker. Do not expand the probe route to production API paths without
reviewing the result.
