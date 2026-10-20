package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCountTokensUsesIndependentInflightPool(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	server.selector.concurrency.limit.Store(1)
	server.selector.maxCountTokensInflightPerAcct = 1
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}

	messageRoute, err := deriveRequestRoute(
		[]byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`),
		http.Header{}, compatibleIngress, "/v1/messages",
	)
	if err != nil {
		t.Fatal(err)
	}
	countRoute, err := deriveRequestRoute(
		[]byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`),
		http.Header{}, compatibleIngress, "/v1/messages/count_tokens",
	)
	if err != nil {
		t.Fatal(err)
	}

	releaseMessage := server.load.reserve(account.ID)
	countSelection, err := server.selector.selectAccount(t.Context(), countRoute, "", map[int64]bool{})
	if err != nil {
		t.Fatalf("count_tokens was blocked by a Messages slot: %v", err)
	}
	countSelection.release()
	countSelection.releaseSession()
	releaseMessage()

	releaseCount := server.countTokensLoad.reserve(account.ID)
	messageSelection, err := server.selector.selectAccount(t.Context(), messageRoute, "", map[int64]bool{})
	if err != nil {
		t.Fatalf("Messages was blocked by a count_tokens slot: %v", err)
	}
	messageSelection.release()
	messageSelection.releaseSession()

	_, err = server.selector.selectAccount(t.Context(), countRoute, "", map[int64]bool{})
	var rateLimit localRateLimitError
	if !errors.As(err, &rateLimit) {
		t.Fatalf("count_tokens limit error = %v, want local rate limit", err)
	}
	releaseCount()
}

func TestCountTokensDoesNotCreateSessionBinding(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":12}`))
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	body := `{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	request.Header.Set("x-api-key", "downstream-key")
	request.Header.Set("X-Claude-Session-Id", "count-only-session")
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	route, err := deriveRequestRoute([]byte(body), request.Header, compatibleIngress, request.URL.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := server.store.BoundAccount(t.Context(), route.ConversationKey, route.AccountAccess, time.Now()); err != nil || found {
		t.Fatalf("count_tokens binding found=%v err=%v", found, err)
	}
}

func TestCountTokensDoesNotConsumeActiveSessionAdmission(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("default account = %#v found=%v err=%v", account, found, err)
	}
	tracker := newSessionAdmissionTracker(server.store, 1)
	server.selector.sessions = tracker
	releaseSession, admitted, err := tracker.reserve(t.Context(), "session:occupied", account.ID, false)
	if err != nil || !admitted {
		t.Fatalf("occupy session slot admitted=%v err=%v", admitted, err)
	}
	defer releaseSession()

	headers := http.Header{"X-Claude-Session-Id": []string{"count-session"}}
	countRoute, err := deriveRequestRoute(
		[]byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`),
		headers, compatibleIngress, "/v1/messages/count_tokens",
	)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := server.selector.selectAccount(t.Context(), countRoute, "", map[int64]bool{})
	if err != nil {
		t.Fatalf("count_tokens was blocked by active-session admission: %v", err)
	}
	selected.release()
	selected.releaseSession()

	tracker.mu.Lock()
	pending, perAccount := len(tracker.pending), tracker.perAccount[account.ID]
	tracker.mu.Unlock()
	if pending != 1 || perAccount != 1 {
		t.Fatalf("count_tokens changed session admissions: pending=%d per_account=%d", pending, perAccount)
	}
}
