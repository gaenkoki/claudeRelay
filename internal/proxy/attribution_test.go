package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/claude-relay/internal/credential"
)

func TestExperimentalAttributionAlignsOnlyGeneratedFields(t *testing.T) {
	t.Parallel()
	const originalSession = "client-session"
	const cliUA = "claude-cli/2.1.295 (external, claude-desktop)"
	const plainBody = `{"model":"claude-test","system":"keep this instruction","messages":[{"role":"user","content":"hello"}]}`
	const opaqueBody = `{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.219.abc; cc_entrypoint=cli; cch=abcde; custom_field=keep;"}],"metadata":{"user_id":"business-user","custom":"keep"},"messages":[]}`
	const identityBody = `{"model":"claude-test","system":"keep this instruction","metadata":{"user_id":"{\"device_id\":\"caller-device\",\"account_uuid\":\"caller-account\",\"session_id\":\"caller-session\"}"},"messages":[]}`
	for _, tc := range []struct {
		name        string
		key         string
		path        string
		body        string
		ua          string
		wantVersion string
		wantAligned bool
	}{
		{"experimental", "experimental-downstream-key", "/v1/messages", plainBody, cliUA, "2.1.295", true},
		{"compatible unchanged", "downstream-key", "/v1/messages", plainBody, cliUA, defaultClaudeCodeVersion, false},
		{"ordinary UA preserved", "experimental-downstream-key", "/v1/messages", plainBody, "third-party-sdk/1.0", defaultClaudeCodeVersion, true},
		{"malformed CLI release not inferred", "experimental-downstream-key", "/v1/messages", plainBody, "claude-cli/2.1.295unknown", defaultClaudeCodeVersion, true},
		{"caller billing and business identity preserved", "experimental-downstream-key", "/v1/messages", opaqueBody, cliUA, "2.1.219.abc", false},
		{"caller structured identity preserved", "experimental-downstream-key", "/v1/messages", identityBody, cliUA, "2.1.295", false},
		{"count tokens has no metadata", "experimental-downstream-key", "/v1/messages/count_tokens", plainBody, cliUA, "2.1.295", false},
		{"official count tokens unchanged", "official-downstream-key", "/v1/messages/count_tokens", plainBody, cliUA, defaultClaudeCodeVersion, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sessions []string
			var sentBodies [][]byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				sentBodies = append(sentBodies, body)
				sessions = append(sessions, r.Header.Get(claudeCodeSessionHeader))
				if r.Header.Get("User-Agent") != tc.ua {
					t.Errorf("User-Agent changed: %q", r.Header.Get("User-Agent"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			for range 2 {
				request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				if tc.key == "official-downstream-key" {
					setOfficialClaudeCodeTestHeaders(request)
				}
				request.Header.Set("x-api-key", tc.key)
				request.Header.Set("User-Agent", tc.ua)
				request.Header.Set(claudeCodeSessionHeader, originalSession)
				recorder := httptest.NewRecorder()
				server.routes().ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if request.Header.Get(claudeCodeSessionHeader) != originalSession {
					t.Fatal("outgoing attribution mutated caller-owned routing headers")
				}
			}
			if len(sentBodies) != 2 {
				t.Fatalf("upstream requests=%d", len(sentBodies))
			}
			first := decodeBody(t, sentBodies[0])
			billing := first["system"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(billing, "cc_version="+tc.wantVersion+";") {
				t.Fatalf("unexpected billing version: %q", billing)
			}
			if tc.body == opaqueBody {
				if string(sentBodies[0]) != tc.body {
					t.Fatalf("caller CCH/billing/identity changed: %s", sentBodies[0])
				}
			} else if strings.Contains(billing, "cch=") || !strings.Contains(billing, "cc_entrypoint=claude-desktop-3p;") {
				t.Fatalf("generated attribution invented CCH or changed entrypoint: %q", billing)
			}
			if tc.wantAligned {
				identity := decodeUserID(t, first)
				if identity.SessionID == originalSession || sessions[0] != identity.SessionID || sessions[1] != sessions[0] {
					t.Fatalf("generated session is inconsistent or unstable: headers=%v metadata=%+v", sessions, identity)
				}
			} else if sessions[0] != originalSession || sessions[1] != originalSession {
				t.Fatalf("caller session header changed: %v", sessions)
			}
			if tc.path == "/v1/messages/count_tokens" {
				if _, exists := first["metadata"]; exists {
					t.Fatal("count_tokens gained unsupported metadata")
				}
			}
			if tc.body == identityBody && decodeUserID(t, first).DeviceID != "caller-device" {
				t.Fatal("caller-provided identity was replaced")
			}
		})
	}
}

func TestExperimentalAttributionPreservesCallerCCHWhenAddingMetadata(t *testing.T) {
	t.Parallel()
	const billing = "x-anthropic-billing-header: cc_version=2.1.219.abc; cc_entrypoint=cli; cch=abcde; custom_field=keep;"
	cred := credential.Credential{DeviceID: strings.Repeat("a", 64), AccountUUID: "11111111-1111-4111-8111-111111111111"}
	for _, headers := range []http.Header{
		{},
		{"X-Session-Id": []string{"caller-session"}},
	} {
		headers.Set("User-Agent", "claude-cli/2.1.295 (external, cli)")
		body := []byte(`{"system":[{"type":"text","text":"` + billing + `"}],"messages":[]}`)
		attributed, err := addSubscriptionAttribution(body, headers, cred, attributionOptions{IncludeMetadata: true, AlignGeneratedFields: true})
		if err != nil {
			t.Fatal(err)
		}
		root := decodeBody(t, attributed.Body)
		if root["system"].([]any)[0].(map[string]any)["text"] != billing {
			t.Fatalf("caller billing or CCH changed: %s", attributed.Body)
		}
		if attributed.SessionHeader != "" {
			t.Fatal("invented a Claude Code session header for a caller without one")
		}
		if decodeUserID(t, root).SessionID == "" {
			t.Fatal("metadata was not generated")
		}
	}
}

func TestExperimentalAttributionKeepsOriginalSessionSeedAcrossFailover(t *testing.T) {
	t.Parallel()
	const originalSession = "client-session"
	const secondAccountUUID = "22222222-2222-4222-8222-222222222222"
	var sentIdentities []attributionUserID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		identity := decodeUserID(t, decodeBody(t, body))
		sentIdentities = append(sentIdentities, identity)
		device, uuid := strings.Repeat("a", 64), "11111111-1111-4111-8111-111111111111"
		if r.Header.Get("Authorization") == "Bearer second-token" {
			device, uuid = strings.Repeat("b", 64), secondAccountUUID
		}
		if identity.DeviceID != device || identity.AccountUUID != uuid || identity.SessionID != deterministicUUID(device, originalSession) || r.Header.Get(claudeCodeSessionHeader) != identity.SessionID {
			t.Errorf("failover reused or rehashed a transformed identity: %+v", identity)
		}
		if len(sentIdentities) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	importTestAccount(t, server.store, "second", "second-token", secondAccountUUID, "b")
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("x-api-key", "experimental-downstream-key")
	request.Header.Set(claudeCodeSessionHeader, originalSession)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || len(sentIdentities) != 2 || sentIdentities[0].AccountUUID == sentIdentities[1].AccountUUID {
		t.Fatalf("status=%d identities=%+v", recorder.Code, sentIdentities)
	}
	if request.Header.Get(claudeCodeSessionHeader) != originalSession {
		t.Fatal("failover mutated the caller session")
	}
}
