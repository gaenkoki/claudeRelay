package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/metrics"
)

const observedExtraUsageMessage = "Third-party apps now draw from extra usage, not plan limits. Ask your workspace admin to add more and keep going."

func extraUsageErrorBody(message string) string {
	body, _ := json.Marshal(map[string]any{
		"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": message},
	})
	return string(body)
}

func TestClassifyUpstreamErrorRequiresObservedErrorShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		truncated bool
		want      string
	}{
		{"observed", 400, extraUsageErrorBody(observedExtraUsageMessage), false, metrics.UpstreamErrorThirdPartyExtraUsage},
		{"your variant", 400, extraUsageErrorBody("Third-party apps now draw from your extra usage, not your plan limits."), false, metrics.UpstreamErrorThirdPartyExtraUsage},
		{"other status", 429, extraUsageErrorBody(observedExtraUsageMessage), false, ""},
		{"ordinary validation", 400, extraUsageErrorBody("tool_choice: type tool is not supported"), false, ""},
		{"echoed message", 400, extraUsageErrorBody("Invalid user input: " + observedExtraUsageMessage), false, ""},
		{"wrong envelope", 400, `{"type":"message","error":{"type":"invalid_request_error","message":` + strconv.Quote(observedExtraUsageMessage) + `}}`, false, ""},
		{"wrong error type", 400, `{"type":"error","error":{"type":"rate_limit_error","message":` + strconv.Quote(observedExtraUsageMessage) + `}}`, false, ""},
		{"malformed", 400, `{"error":`, false, ""},
		{"trailing data", 400, extraUsageErrorBody(observedExtraUsageMessage) + `{}`, false, ""},
		{"truncated", 400, extraUsageErrorBody(observedExtraUsageMessage), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpstreamError(tc.status, upstreamErrorPreview{body: []byte(tc.body), truncated: tc.truncated}); got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtraUsageDiagnosticsPreserveErrorResponseAndExcludeRawBody(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"observed", extraUsageErrorBody(observedExtraUsageMessage + " secret-access user@example.com"), metrics.UpstreamErrorThirdPartyExtraUsage},
		{"other validation", extraUsageErrorBody("tool_choice is invalid: secret-access user@example.com"), ""},
		{"malformed JSON", `{"error":"secret-access`, ""},
		{"oversized", extraUsageErrorBody(observedExtraUsageMessage + strings.Repeat("x", 9<<10)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "upstream-diagnostic-request")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-test","tools":[{"name":"web_search_exa","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("x-api-key", "experimental-downstream-key")
			recorder := httptest.NewRecorder()
			server.routes().ServeHTTP(recorder, request)
			if calls != 1 || recorder.Code != http.StatusBadRequest || recorder.Body.String() != tc.body || recorder.Header().Get("Request-Id") != "upstream-diagnostic-request" {
				t.Fatalf("response changed or retried: calls=%d status=%d bytes=%d", calls, recorder.Code, recorder.Body.Len())
			}
			records := server.metrics.Recent(1)
			if len(records) != 1 || records[0].UpstreamErrorCategory != tc.want {
				t.Fatalf("records=%+v", records)
			}
			wantCount := int64(0)
			if tc.want != "" {
				wantCount = 1
				if !strings.Contains(logs.String(), "upstream third-party extra usage required") {
					t.Fatal("missing categorized diagnostic log")
				}
			}
			summary := server.metrics.Summary(time.Now())
			if summary.ThirdPartyExtraUsageRequests != wantCount || summary.RecentThirdPartyExtraUsageRequests != wantCount {
				t.Fatalf("summary=%+v", summary)
			}
			encoded, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"secret-access", "user@example.com", observedExtraUsageMessage} {
				if strings.Contains(string(encoded), forbidden) || strings.Contains(logs.String(), forbidden) {
					t.Fatalf("raw upstream error leaked: %s", forbidden)
				}
			}
		})
	}
}
