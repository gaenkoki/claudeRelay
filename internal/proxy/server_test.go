package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/store"
)

func TestAuthenticationDiagnosticsPreserveUpstreamResponse(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for _, body := range []string{
		`{"error":{"type":"authentication_error","message":"OAuth access token has been revoked. secret-access user@example.com"}}`,
		strings.Repeat("x", 20<<10),
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, body)
		}))
		server := newTestServer(t, upstream.URL, 4096)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
		request.Header.Set("x-api-key", "downstream-key")
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, request)
		upstream.Close()
		if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != body {
			t.Fatalf("upstream authentication response changed: status=%d bytes=%d", recorder.Code, recorder.Body.Len())
		}
		if !strings.Contains(logs.String(), "upstream authentication rejected") ||
			strings.Contains(logs.String(), "secret-access") || strings.Contains(logs.String(), "user@example.com") {
			t.Fatalf("missing diagnostics or sensitive response leaked: %s", logs.String())
		}
		if len(body) < 8<<10 && !strings.Contains(logs.String(), "reason=revoked") {
			t.Fatalf("missing revocation reason: %s", logs.String())
		}
		if len(body) > 8<<10 && !strings.Contains(logs.String(), "body_truncated=true") {
			t.Fatalf("missing truncation diagnostic: %s", logs.String())
		}
		logs.Reset()
	}
}

func TestForwardDecodesCompressedSSEBeforeUsageObservation(t *testing.T) {
	t.Parallel()
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("upstream Accept-Encoding = %q, want transport-managed gzip", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		_, _ = io.WriteString(compressed, stream)
		_ = compressed.Close()
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("Accept-Encoding", "br, gzip")
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("downstream Content-Encoding = %q after transport decoding", got)
	}
	if got := recorder.Body.String(); got != stream {
		t.Fatalf("decoded response body = %q", got)
	}
	if err := server.accounting.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account found=%v err=%v", found, err)
	}
	totals, err := server.store.UsageTotalsByModel(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	usage := totals["claude-test"]
	if usage.Requests != 1 || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("observed compressed usage = %#v", usage)
	}
}

func TestForwardRecordsSuccessfulRefusalInRequestsAndAccountSummary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","model":"claude-test","stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"not persisted"},"usage":{"input_tokens":8,"output_tokens":0}}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || !records[0].Refusal || records[0].RefusalCategory != "cyber" {
		t.Fatalf("records=%+v", records)
	}
	if summary := server.metrics.Summary(time.Now()); summary.Failures != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	if err := server.accounting.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	accounts, err := server.store.AllAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	views, err := server.accountViews(httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil), accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Refusal == nil || views[0].Refusal.Count24h != 1 || views[0].Refusal.LastCategory != "cyber" {
		t.Fatalf("views=%+v", views)
	}
}

func TestForwardPreservesBodyAndReplacesAuthentication(t *testing.T) {
	t.Parallel()
	requestBody := "{\n  \"model\": \"claude-test\",\n  \"system\": [{\"type\":\"text\",\"text\":\"x-anthropic-billing-header: cc_version=2.1.219.0a7; cc_entrypoint=claude-desktop; cch=abcde;\"}],\n  \"metadata\": {\"user_id\":\"official-client-value\"},\n  \"messages\": []\n}"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("beta") != "true" || r.URL.Query().Get("capture") != "1" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Errorf("x-api-key leaked upstream: %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "claude-cli/2.1.215" {
			t.Errorf("User-Agent = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != requestBody {
			t.Errorf("body changed:\n%s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?capture=1", strings.NewReader(requestBody))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("User-Agent", "claude-cli/2.1.215")
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	server.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Body.String(); got != "event: message_start\ndata: {}\n\n" {
		t.Fatalf("response body = %q", got)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 {
		t.Fatalf("request records = %d, want 1", len(records))
	}
	if records[0].ClientClass != clientClassAmbiguous || records[0].RelayAction != "unchanged" {
		t.Errorf("client observation = class %q action %q", records[0].ClientClass, records[0].RelayAction)
	}
	if records[0].ClientEvidence == nil || records[0].ClientEvidence.StructuredMetadata {
		t.Errorf("client evidence = %#v", records[0].ClientEvidence)
	}
}

func TestForwardNormalizesToolNamesOnlyForExperimentalIngress(t *testing.T) {
	t.Parallel()
	var upstreamBodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		upstreamBodies = append(upstreamBodies, decodeBody(t, body))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)

	compatibleBody := `{"model":"claude-test","system":[{"type":"text","text":"` + defaultBillingAttribution + `"}],` +
		`"metadata":{"user_id":"caller-owned"},"tools":[` +
		`{"name":"get_weather","input_schema":{"type":"object"}},` +
		`{"type":"web_search_20250305","name":"web_search"}],` +
		`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}]}]}`
	compatibleRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(compatibleBody))
	compatibleRequest.Header.Set("x-api-key", "downstream-key")
	compatibleRecorder := httptest.NewRecorder()
	server.routes().ServeHTTP(compatibleRecorder, compatibleRequest)
	if compatibleRecorder.Code != http.StatusOK {
		t.Fatalf("compatible status = %d, body = %s", compatibleRecorder.Code, compatibleRecorder.Body.String())
	}
	experimentalRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(compatibleBody))
	experimentalRequest.Header.Set("x-api-key", "experimental-downstream-key")
	experimentalRecorder := httptest.NewRecorder()
	server.routes().ServeHTTP(experimentalRecorder, experimentalRequest)
	if experimentalRecorder.Code != http.StatusOK {
		t.Fatalf("experimental status = %d, body = %s", experimentalRecorder.Code, experimentalRecorder.Body.String())
	}

	officialBody := `{"model":"claude-test","system":[{"type":"text","text":"` + defaultBillingAttribution + `"}],` +
		`"metadata":{"user_id":` + strconv.Quote(testClaudeCodeMetadata) + `},` +
		`"tools":[{"name":"web_search","input_schema":{"type":"object"}}],"messages":[]}`
	officialRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(officialBody))
	officialRequest.Header.Set("x-api-key", "official-downstream-key")
	setOfficialClaudeCodeTestHeaders(officialRequest)
	officialRecorder := httptest.NewRecorder()
	server.routes().ServeHTTP(officialRecorder, officialRequest)
	if officialRecorder.Code != http.StatusOK {
		t.Fatalf("official status = %d, body = %s", officialRecorder.Code, officialRecorder.Body.String())
	}

	if len(upstreamBodies) != 3 {
		t.Fatalf("upstream requests = %d, want 3", len(upstreamBodies))
	}
	compatibleTools := upstreamBodies[0]["tools"].([]any)
	if got := compatibleTools[0].(map[string]any)["name"]; got != "get_weather" {
		t.Fatalf("compatible tool name = %q", got)
	}
	if got := compatibleTools[1].(map[string]any)["name"]; got != "web_search" {
		t.Fatalf("compatible built-in tool name = %q, want unchanged", got)
	}
	compatibleMessages := upstreamBodies[0]["messages"].([]any)
	compatibleToolUse := compatibleMessages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if got := compatibleToolUse["name"]; got != "get_weather" {
		t.Fatalf("compatible tool use name = %q", got)
	}
	experimentalTools := upstreamBodies[1]["tools"].([]any)
	if got := experimentalTools[0].(map[string]any)["name"]; got != "mcp__get_weather" {
		t.Fatalf("experimental tool name = %q", got)
	}
	if got := experimentalTools[1].(map[string]any)["name"]; got != "web_search" {
		t.Fatalf("experimental built-in tool name = %q, want unchanged", got)
	}
	experimentalMessages := upstreamBodies[1]["messages"].([]any)
	experimentalToolUse := experimentalMessages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if got := experimentalToolUse["name"]; got != "mcp__get_weather" {
		t.Fatalf("experimental tool use name = %q", got)
	}
	officialTools := upstreamBodies[2]["tools"].([]any)
	if got := officialTools[0].(map[string]any)["name"]; got != "web_search" {
		t.Fatalf("official tool name = %q, want unchanged", got)
	}
}

func TestExperimentalIngressRestoresNonStreamingResponseToolNames(t *testing.T) {
	t.Parallel()
	responseBody := `{"id":"msg_1","model":"claude-test","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__get_weather","input":{}}],"usage":{"input_tokens":3,"output_tokens":2}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		root := decodeBody(t, body)
		tool := root["tools"].([]any)[0].(map[string]any)
		if got := tool["name"]; got != "mcp__get_weather" {
			t.Fatalf("upstream tool name = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)

	requestBody := `{"model":"claude-test","system":[{"type":"text","text":"` + defaultBillingAttribution + `"}],` +
		`"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
	request.Header.Set("x-api-key", "experimental-downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("stale Content-Length = %q", got)
	}
	root := decodeBody(t, recorder.Body.Bytes())
	toolUse := root["content"].([]any)[0].(map[string]any)
	if got := toolUse["name"]; got != "get_weather" {
		t.Fatalf("downstream tool name = %q", got)
	}
	if err := server.accounting.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account found=%v err=%v", found, err)
	}
	totals, err := server.store.UsageTotalsByModel(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage := totals["claude-test"]; usage.InputTokens != 3 || usage.OutputTokens != 2 {
		t.Fatalf("usage after response adaptation = %#v", usage)
	}
}

func TestExperimentalIngressRestoresStreamingResponseToolNames(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_start\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"mcp__get_weather\",\"input\":{}}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-test","tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"messages":[]}`))
	request.Header.Set("x-api-key", "experimental-downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"name":"get_weather"`) || strings.Contains(recorder.Body.String(), `"name":"mcp__get_weather"`) {
		t.Fatalf("streaming tool name was not restored: %s", recorder.Body.String())
	}
}

func TestOfficialIngressRejectsNonClaudeCodeShape(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "official-downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("official gate allowed %d upstream calls", upstreamCalls)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Ingress != ingressOfficial ||
		records[0].ClientClass != clientClassCompatible || records[0].Account != "" {
		t.Fatalf("rejected request record = %#v", records)
	}
}

func TestA6APIProbeSwitchAppliesImmediatelyAndPersists(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	if server.allowA6APIProbes.Load() {
		t.Fatal("probe exception did not default to disabled")
	}
	for _, enabled := range []bool{true, false} {
		response := adminRequest(t, server, http.MethodPost, "/admin/v1/official/a6api-probes",
			`{"enabled":`+strconv.FormatBool(enabled)+`}`)
		if response.Code != http.StatusOK {
			t.Fatalf("toggle status = %d: %s", response.Code, response.Body.String())
		}
		overview := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
		if overview.Code != http.StatusOK || !strings.Contains(overview.Body.String(), `"allow_a6api_probes":`+strconv.FormatBool(enabled)) {
			t.Fatal("overview did not expose saved switch state")
		}
		// Construct another server over the same store to exercise restart loading.
		restarted, err := NewServer(server.cfg, server.store)
		if err != nil {
			t.Fatal(err)
		}
		for _, instance := range []*Server{server, restarted} {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("x-api-key", "official-downstream-key")
			request.Header.Set("X-A6API-Probe-Id", "probe-1")
			recorder := httptest.NewRecorder()
			instance.routes().ServeHTTP(recorder, request)
			want := http.StatusForbidden
			if enabled {
				want = http.StatusOK
			}
			if recorder.Code != want {
				t.Fatalf("enabled=%v: probe status=%d, want %d", enabled, recorder.Code, want)
			}
		}
	}
}

func TestA6APIProbeSwitchRejectsInvalidAndUnauthorizedChanges(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "https://upstream.invalid", 4096)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"true"}`, `{"enabled":true,"unknown":1}`} {
		recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/official/a6api-probes", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid toggle %s: status = %d", body, recorder.Code)
		}
	}
	for _, key := range []string{"", "wrong-key", "official-downstream-key", "downstream-key"} {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/official/a6api-probes", strings.NewReader(`{"enabled":true}`))
		request.Header.Set("x-api-key", key)
		recorder := httptest.NewRecorder()
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("non-admin toggle status = %d", recorder.Code)
		}
	}
	if server.allowA6APIProbes.Load() {
		t.Fatal("rejected request changed switch state")
	}
	// Failed persistence must not enable admission, and corrupt stored settings
	// must fail startup rather than silently restore a permissive policy.
	if err := server.store.SetRuntimeSetting(t.Context(), a6APIProbeSetting, "invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(server.cfg, server.store); err == nil {
		t.Fatal("startup accepted corrupt probe policy")
	}
	if err := server.store.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/official/a6api-probes", `{"enabled":true}`)
	if recorder.Code != http.StatusInternalServerError || server.allowA6APIProbes.Load() {
		t.Fatal("failed persistence enabled probe admission")
	}
}

func TestOfficialIngressA6APIProbeException(t *testing.T) {
	t.Parallel()
	const body = `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`
	tests := []struct {
		name       string
		enabled    bool
		probeID    string
		selfTest   string
		key        string
		response   int
		wantStatus int
	}{
		{name: "disabled by default", probeID: "probe-1", key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "missing marker", enabled: true, key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "blank marker", enabled: true, probeID: " \t", key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "self-test disabled", selfTest: "pelican", key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "blank self-test", enabled: true, selfTest: " \t", key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "unknown self-test", enabled: true, selfTest: "other", key: "official-downstream-key", wantStatus: http.StatusForbidden},
		{name: "self-test missing key", enabled: true, selfTest: "pelican", wantStatus: http.StatusUnauthorized},
		{name: "self-test invalid key", enabled: true, selfTest: "pelican", key: "wrong-key", wantStatus: http.StatusUnauthorized},
		{name: "pelican self-test reaches upstream", enabled: true, selfTest: "pelican", key: "official-downstream-key", response: http.StatusOK, wantStatus: http.StatusOK},
		{name: "self-test upstream rejection", enabled: true, selfTest: "pelican", key: "official-downstream-key", response: http.StatusForbidden, wantStatus: http.StatusForbidden},
		{name: "missing key", enabled: true, probeID: "probe-1", wantStatus: http.StatusUnauthorized},
		{name: "invalid key", enabled: true, probeID: "probe-1", key: "wrong-key", wantStatus: http.StatusUnauthorized},
		{name: "admin key is not an ingress key", enabled: true, probeID: "probe-1", key: "admin-key", wantStatus: http.StatusUnauthorized},
		{name: "authenticated probe reaches upstream", enabled: true, probeID: "probe-1", key: "official-downstream-key", response: http.StatusOK, wantStatus: http.StatusOK},
		{name: "upstream rejection stays a rejection", enabled: true, probeID: "probe-1", key: "official-downstream-key", response: http.StatusForbidden, wantStatus: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var forwarded struct {
					Model    string `json:"model"`
					Messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				// Existing relay attribution may add metadata/system fields; the
				// probe's model and conversation must still reach the real upstream.
				if err := json.Unmarshal(raw, &forwarded); err != nil {
					t.Error(err)
				}
				if forwarded.Model != "claude-test" || len(forwarded.Messages) != 1 ||
					forwarded.Messages[0].Role != "user" || forwarded.Messages[0].Content != "hi" {
					t.Errorf("probe conversation changed: %s", raw)
				}
				w.WriteHeader(tc.response)
				_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"upstream rejected probe"}}`)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			server.allowA6APIProbes.Store(tc.enabled)
			// Real probes lack a CLI version and must bypass the version bounds too.
			server.officialVersion.set(officialVersionBounds{Min: "2.1.280"})
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			request.Header.Set("x-api-key", tc.key)
			request.Header.Set("X-A6API-Probe-Id", tc.probeID)
			request.Header.Set("X-A6API-Self-Test-Kind", tc.selfTest)
			request.Header.Set("anthropic-version", "2023-06-01")
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			wantCalls := 0
			if tc.response != 0 {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("upstream calls = %d, want %d", calls, wantCalls)
			}
			if tc.wantStatus != http.StatusUnauthorized {
				records := server.metrics.Recent(1)
				if len(records) != 1 || records[0].Ingress != ingressOfficial || records[0].ClientClass != clientClassAmbiguous {
					t.Fatalf("probe classification was not preserved: %#v", records)
				}
			}
		})
	}
}

func TestOfficialIngressClassifiesClaudeCodeRequestKinds(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)

	tests := []struct {
		name       string
		path       string
		body       string
		mutate     func(*http.Request)
		wantStatus int
		wantKind   string
	}{
		{
			name: "ordinary messages", path: "/v1/messages", body: officialClaudeCodeTestBody(),
			wantStatus: http.StatusOK, wantKind: clientKindMessages,
		},
		{
			name: "background fork messages", path: "/v1/messages", body: officialClaudeCodeTestBody(),
			mutate:     func(request *http.Request) { request.Header.Set("X-App", "cli-bg") },
			wantStatus: http.StatusOK, wantKind: clientKindMessages,
		},
		{
			name: "background fork missing beta", path: "/v1/messages", body: officialClaudeCodeTestBody(),
			mutate: func(request *http.Request) {
				request.Header.Set("X-App", "cli-bg")
				request.Header.Del("anthropic-beta")
			},
			wantStatus: http.StatusForbidden, wantKind: clientKindAmbiguous,
		},
		{
			name: "background fork missing metadata", path: "/v1/messages",
			body:       `{"model":"claude-sonnet-5","system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"hi"}]}`,
			mutate:     func(request *http.Request) { request.Header.Set("X-App", "cli-bg") },
			wantStatus: http.StatusForbidden, wantKind: clientKindAmbiguous,
		},
		{
			name: "unknown app rejects messages", path: "/v1/messages", body: officialClaudeCodeTestBody(),
			mutate:     func(request *http.Request) { request.Header.Set("X-App", "cli-other") },
			wantStatus: http.StatusForbidden, wantKind: clientKindAmbiguous,
		},
		{
			name: "count tokens without attribution", path: "/v1/messages/count_tokens",
			body:       `{"model":"claude-sonnet-5","messages":[],"tools":[]}`,
			wantStatus: http.StatusOK, wantKind: clientKindCountTokens,
		},
		{
			name: "messages token-count fallback", path: "/v1/messages",
			body:       `{"model":"claude-sonnet-5","max_tokens":1,"messages":[],"metadata":{"user_id":` + strconv.Quote(testClaudeCodeMetadata) + `}}`,
			wantStatus: http.StatusOK, wantKind: clientKindTokenCountFallback,
		},
		{
			name: "count tokens missing beta", path: "/v1/messages/count_tokens",
			body:       `{"model":"claude-sonnet-5","messages":[]}`,
			mutate:     func(request *http.Request) { request.Header.Del("anthropic-beta") },
			wantStatus: http.StatusForbidden, wantKind: clientKindAmbiguous,
		},
	}

	accepted := 0
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("x-api-key", "official-downstream-key")
			setOfficialClaudeCodeTestHeaders(request)
			if test.mutate != nil {
				test.mutate(request)
			}
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			records := server.metrics.Recent(1)
			if len(records) != 1 || records[0].ClientKind != test.wantKind {
				t.Fatalf("request record = %#v, want kind %q", records, test.wantKind)
			}
			if test.wantStatus == http.StatusOK {
				accepted++
			}
		})
	}
	if upstreamCalls != accepted {
		t.Fatalf("upstream calls = %d, want %d", upstreamCalls, accepted)
	}
}

// Both compatible policies are fenced: an official-pool account must never
// serve a request that did not come through the official key.
func TestCompatiblePoliciesNeverSelectOfficialPoolAccounts(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "shared", "token-shared", "22222222-2222-4222-8222-222222222222", "b")
	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}

	policies := []struct {
		name string
		key  string
	}{
		{name: ingressCompatible, key: "downstream-key"},
		{name: ingressExperimental, key: "experimental-downstream-key"},
	}
	for _, policy := range policies {
		for _, session := range []string{"one", "two"} {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("x-api-key", policy.key)
			request.Header.Set(claudeCodeSessionHeader, session)
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s status = %d, body = %s", policy.name, recorder.Code, recorder.Body.String())
			}
		}
	}
	if len(authorizations) != 4 {
		t.Fatalf("upstream calls = %#v", authorizations)
	}
	for _, authorization := range authorizations {
		if authorization != "Bearer token-shared" {
			t.Fatalf("compatible ingress reached a fenced account: %#v", authorizations)
		}
	}
	records := server.metrics.Recent(4)
	seen := map[string]bool{}
	for _, record := range records {
		seen[record.Ingress] = true
	}
	if !seen[ingressCompatible] || !seen[ingressExperimental] {
		t.Fatalf("request ingresses = %#v", seen)
	}
}

// Pool permeability is one way, so the official ingress keeps the whole account
// set and does not need a dedicated official-pool account to be placed first.
func TestOfficialIngressDrawsFromTheCompatiblePool(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	// The only account keeps its default compatible placement.
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(officialClaudeCodeTestBody()))
	request.Header.Set("x-api-key", "official-downstream-key")
	setOfficialClaudeCodeTestHeaders(request)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("official status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(authorizations) != 1 || authorizations[0] != "Bearer upstream-access-token" {
		t.Fatalf("official authorizations = %#v", authorizations)
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Ingress != ingressOfficial || records[0].Account != "default" {
		t.Fatalf("request record = %#v", records)
	}
}

// The account override is a selection hint, not a way around the fence.
func TestForcedAccountCannotEnterTheOfficialPoolFromCompatible(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "requested account cannot serve this traffic") ||
		strings.Contains(recorder.Body.String(), "default") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("fenced override made %d upstream calls", upstreamCalls)
	}
}

// The reverse override is allowed, because official traffic may use any pool.
func TestForcedAccountFromOfficialIngressReachesTheCompatiblePool(t *testing.T) {
	t.Parallel()
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(officialClaudeCodeTestBody()))
	request.Header.Set("x-api-key", "official-downstream-key")
	setOfficialClaudeCodeTestHeaders(request)
	request.Header.Set(accountHeader, "default")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("official override made %d upstream calls", upstreamCalls)
	}
}

func TestFailedAlternateSelectionDoesNotDoubleCountAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)

	stat := server.metrics.AccountStats()["default"]
	if stat.Requests != 1 || stat.Failures != 1 {
		t.Fatalf("account stats = %#v, want one failed upstream attempt", stat)
	}
}

func TestForwardAddsStandardAnthropicHeadersWhenMissing(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("content-type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Del("content-type")
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardPreservesClientAnthropicVersion(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != "client-version" {
			t.Errorf("anthropic-version = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("anthropic-version", "client-version")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardTreatsUnknownBillingFieldsAsOpaque(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;"}],"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded := decodeBody(t, got)
		system := decoded["system"].([]any)
		if text := system[0].(map[string]any)["text"]; text != "x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=00000;" {
			t.Fatalf("caller billing block changed: %q", text)
		}
		if _, ok := decoded["metadata"]; !ok {
			t.Fatal("minimum metadata was not added")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForwardAddsMinimumAttributionAndStableHeaderSession(t *testing.T) {
	t.Parallel()
	requestBody := `{"model":"claude-sonnet-5","system":"ordinary prompt","messages":[{"role":"user","content":"hello"}]}`
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, got)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	for range 2 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set(claudeCodeSessionHeader, "chat-42")
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream requests = %d", len(bodies))
	}
	first := decodeBody(t, bodies[0])
	second := decodeBody(t, bodies[1])
	firstSystem := first["system"].([]any)
	billing := firstSystem[0].(map[string]any)["text"].(string)
	const expectedBilling = "x-anthropic-billing-header: cc_version=2.1.280; cc_entrypoint=claude-desktop-3p;"
	if billing != expectedBilling || strings.Contains(billing, "cch=") {
		t.Fatalf("billing attribution = %q", billing)
	}
	if got := firstSystem[1].(map[string]any)["text"]; got != "ordinary prompt" {
		t.Fatalf("original system = %#v", got)
	}
	firstUserID := decodeUserID(t, first)
	secondUserID := decodeUserID(t, second)
	if firstUserID != secondUserID {
		t.Fatalf("header-derived session was not stable: %#v != %#v", firstUserID, secondUserID)
	}
	if firstUserID.AccountUUID != "11111111-1111-4111-8111-111111111111" || firstUserID.DeviceID != strings.Repeat("a", 64) {
		t.Fatalf("metadata identity does not match credential: %#v", firstUserID)
	}
}

func TestForwardPreservesExistingMetadataWithoutCCH(t *testing.T) {
	t.Parallel()
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=custom; cc_entrypoint=cli;"}],"metadata":{"user_id":"caller-owned"},"messages":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("already-attributed body changed: %s", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAttributionTransformIsIdempotent(t *testing.T) {
	t.Parallel()
	cred := credential.Credential{
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("b", 64),
	}
	headers := http.Header{"X-Session-Id": []string{"stable-session"}}
	body := []byte(`{"system":[{"type":"text","text":"keep me"}],"messages":[]}`)
	first, changed, err := addSubscriptionAttribution(body, headers, cred, true, "")
	if err != nil || !changed {
		t.Fatalf("first transform: changed=%v err=%v", changed, err)
	}
	second, changed, err := addSubscriptionAttribution(first, headers, cred, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(second) != string(first) {
		t.Fatalf("second transform was not idempotent: changed=%v\n%s\n%s", changed, first, second)
	}
}

func TestCountTokensAddsBillingWithoutMetadata(t *testing.T) {
	t.Parallel()
	requestBody := `{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hello"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := decodeBody(t, got)
		if _, exists := body["metadata"]; exists {
			t.Fatalf("count_tokens request contains unsupported metadata: %s", got)
		}
		system := body["system"].([]any)
		if got := system[0].(map[string]any)["text"]; got != defaultBillingAttribution {
			t.Fatalf("billing attribution = %#v", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"input_tokens":41}`)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(requestBody))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func officialClaudeCodeTestBody() string {
	return `{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.247.abc; cc_entrypoint=cli;"}],"metadata":{"user_id":` +
		strconv.Quote(testClaudeCodeMetadata) + `},"messages":[{"role":"user","content":"hi"}]}`
}

func setOfficialClaudeCodeTestHeaders(request *http.Request) {
	request.Header.Set("User-Agent", "claude-cli/2.1.247 (external, cli)")
	request.Header.Set(claudeCodeSessionHeader, "official-session")
	request.Header.Set("X-App", "cli")
	request.Header.Set("anthropic-beta", "claude-code-20250219")
	request.Header.Set("anthropic-version", "2023-06-01")
}

func decodeUserID(t *testing.T, body map[string]any) attributionUserID {
	t.Helper()
	metadata := body["metadata"].(map[string]any)
	var value attributionUserID
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAuthenticationRejectsWrongKey(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	request.Header.Set("x-api-key", "wrong-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestRelayAndAdminAuthenticationAreSeparated(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 1024)

	adminWithRelayKey := httptest.NewRecorder()
	adminRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	adminRequest.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(adminWithRelayKey, adminRequest)
	if adminWithRelayKey.Code != http.StatusUnauthorized {
		t.Fatalf("relay key on admin endpoint status = %d", adminWithRelayKey.Code)
	}

	relayWithAdminKey := httptest.NewRecorder()
	relayRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	relayRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(relayWithAdminKey, relayRequest)
	if relayWithAdminKey.Code != http.StatusUnauthorized {
		t.Fatalf("admin key on relay endpoint status = %d", relayWithAdminKey.Code)
	}

	adminWithAdminKey := httptest.NewRecorder()
	authorizedAdminRequest := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	authorizedAdminRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(adminWithAdminKey, authorizedAdminRequest)
	if adminWithAdminKey.Code != http.StatusOK {
		t.Fatalf("admin key on admin endpoint status = %d", adminWithAdminKey.Code)
	}
}

func TestWebUIIsPublicButManagementAPIStillRequiresAuthentication(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)

	page := httptest.NewRecorder()
	server.routes().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Claude Relay") {
		t.Fatalf("Web UI status = %d body = %s", page.Code, page.Body.String())
	}
	if got := page.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("Web UI omitted Content-Security-Policy")
	}

	asset := httptest.NewRecorder()
	server.routes().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "claudeRelayAdminKey") || !strings.Contains(asset.Body.String(), "renderExhaustedWindowChart") {
		t.Fatalf("Web UI asset status = %d", asset.Code)
	}
	chartAsset := httptest.NewRecorder()
	server.routes().ServeHTTP(chartAsset, httptest.NewRequest(http.MethodGet, "/assets/five-hour-chart.js", nil))
	if chartAsset.Code != http.StatusOK || !strings.Contains(chartAsset.Body.String(), "buildSeries") {
		t.Fatalf("five-hour chart asset status = %d", chartAsset.Code)
	}

	admin := httptest.NewRecorder()
	server.routes().ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil))
	if admin.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated management status = %d", admin.Code)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"too":"large"}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestForcedAccountHeaderSelectsAliasAndIsNotForwarded(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(accountHeader); got != "" {
			t.Errorf("private routing header leaked upstream: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "secondary")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(accountHeader); got != "secondary" {
		t.Fatalf("selected account response header = %q", got)
	}
}

func TestAuthenticationFailureDoesNotExposeAccountAlias(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account found=%v err=%v", found, err)
	}
	credential := account.Credential
	credential.ExpiresAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	credential.RefreshToken = ""
	if _, err := server.store.ImportAccount(t.Context(), account.Alias, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.SetAccountEnabled(t.Context(), account.Alias, true); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, account.Alias)
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "authentication is temporarily unavailable") ||
		strings.Contains(recorder.Body.String(), account.Alias) {
		t.Fatalf("authentication failure exposed account identity: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUnavailableAccountsUseStablePublicError(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	if _, err := server.store.SetAccountEnabled(t.Context(), "default", false); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-test","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), `"type":"api_error"`) ||
		!strings.Contains(recorder.Body.String(), accountUnavailableMessage) ||
		strings.Contains(recorder.Body.String(), "default") {
		t.Fatalf("unstable unavailable-account error: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStickySessionKeepsSelectedAccount(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`
	for range 2 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set("X-Claude-Session-Id", "conversation-1")
		server.routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	}
	if len(authorizations) != 2 || authorizations[0] != authorizations[1] {
		t.Fatalf("sticky session accounts = %#v", authorizations)
	}
}

func TestStickyOverloadReturns429WithoutSwitchingAccount(t *testing.T) {
	t.Parallel()
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	server.selector.maxInflightPerAcct = 1
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"model":"claude-test","messages":[{"role":"user","content":"sticky overload"}]}`
	headers := http.Header{"X-Claude-Session-Id": []string{"sticky-overload"}}
	route, err := deriveRequestRoute([]byte(body), headers, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	if err := server.store.Bind(t.Context(), route.ConversationKey, primary.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	holdPrimary := server.load.reserve(primary.ID)

	send := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", "downstream-key")
		request.Header.Set("X-Claude-Session-Id", "sticky-overload")
		server.routes().ServeHTTP(recorder, request)
		return recorder
	}
	first := send()
	if first.Code != http.StatusTooManyRequests || len(authorizations) != 0 {
		t.Fatalf("sticky overload status=%d authorizations=%#v body=%s", first.Code, authorizations, first.Body.String())
	}
	if strings.Contains(first.Body.String(), primary.Alias) ||
		!strings.Contains(first.Body.String(), `"type":"rate_limit_error"`) ||
		!strings.Contains(first.Body.String(), relayCapacityClientMessage) {
		t.Fatalf("sticky overload exposed account identity: %s", first.Body.String())
	}
	bound, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, store.AccountAccessCompatibleOnly, time.Now())
	if err != nil || !found || bound.ID != primary.ID {
		t.Fatalf("temporary fallback changed sticky binding: account=%#v found=%v err=%v", bound, found, err)
	}
	holdPrimary()

	second := send()
	if second.Code != http.StatusOK || len(authorizations) != 1 || authorizations[0] != "Bearer upstream-access-token" {
		t.Fatalf("sticky recovery status=%d authorizations=%#v body=%s", second.Code, authorizations, second.Body.String())
	}
	records := server.metrics.Recent(2)
	if len(records) != 2 || records[0].Selection != "sticky" || records[1].Status != http.StatusTooManyRequests || records[1].Account != "" {
		t.Fatalf("routing records = %#v", records)
	}
}

func TestUnboundRequestUsesLeastLoadedAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("least-loaded account authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	holdPrimary := server.load.reserve(primary.ID)
	defer holdPrimary()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"unbound load"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	records := server.metrics.Recent(1)
	if len(records) != 1 || records[0].Selection != "load_balance" || records[0].Account != "secondary" {
		t.Fatalf("load-balanced record = %#v", records)
	}
}

func TestForcedAccountDoesNotBypassWhenOverloaded(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("forced account authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	server.selector.maxInflightPerAcct = 1
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	primary, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("primary account = %#v found=%v err=%v", primary, found, err)
	}
	holdPrimary := server.load.reserve(primary.ID)
	defer holdPrimary()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"forced overload"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCooldownPolicyRequiresExplicitWindowExhaustion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)

	if got, ok := retryAfterHeader("1.5", now); !ok || got != 1500*time.Millisecond {
		t.Errorf("fractional Retry-After = %v, %v", got, ok)
	}
	date := now.Add(45 * time.Second).Format(http.TimeFormat)
	if got, ok := retryAfterHeader(date, now); !ok || got != 45*time.Second {
		t.Errorf("date Retry-After = %v, %v", got, ok)
	}

	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.31")
	decision, ok := cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-test", now)
	if !ok || decision.reason != cooldownReasonAmbiguous429 || decision.model != "claude-test" || !decision.until.Equal(now.Add(ambiguous429Cooldown)) {
		t.Fatalf("ambiguous 429 decision = %#v ok=%v", decision, ok)
	}
	headers.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	decision, ok = cooldownDecisionForResponse(headers, http.StatusTooManyRequests, "claude-test", now)
	if !ok || decision.reason != cooldownReasonFiveHourExhausted || decision.model != "" || !decision.until.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("exhausted 5h decision = %#v ok=%v", decision, ok)
	}
	if _, ok := cooldownDecisionForResponse(headers, 529, "claude-test", now); ok {
		t.Fatal("529 persisted an account cooldown")
	}
	if _, ok := cooldownDecisionForResponse(headers, http.StatusInternalServerError, "claude-test", now); ok {
		t.Fatal("500 persisted an account cooldown")
	}
}

func TestRetryableResponseSwitchesAccountOnce(t *testing.T) {
	t.Parallel()
	var firstAuthorization string
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			firstAuthorization = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if got := r.Header.Get("Authorization"); got == firstAuthorization {
			t.Errorf("retry reused first account: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status = %d calls = %d body = %s", recorder.Code, calls, recorder.Body.String())
	}
}

func TestGenericUpstreamFailuresFailOverWithoutCoolingAccounts(t *testing.T) {
	t.Parallel()
	statuses := []int{529, http.StatusInternalServerError}
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[calls])
		calls++
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	cooldowns, err := server.store.ActiveCooldowns(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 0 {
		t.Fatalf("generic upstream failures persisted cooldowns: %#v", cooldowns)
	}
}

func TestAmbiguous429FailsOverWithShortModelCooldown(t *testing.T) {
	t.Parallel()
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	cooldowns, err := server.store.ActiveCooldowns(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns) != 1 || cooldowns[0].Model != "claude-test" || cooldowns[0].Reason != cooldownReasonAmbiguous429 {
		t.Fatalf("ambiguous 429 cooldowns = %#v", cooldowns)
	}
	remaining := time.Until(time.Unix(cooldowns[0].UntilAt, 0))
	if remaining <= 0 || remaining > ambiguous429Cooldown {
		t.Fatalf("ambiguous 429 remaining cooldown = %v", remaining)
	}
}

func TestExplicitFiveHourExhaustionCoolsAccountAcrossModels(t *testing.T) {
	t.Parallel()
	var calls int
	var firstAuthorization string
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			firstAuthorization = r.Header.Get("Authorization")
			w.Header().Set("anthropic-ratelimit-unified-5h-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"retry me"}]}`))
	request.Header.Set("x-api-key", "downstream-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
	alias := "secondary"
	if firstAuthorization == "Bearer upstream-access-token" {
		alias = "default"
	}
	account, found, err := server.store.AccountByAlias(t.Context(), alias)
	if err != nil || !found {
		t.Fatalf("first account %q found=%v err=%v", alias, found, err)
	}
	cooling, err := server.store.IsCooling(t.Context(), account.ID, "claude-other-model", time.Now())
	if err != nil || !cooling {
		t.Fatalf("cross-model cooling=%v err=%v", cooling, err)
	}
	windows, err := server.store.FiveHourWindows(t.Context(), true, time.Now().UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].Account != alias || windows[0].ResetsAt != strconv.FormatInt(reset.Unix(), 10) {
		t.Fatalf("exhausted windows=%+v", windows)
	}
}

func TestUnknownBillingFieldsDoNotOverrideForcedAccount(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-secondary" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "secondary", "token-secondary", "22222222-2222-4222-8222-222222222222", "b")
	body := `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=x; cc_entrypoint=cli; cch=abcde;"}],"metadata":{"user_id":"{\"account_uuid\":\"11111111-1111-4111-8111-111111111111\",\"session_id\":\"s\"}"},"messages":[]}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "secondary")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestRoutingPrefixIgnoresContentAfterCacheBreakpoint(t *testing.T) {
	t.Parallel()
	headers := http.Header{}
	first, err := deriveRequestRoute([]byte(`{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"one"}]}`), headers, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	second, err := deriveRequestRoute([]byte(`{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"different tail"}]}`), headers, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectionKey != second.SelectionKey {
		t.Fatalf("cache prefix keys differ: %q != %q", first.SelectionKey, second.SelectionKey)
	}
	if first.StickyTTL != defaultCacheStickyTTL || second.StickyTTL != defaultCacheStickyTTL {
		t.Fatalf("default cache sticky TTLs = %v, %v", first.StickyTTL, second.StickyTTL)
	}
}

func TestCompatibleIngressPoliciesUseSeparateRoutingScopes(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"same"}]}`)
	headers := http.Header{sessionHeaderNames[0]: []string{"same-session"}}
	compatible, err := deriveRequestRoute(body, headers, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	experimental, err := deriveRequestRoute(body, headers, experimentalIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if compatible.AccountAccess != experimental.AccountAccess {
		t.Fatalf("account access differs: %q != %q", compatible.AccountAccess, experimental.AccountAccess)
	}
	if compatible.ConversationKey == experimental.ConversationKey || compatible.SelectionKey == experimental.SelectionKey {
		t.Fatalf("routing scopes collided: compatible=%#v experimental=%#v", compatible, experimental)
	}
}

func TestRoutingStickyTTLSeparatesSessionsCachesAndFallbacks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		headers http.Header
		wantTTL time.Duration
	}{
		{
			name:    "explicit session remains one hour",
			body:    `{"model":"m","messages":[{"role":"user","content":"hello"}]}`,
			headers: http.Header{"X-Claude-Session-Id": []string{"session-a"}},
			wantTTL: sessionStickyTTL,
		},
		{
			name:    "cache breakpoint defaults to five minutes",
			body:    `{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			wantTTL: defaultCacheStickyTTL,
		},
		{
			name:    "request level automatic cache defaults to five minutes",
			body:    `{"model":"m","cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"hello"}]}`,
			wantTTL: defaultCacheStickyTTL,
		},
		{
			name:    "request level automatic cache follows one hour declaration",
			body:    `{"model":"m","cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hello"}]}`,
			wantTTL: time.Hour,
		},
		{
			name: "longest included cache lifetime wins",
			body: `{"model":"m","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"turn","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`,
			wantTTL: time.Hour,
		},
		{
			name:    "ordinary anchor is selection only",
			body:    `{"model":"m","system":"stable","messages":[{"role":"user","content":"hello"}]}`,
			wantTTL: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := deriveRequestRoute([]byte(test.body), test.headers, compatibleIngress, "/v1/messages")
			if err != nil {
				t.Fatal(err)
			}
			if route.StickyTTL != test.wantTTL {
				t.Fatalf("sticky TTL = %v, want %v", route.StickyTTL, test.wantTTL)
			}
			if route.ConversationKey == "" || route.SelectionKey == "" {
				t.Fatalf("route lost stable identity: %#v", route)
			}
		})
	}
}

func TestStickyTTLAtCompletionUsesCacheObservationTime(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	cacheRoute := requestRoute{ConversationKey: "prefix:cache", StickyTTL: defaultCacheStickyTTL}
	if got := stickyTTLAtCompletion(cacheRoute, observedAt, observedAt.Add(2*time.Minute)); got != 3*time.Minute {
		t.Fatalf("cache TTL after stream = %v, want 3m", got)
	}
	if got := stickyTTLAtCompletion(cacheRoute, observedAt, observedAt.Add(6*time.Minute)); got != 0 {
		t.Fatalf("expired cache TTL after stream = %v, want 0", got)
	}
	sessionRoute := requestRoute{ConversationKey: "session:explicit", StickyTTL: sessionStickyTTL}
	if got := stickyTTLAtCompletion(sessionRoute, observedAt, observedAt.Add(10*time.Minute)); got != sessionStickyTTL {
		t.Fatalf("session TTL after stream = %v, want %v", got, sessionStickyTTL)
	}
}

func TestOnlyDeclaredAffinityPersistsAfterSuccess(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	tests := []struct {
		name          string
		body          string
		foundAfter    time.Duration
		wantFound     bool
		notFoundAfter time.Duration
	}{
		{
			name:      "ordinary fallback is not persisted",
			body:      `{"model":"m","system":"no-cache","messages":[{"role":"user","content":"fallback"}]}`,
			wantFound: false,
		},
		{
			name:          "default cache persists within five minutes",
			body:          `{"model":"m","system":[{"type":"text","text":"five","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			foundAfter:    4 * time.Minute,
			wantFound:     true,
			notFoundAfter: 6 * time.Minute,
		},
		{
			name:          "one hour cache persists beyond five minutes",
			body:          `{"model":"m","system":[{"type":"text","text":"hour","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`,
			foundAfter:    30 * time.Minute,
			wantFound:     true,
			notFoundAfter: 61 * time.Minute,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route, err := deriveRequestRoute([]byte(test.body), nil, compatibleIngress, "/v1/messages")
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			request.Header.Set("x-api-key", "downstream-key")
			server.routes().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			_, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey,
				store.AccountAccessCompatibleOnly, time.Now().Add(test.foundAfter))
			if err != nil || found != test.wantFound {
				t.Fatalf("binding found=%v want=%v err=%v route=%#v", found, test.wantFound, err, route)
			}
			if test.notFoundAfter > 0 {
				_, found, err = server.store.BoundAccount(t.Context(), route.ConversationKey,
					store.AccountAccessCompatibleOnly, time.Now().Add(test.notFoundAfter))
				if err != nil || found {
					t.Fatalf("expired binding found=%v err=%v route=%#v", found, err, route)
				}
			}
		})
	}
}

func TestOrdinaryFallbackIgnoresLegacyPrefixBinding(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	route, err := deriveRequestRoute([]byte(`{"model":"m","messages":[{"role":"user","content":"legacy"}]}`), nil, compatibleIngress, "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account found=%v err=%v", found, err)
	}
	if err := server.store.Bind(t.Context(), route.ConversationKey, account.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	selected, err := server.selector.selectAccount(t.Context(), route, "", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	defer selected.release()
	defer selected.releaseSession()
	if selected.Source == "sticky" {
		t.Fatal("ordinary fallback honored a legacy persisted binding")
	}
}

func TestAdminAccountLifecycleDoesNotExposeTokens(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	request.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "upstream-access-token") || strings.Contains(recorder.Body.String(), "refresh_token\":\"") {
		t.Fatalf("account response exposed token material: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/admin/v1/accounts/default/disable", nil)
	request.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"enabled":false`) {
		t.Fatalf("disable status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set(accountHeader, "default")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled forced account status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestOAuthManagementFlowImportsDisabledAccount(t *testing.T) {
	t.Parallel()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"oauth-access","refresh_token":"oauth-refresh","expires_in":28800,"account":{"uuid":"33333333-3333-4333-8333-333333333333","email_address":"oauth@example.com"}}`)
	}))
	defer tokenServer.Close()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	oauthClient := claudeoauth.NewForTest(tokenServer.Client(), "https://example.test/authorize", tokenServer.URL, "https://example.test/callback")
	server.oauth = oauthClient
	server.tokens.oauth = oauthClient

	startRecorder := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/oauth/claude/start", strings.NewReader(`{"alias":"oauth-account"}`))
	startRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d body = %s", startRecorder.Code, startRecorder.Body.String())
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
		SessionID        string `json:"session_id"`
	}
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	parsedURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsedURL.Query().Get("state")
	exchangeBody, _ := json.Marshal(map[string]string{"session_id": started.SessionID, "code": "auth-code#" + state})
	exchangeRecorder := httptest.NewRecorder()
	exchangeRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/oauth/claude/exchange", strings.NewReader(string(exchangeBody)))
	exchangeRequest.Header.Set("x-api-key", "admin-key")
	server.routes().ServeHTTP(exchangeRecorder, exchangeRequest)
	if exchangeRecorder.Code != http.StatusCreated || !strings.Contains(exchangeRecorder.Body.String(), `"enabled":false`) {
		t.Fatalf("exchange status = %d body = %s", exchangeRecorder.Code, exchangeRecorder.Body.String())
	}
	account, found, err := server.store.AccountByAlias(t.Context(), "oauth-account")
	if err != nil || !found || account.Enabled || account.RefreshToken != "oauth-refresh" {
		t.Fatalf("OAuth account = %#v found=%v err=%v", account, found, err)
	}
}

func TestTokenManagerPersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800}`)
	}))
	defer tokenServer.Close()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, err := server.store.ImportAccount(t.Context(), "refreshing", credential.Credential{
		Type:         "claude",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).Format(time.RFC3339),
		AccountUUID:  "44444444-4444-4444-8444-444444444444",
		DeviceID:     strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err = server.store.SetAccountEnabled(t.Context(), account.Alias, true)
	if err != nil {
		t.Fatal(err)
	}
	oauthClient := claudeoauth.NewForTest(tokenServer.Client(), "https://example.test/authorize", tokenServer.URL, "https://example.test/callback")
	manager := tokenManager{store: server.store, oauth: oauthClient, autoRefresh: true}
	updated, err := manager.ensureFresh(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessToken != "new-access" || updated.RefreshToken != "new-refresh" {
		t.Fatalf("updated account tokens were not rotated")
	}
	persisted, _, err := server.store.AccountByID(t.Context(), account.ID)
	if err != nil || persisted.RefreshToken != "new-refresh" {
		t.Fatalf("persisted account refresh token was not rotated: err=%v", err)
	}
}

func newTestServer(t *testing.T, upstreamURL string, maxRequestBytes int64) *Server {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.ImportAccount(t.Context(), "default", credential.Credential{
		Type:        "claude",
		AccessToken: "upstream-access-token",
		AccountUUID: "11111111-1111-4111-8111-111111111111",
		DeviceID:    strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetAccountEnabled(t.Context(), "default", true); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(config.Config{
		Listen:             "127.0.0.1:0",
		RelayAPIKey:        "downstream-key",
		ExperimentalAPIKey: "experimental-downstream-key",
		OfficialAPIKey:     "official-downstream-key",
		AdminAPIKey:        "admin-key",
		CredentialsFile:    "unused.json",
		UpstreamBaseURL:    upstreamURL,
		MaxRequestBytes:    maxRequestBytes,
		RequestLogSize:     50,
		AutoRefresh:        true,
	}, database)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func importTestAccount(t *testing.T, database *store.Store, alias, token, accountUUID, deviceByte string) {
	t.Helper()
	_, err := database.ImportAccount(t.Context(), alias, credential.Credential{
		Type:        "claude",
		AccessToken: token,
		AccountUUID: accountUUID,
		DeviceID:    strings.Repeat(deviceByte, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetAccountEnabled(t.Context(), alias, true); err != nil {
		t.Fatal(err)
	}
}
