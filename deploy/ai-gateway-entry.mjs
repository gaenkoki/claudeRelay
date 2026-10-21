// Isolated entry-path trial. Bind only new.etgpao.de/__aig_probe*.
// The Gateway provider must point to new-o.etgpao.de, never the routed hostname.
const prefix = "/__aig_probe";
const gateway = "https://gateway.ai.cloudflare.com/v1/cfd6012cd19882aa5329fa72d146260c/gate-llm/custom-newapi-entry-test";

export default {
  async fetch(request) {
    const incoming = new URL(request.url);
    if (request.method === "GET" && (incoming.pathname === prefix || incoming.pathname === `${prefix}/`)) {
      return Response.json({ probe: "newapi-aigateway-entry", hostname: incoming.hostname }, {
        headers: { "Cache-Control": "no-store", "X-NewAPI-Edge-Probe": "matched" },
      });
    }

    const path = incoming.pathname.slice(prefix.length);
    const statusProbe = request.method === "GET" && path === "/api/status";
    const modelProbe = request.method === "POST" && ["/v1/messages", "/v1/messages/count_tokens"].includes(path);
    if (!incoming.pathname.startsWith(`${prefix}/`) || (!statusProbe && !modelProbe)) {
      return new Response("Unknown probe endpoint", { status: 404 });
    }

    const target = new URL(gateway + path);
    target.search = incoming.search;
    const forwarded = new Request(target, request);
    forwarded.headers.delete("Host");
    forwarded.headers.delete("Cookie");
    for (const name of [...forwarded.headers.keys()]) {
      if (name.toLowerCase().startsWith("cf-aig-")) forwarded.headers.delete(name);
    }
    forwarded.headers.set("cf-aig-skip-cache", "true");
    forwarded.headers.set("cf-aig-collect-log", "true");
    forwarded.headers.set("cf-aig-collect-log-payload", "true");

    // Do not retry: a transport failure can occur after upstream generation starts.
    const upstream = await fetch(forwarded, { redirect: "manual" });
    const response = new Response(upstream.body, upstream);
    response.headers.set("X-NewAPI-Edge-Probe", "gateway");
    return response;
  },
};
