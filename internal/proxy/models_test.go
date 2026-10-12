package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func modelsRequest(t *testing.T, server *Server, key, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("x-api-key", key)
	request.Header.Set("anthropic-version", "2023-06-01")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	return recorder
}

type modelListPayload struct {
	Data []struct {
		ID   string `json:"id"`
		Line string `json:"line"`
	} `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
}

func (p modelListPayload) ids() []string {
	ids := make([]string, 0, len(p.Data))
	for _, model := range p.Data {
		ids = append(ids, model.ID)
	}
	return ids
}

func decodeModelList(t *testing.T, recorder *httptest.ResponseRecorder) modelListPayload {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("model list status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload modelListPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode model list: %v (%s)", err, recorder.Body.String())
	}
	return payload
}

// modelsUpstream serves ids newest first in pages of pageSize and counts reads.
func modelsUpstream(t *testing.T, pageSize int, ids ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access-token" {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("upstream anthropic-beta = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("upstream anthropic-version = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("upstream limit = %q", got)
		}
		start := 0
		if after := r.URL.Query().Get("after_id"); after != "" {
			start = slices.Index(ids, after) + 1
		}
		end := min(start+pageSize, len(ids))
		data := make([]map[string]any, 0, end-start)
		for _, id := range ids[start:end] {
			data = append(data, map[string]any{"type": "model", "id": id, "display_name": strings.ToUpper(id), "line": "opus"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data, "has_more": end < len(ids), "first_id": ids[start], "last_id": ids[end-1],
		})
	}))
	t.Cleanup(upstream.Close)
	return upstream, &reads
}

func waitForModelRefresh(t *testing.T, catalog *modelCatalog, access store.AccountAccess) {
	t.Helper()
	catalog.mu.Lock()
	done := catalog.inflight[access]
	catalog.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("model list refresh did not finish")
	}
}

func TestModelListReadsUpstreamPagesOncePerTTL(t *testing.T) {
	t.Parallel()
	upstream, reads := modelsUpstream(t, 2, "claude-c", "claude-b", "claude-a")
	server := newTestServer(t, upstream.URL, 4096)
	for range 2 {
		recorder := modelsRequest(t, server, "downstream-key", "/v1/models?limit=1000")
		payload := decodeModelList(t, recorder)
		if source := recorder.Header().Get(modelsSourceHeader); source != modelsSourceUpstream {
			t.Fatalf("source = %q", source)
		}
		if got := payload.ids(); !slices.Equal(got, []string{"claude-c", "claude-b", "claude-a"}) {
			t.Fatalf("model ids = %#v", got)
		}
		if payload.HasMore || payload.Data[0].Line != "opus" {
			t.Fatalf("payload = %#v; uninterpreted upstream fields must survive", payload)
		}
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("upstream reads = %d, want both pages read once", got)
	}
}

func TestModelEndpointsFailWithoutUpstreamList(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		writeError(w, http.StatusUnauthorized, "authentication_error", "OAuth authentication is currently not supported.")
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	for _, path := range []string{"/v1/models", "/v1/models/claude-opus-5-5"} {
		recorder := modelsRequest(t, server, "downstream-key", path)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "upstream model list is unavailable") {
			t.Fatalf("%s: status = %d, body = %s", path, recorder.Code, recorder.Body.String())
		}
		if source := recorder.Header().Get(modelsSourceHeader); source != modelsSourceUnavailable {
			t.Fatalf("source = %q", source)
		}
		if body := recorder.Body.String(); strings.Contains(body, "OAuth authentication") || strings.Contains(body, "default") {
			t.Fatalf("model list exposed upstream detail: %s", body)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("upstream reads = %d, want the failed read held back", got)
	}
}

func TestModelListInitialTimeoutReturnsErrorThenUsesCompletedRead(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	server.models = newModelCatalog(func(ctx context.Context, _ store.AccountAccess) ([]catalogModel, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []catalogModel{{ID: "claude-loaded", Raw: json.RawMessage(`{"id":"claude-loaded","type":"model"}`)}}, nil
	})
	recorder := modelsRequest(t, server, "downstream-key", "/v1/models")
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get(modelsSourceHeader) != modelsSourceUnavailable {
		t.Fatalf("initial timeout status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	close(release)
	released = true
	waitForModelRefresh(t, server.models, store.AccountAccessCompatibleOnly)
	payload := decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models"))
	if !slices.Equal(payload.ids(), []string{"claude-loaded"}) {
		t.Fatalf("completed read models = %#v", payload.ids())
	}
}

func TestModelListServesStaleListWhileRefreshing(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reads.Add(1) > 1 {
			writeError(w, http.StatusInternalServerError, "api_error", "unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-cached"}],"has_more":false,"first_id":"claude-cached","last_id":"claude-cached"}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	server.models.now = func() time.Time { return time.Unix(0, clock.Load()) }

	if source := modelsRequest(t, server, "downstream-key", "/v1/models").Header().Get(modelsSourceHeader); source != modelsSourceUpstream {
		t.Fatalf("first source = %q", source)
	}
	clock.Add(int64(modelCatalogTTL))
	for attempt := range 2 {
		recorder := modelsRequest(t, server, "downstream-key", "/v1/models")
		payload := decodeModelList(t, recorder)
		if source := recorder.Header().Get(modelsSourceHeader); source != modelsSourceStale {
			t.Fatalf("attempt %d source = %q", attempt, source)
		}
		if got := payload.ids(); !slices.Equal(got, []string{"claude-cached"}) {
			t.Fatalf("attempt %d model ids = %#v", attempt, got)
		}
		waitForModelRefresh(t, server.models, store.AccountAccessCompatibleOnly)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("upstream reads = %d, want one refresh after expiry", got)
	}
}

func TestModelListAppliesCursorPagination(t *testing.T) {
	t.Parallel()
	upstream, _ := modelsUpstream(t, 1000, "m5", "m4", "m3", "m2", "m1")
	server := newTestServer(t, upstream.URL, 4096)
	cases := []struct {
		query   string
		want    []string
		hasMore bool
	}{
		{query: "limit=2", want: []string{"m5", "m4"}, hasMore: true},
		{query: "limit=2&after_id=m4", want: []string{"m3", "m2"}, hasMore: true},
		{query: "after_id=m2", want: []string{"m1"}},
		{query: "limit=2&before_id=m2", want: []string{"m4", "m3"}, hasMore: true},
		{query: "before_id=m4", want: []string{"m5"}},
		{query: "after_id=m1", want: []string{}},
	}
	for _, tc := range cases {
		payload := decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models?"+tc.query))
		if got := payload.ids(); !slices.Equal(got, tc.want) || payload.HasMore != tc.hasMore {
			t.Fatalf("%s: ids = %#v has_more = %v", tc.query, got, payload.HasMore)
		}
		if len(tc.want) == 0 {
			if payload.FirstID != nil || payload.LastID != nil {
				t.Fatalf("%s: empty page cursors = %v %v", tc.query, payload.FirstID, payload.LastID)
			}
			continue
		}
		if payload.FirstID == nil || *payload.FirstID != tc.want[0] || payload.LastID == nil || *payload.LastID != tc.want[len(tc.want)-1] {
			t.Fatalf("%s: cursors = %v %v", tc.query, payload.FirstID, payload.LastID)
		}
	}
	for _, query := range []string{"limit=0", "limit=1001", "limit=two", "after_id=m4&before_id=m2", "after_id=missing"} {
		recorder := modelsRequest(t, server, "downstream-key", "/v1/models?"+query)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request_error") {
			t.Fatalf("%s: status = %d, body = %s", query, recorder.Code, recorder.Body.String())
		}
	}
}

func TestModelRetrieveUsesSharedList(t *testing.T) {
	t.Parallel()
	upstream, reads := modelsUpstream(t, 1000, "claude-b", "claude-a")
	server := newTestServer(t, upstream.URL, 4096)
	found := modelsRequest(t, server, "downstream-key", "/v1/models/claude-a")
	var model struct {
		ID   string `json:"id"`
		Line string `json:"line"`
	}
	if found.Code != http.StatusOK || json.Unmarshal(found.Body.Bytes(), &model) != nil || model.ID != "claude-a" || model.Line != "opus" {
		t.Fatalf("retrieve status = %d, body = %s", found.Code, found.Body.String())
	}
	missing := modelsRequest(t, server, "downstream-key", "/v1/models/claude-missing")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "not_found_error") {
		t.Fatalf("missing status = %d, body = %s", missing.Code, missing.Body.String())
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("upstream reads = %d", got)
	}
}

func TestModelListKeepsAccountFenceWithoutClaudeCodeShape(t *testing.T) {
	t.Parallel()
	upstream, reads := modelsUpstream(t, 1000, "claude-a")
	server := newTestServer(t, upstream.URL, 4096)
	if _, err := server.store.SetAccountPool(t.Context(), "default", store.AccountPoolOfficial); err != nil {
		t.Fatal(err)
	}
	compatible := modelsRequest(t, server, "downstream-key", "/v1/models")
	if compatible.Code != http.StatusServiceUnavailable {
		t.Fatalf("compatible status = %d, body = %s", compatible.Code, compatible.Body.String())
	}
	if source := compatible.Header().Get(modelsSourceHeader); source != modelsSourceUnavailable || reads.Load() != 0 {
		t.Fatalf("compatible source = %q after %d upstream reads; the official-pool account was used", source, reads.Load())
	}
	// Official traffic must be Claude Code-shaped, but discovery sends no X-App or session header.
	official := modelsRequest(t, server, "official-downstream-key", "/v1/models")
	payload := decodeModelList(t, official)
	if source := official.Header().Get(modelsSourceHeader); source != modelsSourceUpstream || !slices.Equal(payload.ids(), []string{"claude-a"}) {
		t.Fatalf("official source = %q ids = %#v", source, payload.ids())
	}
}

func TestModelEndpointsRequireModelKey(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	for _, key := range []string{"", "wrong-key", "admin-key"} {
		for _, path := range []string{"/v1/models", "/v1/models/claude-opus-5-5"} {
			if recorder := modelsRequest(t, server, key, path); recorder.Code != http.StatusUnauthorized {
				t.Fatalf("key %q %s status = %d", key, path, recorder.Code)
			}
		}
	}
}

func adminRefreshModels(t *testing.T, server *Server) []modelCatalogStatus {
	t.Helper()
	recorder := adminRequest(t, server, http.MethodPost, "/admin/v1/models/refresh", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Policies []modelCatalogStatus `json:"policies"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode refresh: %v (%s)", err, recorder.Body.String())
	}
	return payload.Policies
}

func TestAdminModelRefreshReadsAgainDespiteFreshList(t *testing.T) {
	t.Parallel()
	upstream, reads := modelsUpstream(t, 1000, "claude-a")
	server := newTestServer(t, upstream.URL, 4096)
	decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models"))
	statuses := adminRefreshModels(t, server)
	if len(statuses) != 2 || statuses[0].Access != store.AccountAccessCompatibleOnly || statuses[1].Access != store.AccountAccessAll {
		t.Fatalf("refreshed policies = %#v", statuses)
	}
	for _, status := range statuses {
		if status.Source != modelsSourceUpstream || status.Models != 1 || status.FetchedAt == 0 || status.Refreshing || status.Error != "" {
			t.Fatalf("status = %#v", status)
		}
		if len(status.Items) != 1 || status.Items[0].ID != "claude-a" || status.Items[0].DisplayName != "CLAUDE-A" {
			t.Fatalf("status items = %#v", status.Items)
		}
	}
	if got := reads.Load(); got != 3 {
		t.Fatalf("upstream reads = %d, want the fresh list read again for both policies", got)
	}
	decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models"))
	if got := reads.Load(); got != 3 {
		t.Fatalf("upstream reads after refresh = %d", got)
	}
}

func TestAdminModelRefreshIgnoresFailureDelayAndReportsError(t *testing.T) {
	t.Parallel()
	var failing atomic.Bool
	failing.Store(true)
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		if failing.Load() {
			writeError(w, http.StatusUnauthorized, "authentication_error", "OAuth authentication is currently not supported.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-new"}],"has_more":false}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	if recorder := modelsRequest(t, server, "downstream-key", "/v1/models"); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, status := range adminRefreshModels(t, server) {
		if status.Source != modelsSourceUnavailable || status.Models != 0 || len(status.Items) != 0 ||
			!strings.Contains(status.Error, `account "default"`) || !strings.Contains(status.Error, "OAuth authentication is currently not supported.") {
			t.Fatalf("failed refresh status = %#v", status)
		}
	}
	failing.Store(false)
	for _, status := range adminRefreshModels(t, server) {
		if status.Source != modelsSourceUpstream || status.Models != 1 || status.Error != "" {
			t.Fatalf("recovered refresh status = %#v", status)
		}
	}
	if got := reads.Load(); got != 5 {
		t.Fatalf("upstream reads = %d, want the failure delay bypassed by both refreshes", got)
	}
	payload := decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models"))
	if !slices.Equal(payload.ids(), []string{"claude-new"}) {
		t.Fatalf("model ids = %#v", payload.ids())
	}
}

func TestAdminModelRefreshSkipsOfficialPolicyWithoutOfficialKey(t *testing.T) {
	t.Parallel()
	upstream, reads := modelsUpstream(t, 1000, "claude-a")
	server := newTestServer(t, upstream.URL, 4096)
	server.cfg.OfficialAPIKey = ""
	statuses := adminRefreshModels(t, server)
	if len(statuses) != 1 || statuses[0].Access != store.AccountAccessCompatibleOnly || reads.Load() != 1 {
		t.Fatalf("statuses = %#v after %d upstream reads", statuses, reads.Load())
	}
}

func TestAdminModelRefreshRequiresAdminKey(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, "http://127.0.0.1:1", 4096)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/models/refresh", nil)
	request.Header.Set("x-api-key", "downstream-key")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("relay key refresh status = %d", recorder.Code)
	}
}

func TestAdminModelViewReportsListsWithoutReadingUpstream(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"type":"model","id":"claude-x","display_name":"Claude X","created_at":"2026-10-07T18:00:00Z","lifecycle":"deprecated","max_input_tokens":1000000,"max_tokens":128000}],"has_more":false}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, 4096)
	view := func() []modelCatalogStatus {
		t.Helper()
		recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/models", "")
		var payload struct {
			Policies []modelCatalogStatus `json:"policies"`
		}
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || len(payload.Policies) != 2 {
			t.Fatalf("view status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		return payload.Policies
	}

	for _, status := range view() {
		if status.Source != modelsSourceUnavailable || status.Models != 0 || len(status.Items) != 0 {
			t.Fatalf("initial status = %#v", status)
		}
	}
	decodeModelList(t, modelsRequest(t, server, "downstream-key", "/v1/models"))
	statuses := view()
	want := modelListItem{ID: "claude-x", DisplayName: "Claude X", CreatedAt: "2026-10-07T18:00:00Z",
		Lifecycle: "deprecated", MaxInputTokens: 1000000, MaxTokens: 128000}
	if statuses[0].Source != modelsSourceUpstream || len(statuses[0].Items) != 1 || statuses[0].Items[0] != want {
		t.Fatalf("compatible status = %#v", statuses[0])
	}
	if statuses[1].Source != modelsSourceUnavailable {
		t.Fatalf("unrequested official status = %#v", statuses[1])
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("upstream reads = %d, want only the model-list request to read upstream", got)
	}
}
