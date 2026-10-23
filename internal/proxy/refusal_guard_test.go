package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestRefusalGuardFingerprints(t *testing.T) {
	g := &refusalGuard{secret: []byte("test-secret")}
	derive := func(body string, headers http.Header, ingress string) refusalFingerprints {
		t.Helper()
		keys, err := g.fingerprints([]byte(body), headers, ingress)
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	base := derive(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, nil, "compatible")
	refused := base.record[len(base.record)-1]
	for _, tc := range []struct {
		name, body string
		match      bool
	}{
		{"same", `{"messages":[{"content":"hello","role":"user"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, true},
		{"continuation", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"},{"role":"assistant","content":"cannot help"},{"role":"user","content":"try again"}]}`, true},
		{"changed session and model", `{"model":"other","metadata":{"user_id":"{\"session_id\":\"new-session\"}"},"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, true},
		{"cache annotation", `{"tools":[],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]},{"role":"assistant","content":"hi"},{"role":"user","content":[{"text":"refused request","type":"text"}]}]}`, true},
		{"billing annotation", `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=changed; cch=123;"}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, true},
		{"different system", `{"system":"different instructions","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, false},
		{"same role merged", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":[{"type":"text","text":"refused request"},{"type":"text","text":"try again"}]}]}`, true},
		{"shared harmless history", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"normal question"}]}`, false},
		{"new conversation", `{"messages":[{"role":"user","content":"normal question"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := derive(tc.body, nil, "compatible")
			found := false
			for _, key := range keys.lookup {
				found = found || key == refused
			}
			if found != tc.match {
				t.Fatalf("match=%v want=%v", found, tc.match)
			}
		})
	}
	if got := derive(`{"messages":[]}`, nil, "compatible"); len(got.record) != 0 {
		t.Fatal("empty conversation created shared fingerprint")
	}
	if got := derive(`{"messages":[{"role":"user","content":[]}]}`, nil, "compatible"); len(got.record) != 0 {
		t.Fatal("empty blocks created shared fingerprint")
	}
	for _, header := range sessionHeaderNames {
		keys := derive(`{"messages":[]}`, http.Header{http.CanonicalHeaderKey(header): []string{"session"}}, "compatible")
		metadata := derive(`{"metadata":{"user_id":"{\"session_id\":\"session\"}"},"messages":[]}`, nil, "compatible")
		if len(keys.record) != 1 || keys.record[0] != metadata.record[0] {
			t.Fatalf("header %s did not match metadata", header)
		}
	}
	otherIngress := derive(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"refused request"}]}`, nil, "official")
	if otherIngress.record[0] == refused {
		t.Fatal("fingerprints crossed ingress boundaries")
	}
	tool := func(value string) refusalFingerprints {
		return derive(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"id","content":{"cache_control":"`+value+`"}}]}]}`, nil, "compatible")
	}
	if tool("one").record[0] == tool("two").record[0] {
		t.Fatal("normalization stripped tool data")
	}
}

func TestRefusalGuardForward(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/v1/messages/count_tokens" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"input_tokens":10}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				body := `{"model":"claude-test","stop_reason":"refusal","stop_details":{"category":"bio"},"usage":{"input_tokens":10,"output_tokens":1}}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":10}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"category\":\"bio\"}},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			const body = `{"model":"claude-test","system":"shared","messages":[{"role":"user","content":"refused request"}]}`
			forward := func(path, body, session string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("x-api-key", "downstream-key")
				if session != "" {
					r.Header.Set("X-Session-Id", session)
				}
				w := httptest.NewRecorder()
				server.routes().ServeHTTP(w, r)
				return w
			}
			if first := forward("/v1/messages", body, "original"); first.Code != 200 {
				t.Fatalf("first refusal changed: %d %s", first.Code, first.Body.String())
			}
			guard, err := loadRefusalGuard(server.store)
			if err != nil || guard.digest("restart") != server.refusalGuard.digest("restart") {
				t.Fatalf("fingerprint secret did not survive reload: %v", err)
			}
			for _, tc := range []struct{ body, session string }{
				{body, "original"},
				{body, "changed"},
				{body, ""},
				{strings.Replace(body, `"refused request"`, `"completely different request"`, 1), "original"},
				{`{"model":"claude-test","system":"shared","messages":[{"role":"user","content":"refused request"},{"role":"assistant","content":"no"},{"role":"user","content":"again"}]}`, "changed"},
			} {
				w := forward("/v1/messages", tc.body, tc.session)
				var response struct {
					Error struct {
						Type, Code, Message string
						Seconds             int64 `json:"retry_after_seconds"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if w.Code != 403 || response.Error.Type != "permission_error" || response.Error.Code != "refusal_cooldown" || response.Error.Seconds < 590 || response.Error.Seconds > 600 || !strings.Contains(response.Error.Message, "请修改请求内容") || !strings.Contains(response.Error.Message, "Please revise the request content") || w.Header().Get("Retry-After") != strconv.FormatInt(response.Error.Seconds, 10) {
					t.Fatalf("invalid cooldown response: status=%d body=%s", w.Code, w.Body.String())
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("blocked requests reached upstream: calls=%d", calls.Load())
			}
			if w := forward("/v1/messages/count_tokens", body, "original"); w.Code != 200 {
				t.Fatalf("count_tokens was blocked: %d %s", w.Code, w.Body.String())
			}
			if w := forward("/v1/messages", strings.Replace(body, "refused request", "normal question", 1), "unrelated"); w.Code != 200 {
				t.Fatalf("unrelated conversation blocked: %d %s", w.Code, w.Body.String())
			}
			if calls.Load() != 3 {
				t.Fatalf("upstream calls=%d want=3", calls.Load())
			}
			keys, err := server.refusalGuard.fingerprints([]byte(body), nil, compatibleIngress.Name)
			if err != nil {
				t.Fatal(err)
			}
			state, err := server.store.RefusalCooldown(t.Context(), keys.lookup, time.Now())
			if err != nil || state.Strikes != 1 {
				t.Fatalf("blocked retries escalated strikes: %+v err=%v", state, err)
			}
		})
	}
}

func TestRefusalCooldownResponseRoundsUp(t *testing.T) {
	now := time.Now()
	w := httptest.NewRecorder()
	writeRefusalCooldown(w, store.RefusalCooldown{UntilAt: now.Add(time.Millisecond).UnixMilli()}, now)
	if w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), "0 分 1 秒") {
		t.Fatalf("incorrect remaining time: %s", w.Body.String())
	}
}

func TestRefusalGuardEscalatesAfterExpiredCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"stop_reason":"refusal","stop_details":{"category":null},"usage":{"input_tokens":10,"output_tokens":0}}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	const body = `{"model":"claude-test","messages":[{"role":"user","content":"refused request"}]}`
	keys, err := server.refusalGuard.fingerprints([]byte(body), nil, compatibleIngress.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.RecordRefusalCooldown(t.Context(), keys.lookup, keys.record, "bio", time.Now().Add(-11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	r.Header.Set("x-api-key", "downstream-key")
	w := httptest.NewRecorder()
	server.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expired cooldown blocked admission: %d %s", w.Code, w.Body.String())
	}
	state, err := server.store.RefusalCooldown(t.Context(), keys.lookup, time.Now())
	remaining := time.Until(time.UnixMilli(state.UntilAt))
	if err != nil || state.Strikes != 2 || state.Category != "unknown" || remaining < 59*time.Minute || remaining > time.Hour {
		t.Fatalf("unknown refusal did not escalate: %+v err=%v", state, err)
	}
}

func TestRefusalGuardDatabaseFailureStopsAdmission(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	if err := server.store.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("x-api-key", "downstream-key")
	w := httptest.NewRecorder()
	server.routes().ServeHTTP(w, r)
	if w.Code != 503 || calls.Load() != 0 || !strings.Contains(w.Body.String(), "failed to check refusal cooldown") {
		t.Fatalf("database failure admitted request: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	}
}
