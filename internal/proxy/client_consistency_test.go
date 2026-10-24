package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func consistencyTestRequest() (map[string]any, http.Header) {
	body := map[string]any{
		"model":    "claude-test",
		"system":   []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.295.768; cc_entrypoint=sdk-cli;"}},
		"metadata": map[string]any{"user_id": testClaudeCodeMetadata},
		"messages": []any{map[string]any{"role": "user", "content": "Reply with OK only. Do not read or write files."}},
	}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.295 (external, sdk-cli)")
	headers.Set(claudeCodeSessionHeader, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	headers.Set("X-App", "cli")
	headers.Set("anthropic-beta", "claude-code-20250219")
	headers.Set("anthropic-version", "2023-06-01")
	return body, headers
}

func TestClientConsistencyChecksClaimsWithoutRestrictingModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(map[string]any, http.Header)
		reason   string
		checksum string
		cch      bool
	}{
		{name: "matching SDK request", checksum: "matched"},
		{name: "background app is not an entrypoint claim", mutate: func(_ map[string]any, h http.Header) { h.Set("X-App", "cli-bg") }, checksum: "matched"},
		{name: "session mismatch", mutate: func(_ map[string]any, h http.Header) { h.Set(claudeCodeSessionHeader, "private-session-header") }, reason: "session_mismatch", checksum: "matched"},
		{name: "version mismatch", mutate: func(_ map[string]any, h http.Header) { h.Set("User-Agent", "claude-cli/2.1.286 (external, sdk-cli)") }, reason: "version_mismatch"},
		{name: "entrypoint mismatch", mutate: func(_ map[string]any, h http.Header) { h.Set("User-Agent", "claude-cli/2.1.295 (external, cli)") }, reason: "entrypoint_mismatch", checksum: "matched"},
		{name: "UA without entrypoint", mutate: func(_ map[string]any, h http.Header) { h.Set("User-Agent", "claude-cli/2.1.295") }, checksum: "matched"},
		{name: "legacy metadata", mutate: func(b map[string]any, _ http.Header) {
			b["metadata"] = map[string]any{"user_id": "user_" + strings.Repeat("a", 64) + "_account__session_aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
		}, checksum: "matched"},
		{name: "CCH is observational", mutate: func(b map[string]any, _ http.Header) {
			b["system"].([]any)[0].(map[string]any)["text"] = "x-anthropic-billing-header: cc_version=2.1.295.768; cc_entrypoint=sdk-cli; cch=arbitrary;"
		}, checksum: "matched", cch: true},
		{name: "wrong checksum is observational", mutate: func(b map[string]any, _ http.Header) {
			b["system"].([]any)[0].(map[string]any)["text"] = "x-anthropic-billing-header: cc_version=2.1.295.000; cc_entrypoint=sdk-cli;"
		}, checksum: "mismatched"},
		{name: "reminders before prompt", mutate: func(b map[string]any, _ http.Header) {
			b["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "Synthetic client reminder."}, map[string]any{"type": "text", "text": "Reply with OK only. Do not read or write files."}}
		}, checksum: "matched"},
		{name: "short custom prompt and no tools", mutate: func(b map[string]any, _ http.Header) {
			b["system"] = append(b["system"].([]any), map[string]any{"type": "text", "text": "Short custom prompt."})
			b["tools"] = []any{}
		}, checksum: "matched"},
		{name: "unknown CLI version", mutate: func(b map[string]any, h http.Header) {
			h.Set("User-Agent", "claude-cli/2.2.0 (external, sdk-cli)")
			b["system"].([]any)[0].(map[string]any)["text"] = "x-anthropic-billing-header: cc_version=2.2.0.abc; cc_entrypoint=sdk-cli;"
		}, checksum: "unsupported_version"},
		{name: "no attribution on auxiliary request", mutate: func(b map[string]any, _ http.Header) { delete(b, "system"); delete(b, "metadata"); b["max_tokens"] = 1 }, checksum: "not_present"},
		{name: "no billing but official identity prompt", mutate: func(b map[string]any, _ http.Header) {
			b["system"] = "You are Claude Code, Anthropic's official CLI for Claude."
		}, checksum: "not_present"},
		{name: "malformed billing version", mutate: func(b map[string]any, _ http.Header) {
			b["system"] = "x-anthropic-billing-header: cc_version=not-a-version; cc_entrypoint=sdk-cli;"
		}, reason: "version_mismatch"},
		{name: "duplicate version cannot mask mismatch", mutate: func(b map[string]any, _ http.Header) {
			b["system"] = "x-anthropic-billing-header: cc_version=1.0.0; cc_version=2.1.295.768; cc_entrypoint=sdk-cli;"
		}, reason: "version_mismatch"},
		{name: "second block cannot mask mismatch", mutate: func(b map[string]any, _ http.Header) {
			b["system"] = append([]any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=1.0.0;"}}, b["system"].([]any)...)
		}, reason: "version_mismatch"},
		{name: "Desktop third party alias", mutate: func(b map[string]any, h http.Header) {
			h.Set("User-Agent", "claude-cli/2.1.295 (external, claude-desktop)")
			b["system"] = "x-anthropic-billing-header: cc_version=2.1.295.768; cc_entrypoint=claude-desktop-3p;"
		}, checksum: "matched"},
		{name: "CCH in user text is not attribution", mutate: func(b map[string]any, _ http.Header) {
			b["messages"].([]any)[0].(map[string]any)["content"] = "Please explain cch=12345."
		}, checksum: "mismatched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, headers := consistencyTestRequest()
			if tc.mutate != nil {
				tc.mutate(body, headers)
			}
			got := inspectClientConsistency(body, headers)
			if tc.name == "legacy metadata" && got.Session != "matched" {
				t.Fatalf("legacy session was not compared: %+v", got)
			}
			if tc.reason == "" && len(got.Violations) != 0 || tc.reason != "" && !slices.Contains(got.Violations, tc.reason) {
				t.Fatalf("violations=%v want=%q", got.Violations, tc.reason)
			}
			if tc.checksum != "" && got.Checksum != tc.checksum {
				t.Fatalf("checksum=%q want=%q", got.Checksum, tc.checksum)
			}
			if got.CCH != tc.cch {
				t.Fatalf("CCH=%v want=%v", got.CCH, tc.cch)
			}
		})
	}
}

func TestOfficialIngressRejectsInconsistencyBeforeUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 1<<20)
	for _, tc := range []struct {
		name, key string
		mutate    func(map[string]any, http.Header)
		status    int
		reason    string
	}{
		{name: "matching request", key: "official-downstream-key", status: 200},
		{name: "session mismatch", key: "official-downstream-key", mutate: func(_ map[string]any, h http.Header) { h.Set(claudeCodeSessionHeader, "private-session-header") }, status: 403, reason: "session_mismatch"},
		{name: "version mismatch", key: "official-downstream-key", mutate: func(_ map[string]any, h http.Header) { h.Set("User-Agent", "claude-cli/2.1.286 (external, sdk-cli)") }, status: 403, reason: "version_mismatch"},
		{name: "entrypoint mismatch", key: "official-downstream-key", mutate: func(_ map[string]any, h http.Header) { h.Set("User-Agent", "claude-cli/2.1.295 (external, cli)") }, status: 403, reason: "entrypoint_mismatch"},
		{name: "compatible remains admitted", key: "downstream-key", mutate: func(_ map[string]any, h http.Header) { h.Set(claudeCodeSessionHeader, "private-session-header") }, status: 200},
		{name: "experimental remains admitted", key: "experimental-downstream-key", mutate: func(_ map[string]any, h http.Header) { h.Set(claudeCodeSessionHeader, "private-session-header") }, status: 200},
		{name: "checksum mismatch is observational", key: "official-downstream-key", mutate: func(b map[string]any, _ http.Header) {
			b["messages"].([]any)[0].(map[string]any)["content"] = "Different prompt"
		}, status: 200},
		{name: "CCH is observational", key: "official-downstream-key", mutate: func(b map[string]any, _ http.Header) {
			b["system"].([]any)[0].(map[string]any)["text"] = "x-anthropic-billing-header: cc_version=2.1.295.768; cc_entrypoint=sdk-cli; cch=00000;"
		}, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, headers := consistencyTestRequest()
			if tc.mutate != nil {
				tc.mutate(body, headers)
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(encoded)))
			r.Header = headers
			r.Header.Set("x-api-key", tc.key)
			w := httptest.NewRecorder()
			before := calls
			server.routes().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 403 && (calls != before || strings.Contains(w.Body.String(), tc.reason) || !strings.Contains(w.Body.String(), "该分组仅支持 Claude Code 请求")) {
				t.Fatalf("unexpected upstream call or invalid public message: calls=%d body=%s", calls, w.Body.String())
			}
			if tc.status == 200 && calls != before+1 {
				t.Fatalf("accepted request not forwarded: calls=%d", calls)
			}
			record := server.metrics.Recent(1)[0]
			if record.ClientConsistency == nil {
				t.Fatal("missing consistency record")
			}
			if tc.status == 403 && (record.RelayAction != "client_consistency_rejected" || !slices.Contains(record.ClientConsistency.Violations, tc.reason)) {
				t.Fatalf("invalid rejected request record: %+v", record)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-session-header") || strings.Contains(string(data), "0123456789abcdef0123456789abcdef") {
				t.Fatal("raw identity leaked to request record")
			}
		})
	}
}

func TestNativeVersionChecksumCaptureVectors(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Reply with OK only. Do not read or write files.", "768"},
		{"Read fixture.txt and reply OK.", "dac"},
		{"你好，只回复好的。", "3ce"},
		{"OK", "8a8"},
		{"ab😀cd😀efghijklmnop😀xyz", "1e3"},
		{"😀😀😀😀😀😀😀😀😀😀😀", "081"},
		{"", "8a8"},
	} {
		if got := nativeVersionChecksum(tc.text, "2.1.295"); got != tc.want {
			t.Errorf("capture checksum=%s want=%s", got, tc.want)
		}
	}
}
