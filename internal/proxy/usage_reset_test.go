package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const resetTestOrg = "22222222-2222-4222-8222-222222222222"
const resetTestSubject = "33333333-3333-4333-8333-333333333333"

type resetTestProvider struct {
	posts         atomic.Int64
	mode          atomic.Int32
	badProfile    atomic.Bool
	badStatus     atomic.Bool
	redirects     atomic.Int64
	lastRequestID atomic.Value
}

func (p *resetTestProvider) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/oauth/profile":
		if p.badProfile.Load() {
			io.WriteString(w, `{"organization":{"uuid":"not-an-org"}}`)
			return
		}
		fmt.Fprintf(w, `{"account":{"uuid":%q},"organization":{"uuid":%q}}`, resetTestSubject, resetTestOrg)
	case "/api/oauth/usage":
		if r.URL.RawQuery != "" {
			if r.URL.Query().Get("cedar_ember") != "1" || r.URL.Query().Get("skip_spend") != "1" {
				w.WriteHeader(400)
				return
			}
			if p.badStatus.Load() {
				io.WriteString(w, `{"cedar_ember":{"eligible":true,"grants":[{"id":"launch"}]}}`)
				return
			}
			io.WriteString(w, `{"cedar_ember":{"eligible":true,"at_limit":true,"grants":[{"id":"launch","label":"Reset test","resets_total":2,"resets_left":2,"clears":["five_hour"],"paused":false,"usable_now":true,"use_requires_limit":false}]}}`)
		} else {
			fmt.Fprintf(w, `{"five_hour":{"utilization":0,"resets_at":%q}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		}
	case "/api/organizations/" + resetTestOrg + "/reset_rate_limits":
		p.posts.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["program"] != "cedar_ember" || body["grant_id"] != "launch" || body["request_id"] == "" {
			w.WriteHeader(400)
			return
		}
		p.lastRequestID.Store(body["request_id"])
		switch p.mode.Load() {
		case 1:
			w.WriteHeader(503)
			io.WriteString(w, `{"error":"secret-upstream-token"}`)
		case 2:
			io.WriteString(w, `{"result":"already_used"}`)
		case 3:
			w.Header().Set("Location", "/redirect-must-not-be-followed")
			w.WriteHeader(307)
		case 4:
			w.WriteHeader(429)
		default:
			io.WriteString(w, `{"result":"reset"}`)
		}
	case "/redirect-must-not-be-followed":
		p.redirects.Add(1)
		io.WriteString(w, `{"result":"reset"}`)
	default:
		w.WriteHeader(404)
	}
}

func newResetTestServer(t *testing.T) (*Server, *resetTestProvider) {
	t.Helper()
	provider := &resetTestProvider{}
	upstream := httptest.NewServer(http.HandlerFunc(provider.serve))
	t.Cleanup(upstream.Close)
	return newTestServer(t, upstream.URL, 4096), provider
}

func postReset(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	return adminRequest(t, s, "POST", "/admin/v1/accounts/default/usage-resets", fmt.Sprintf(`{"grant_id":"launch","request_id":%q,"confirmed":true}`, id))
}

func TestUsageResetReadClaimAndIdempotency(t *testing.T) {
	s, p := newResetTestServer(t)
	get := adminRequest(t, s, "GET", "/admin/v1/accounts/default/usage-resets", "")
	if get.Code != 200 || p.posts.Load() != 0 || !strings.Contains(get.Body.String(), `"resets_left":2`) {
		t.Fatalf("read: %d %s", get.Code, get.Body.String())
	}
	accounts, _ := s.store.AllAccounts(t.Context())
	a := accounts[0]
	for _, reason := range []string{cooldownReasonFiveHourExhausted, cooldownReasonSevenDayExhausted, cooldownReasonAmbiguous429} {
		if err := s.store.Cooldown(t.Context(), a.ID, reason, time.Now().Add(time.Hour), reason); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		w := postReset(t, s, "first-operation")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"reset"`) {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
	}
	if p.posts.Load() != 1 {
		t.Fatalf("duplicate spending calls=%d", p.posts.Load())
	}
	op, found, err := s.store.UsageResetOperation(t.Context(), "first-operation")
	if err != nil || !found || op.State != "settled" || op.OrganizationUUID != resetTestOrg || op.SubjectUUID != resetTestSubject {
		t.Fatalf("operation=%+v err=%v", op, err)
	}
	cooldowns, _ := s.store.ActiveCooldowns(t.Context(), time.Now())
	if len(cooldowns) != 2 {
		t.Fatalf("unrelated cooldowns changed: %+v", cooldowns)
	}
	for _, c := range cooldowns {
		if c.Reason == cooldownReasonFiveHourExhausted {
			t.Fatal("covered quota cooldown survived reset")
		}
	}
	if reading, ok := s.sampler.current(a, time.Now()); !ok || reading.usedPercent != 0 {
		t.Fatalf("stale live quota: %+v %v", reading, ok)
	}
	if strings.Contains(get.Body.String(), resetTestOrg) || strings.Contains(get.Body.String(), "upstream-access-token") {
		t.Fatal("identity or credentials exposed")
	}
}

func TestUsageResetUnknownSurvivesRestartAndBlocksOtherClaims(t *testing.T) {
	s, p := newResetTestServer(t)
	p.mode.Store(1)
	w := postReset(t, s, "uncertain-operation")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unknown"`) || strings.Contains(w.Body.String(), "secret-upstream-token") {
		t.Fatalf("unknown outcome: %d %s", w.Code, w.Body.String())
	}
	restarted, err := NewServer(s.cfg, s.store)
	if err != nil {
		t.Fatal(err)
	}
	p.badStatus.Store(true)
	read := adminRequest(t, restarted, "GET", "/admin/v1/accounts/default/usage-resets", "")
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"status_error"`) || !strings.Contains(read.Body.String(), `"state":"unknown"`) {
		t.Fatalf("unavailable grant status hid durable receipt: %d %s", read.Code, read.Body.String())
	}
	p.badStatus.Store(false)
	w = postReset(t, restarted, "different-operation")
	if w.Code != 409 || p.posts.Load() != 1 {
		t.Fatalf("new claim allowed during ambiguity: %d %s", w.Code, w.Body.String())
	}
	// A 429 on retry cannot establish that the earlier POST spent nothing.
	p.mode.Store(4)
	w = postReset(t, restarted, "uncertain-operation")
	if !strings.Contains(w.Body.String(), `"state":"unknown"`) {
		t.Fatalf("retry refusal erased ambiguity: %s", w.Body.String())
	}
	p.mode.Store(2)
	w = postReset(t, restarted, "uncertain-operation")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"result":"already_used"`) || !strings.Contains(w.Body.String(), `"state":"settled"`) {
		t.Fatalf("same-ID recovery: %d %s", w.Code, w.Body.String())
	}
	if p.posts.Load() != 3 {
		t.Fatalf("unexpected upstream calls: %d", p.posts.Load())
	}
	if p.lastRequestID.Load() != "uncertain-operation" {
		t.Fatal("retry generated a different upstream operation ID")
	}
}

func TestUsageResetExpiredAmbiguityRequiresManualAcknowledgement(t *testing.T) {
	s, p := newResetTestServer(t)
	op := store.UsageResetOperation{RequestID: "expired-operation", OrganizationUUID: resetTestOrg, SubjectUUID: resetTestSubject,
		AccountID: 1, AccountUUID: "11111111-1111-4111-8111-111111111111", GrantID: "launch", Clears: []string{"five_hour"}, CreatedAt: time.Now().Add(-11 * time.Minute).UnixMilli()}
	if err := s.store.BeginUsageReset(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if w := postReset(t, s, op.RequestID); w.Code != 409 || p.posts.Load() != 0 {
		t.Fatalf("expired retry dispatched: %d", w.Code)
	}
	w := adminRequest(t, s, "POST", "/admin/v1/accounts/default/usage-resets", `{"grant_id":"launch","request_id":"expired-operation","confirmed":true,"resolve_only":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "manually_resolved") || p.posts.Load() != 0 {
		t.Fatalf("manual resolution: %d %s", w.Code, w.Body.String())
	}
	if w := postReset(t, s, "after-verification"); w.Code != 200 || p.posts.Load() != 1 {
		t.Fatalf("verified account stayed locked: %d", w.Code)
	}
}

func TestUsageResetFailsClosedBeforeSpending(t *testing.T) {
	s, p := newResetTestServer(t)
	for _, body := range []string{`{}`, `{"grant_id":"launch","request_id":"test","confirmed":false}`, `{"grant_id":"../../other","request_id":"test","confirmed":true}`} {
		w := adminRequest(t, s, "POST", "/admin/v1/accounts/default/usage-resets", body)
		if w.Code != 400 {
			t.Fatalf("invalid request: %d", w.Code)
		}
	}
	p.badProfile.Store(true)
	if w := postReset(t, s, "bad-profile"); w.Code != 502 {
		t.Fatalf("bad profile accepted: %d", w.Code)
	}
	p.badProfile.Store(false)
	p.badStatus.Store(true)
	if w := postReset(t, s, "bad-status"); w.Code != 502 {
		t.Fatalf("bad grant accepted: %d", w.Code)
	}
	if p.posts.Load() != 0 {
		t.Fatal("invalid request spent a card")
	}
	var grants struct {
		Status usageResetStatus `json:"cedar_ember"`
	}
	json.Unmarshal([]byte(`{"cedar_ember":{"eligible":true,"at_limit":false,"grants":[{"id":"launch","resets_total":1,"resets_left":1,"usable_now":true}]}}`), &grants)
	if resetGrantBlocker(grants.Status, grants.Status.Grants[0], time.Now()) == "" {
		t.Fatal("missing spending condition admitted")
	}
}

func TestUsageResetDoesNotFollowSpendingRedirect(t *testing.T) {
	s, p := newResetTestServer(t)
	p.mode.Store(3)
	w := postReset(t, s, "redirect-operation")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unknown"`) || p.posts.Load() != 1 || p.redirects.Load() != 0 {
		t.Fatalf("redirect: %d %s", w.Code, w.Body.String())
	}
}

type resetLedgerFailureTransport struct {
	base     http.RoundTripper
	database *store.Store
}

func (t resetLedgerFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if r.URL.Query().Get("cedar_ember") == "1" {
		// Simulate storage becoming unavailable after preflight, before the
		// required durable spending record can be inserted.
		t.database.Close()
	}
	return response, err
}

func TestUsageResetDoesNotSpendWhenLedgerWriteFails(t *testing.T) {
	s, p := newResetTestServer(t)
	s.client.Transport = resetLedgerFailureTransport{base: s.client.Transport, database: s.store}
	w := postReset(t, s, "storage-failure")
	if w.Code != 500 || p.posts.Load() != 0 || !strings.Contains(w.Body.String(), "未提交兑换") {
		t.Fatalf("spending without durable record: %d %s calls=%d", w.Code, w.Body.String(), p.posts.Load())
	}
}
