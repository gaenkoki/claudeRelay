package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These names reproduced the upstream extra-usage 400 on 2026-10-24. Keep this
// fixture in tests only: the relay does not maintain an upstream tool blacklist.
func TestExperimentalIngressAdaptsObservedRejectedToolNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"web_search_exa", "get_code_context_exa", "mcp_weather"} {
		for _, toolType := range []string{"", "custom"} {
			for _, key := range []string{"downstream-key", "experimental-downstream-key"} {
				t.Run(name+"/"+toolType+"/"+key, func(t *testing.T) {
					experimental := key == "experimental-downstream-key"
					wantName := name
					if experimental {
						wantName = mcpToolNamePrefix + name
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						root := decodeBody(t, body)
						declaration := root["tools"].([]any)[0].(map[string]any)
						choice := root["tool_choice"].(map[string]any)
						message := root["messages"].([]any)[0].(map[string]any)
						use := message["content"].([]any)[0].(map[string]any)
						if declaration["name"] != wantName || choice["name"] != wantName || use["name"] != wantName {
							t.Errorf("declaration/references do not agree: %s", body)
						}
						if toolType == "custom" && declaration["type"] != "custom" {
							t.Errorf("custom tool type changed: %v", declaration)
						}
						w.Header().Set("Content-Type", "application/json")
						if declaration["name"] == name {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, extraUsageErrorBody(observedExtraUsageMessage))
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{
							"content": []any{map[string]any{"type": "tool_use", "id": "toolu_2", "name": wantName, "input": map[string]any{}}},
							"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
						})
					}))
					defer upstream.Close()
					server := newTestServer(t, upstream.URL, 4096)
					declaration := map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}}
					if toolType != "" {
						declaration["type"] = toolType
					}
					body, err := json.Marshal(map[string]any{
						"model": "claude-test", "tools": []any{declaration},
						"tool_choice": map[string]any{"type": "tool", "name": name},
						"messages": []any{map[string]any{"role": "assistant", "content": []any{
							map[string]any{"type": "tool_use", "id": "toolu_1", "name": name, "input": map[string]any{}},
						}}},
					})
					if err != nil {
						t.Fatal(err)
					}
					request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
					request.Header.Set("x-api-key", key)
					recorder := httptest.NewRecorder()
					server.routes().ServeHTTP(recorder, request)
					if !experimental {
						if recorder.Code != http.StatusBadRequest || recorder.Body.String() != extraUsageErrorBody(observedExtraUsageMessage) {
							t.Fatalf("compatible response changed: status=%d body=%s", recorder.Code, recorder.Body.String())
						}
						return
					}
					if recorder.Code != http.StatusOK {
						t.Fatalf("experimental request rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					response := decodeBody(t, recorder.Body.Bytes())
					use := response["content"].([]any)[0].(map[string]any)
					if use["name"] != name {
						t.Fatalf("original response tool name not restored: %v", use)
					}
				})
			}
		}
	}
}
