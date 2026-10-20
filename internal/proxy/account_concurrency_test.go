package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAccountConcurrencyLiveAdmissionAndPersistence(t *testing.T) {
	for _, policy := range []string{routingPolicyLegacy, routingPolicyQuotaAware} {
		t.Run(policy, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 4096)
			if _, err := server.routingPolicy.set(policy); err != nil {
				t.Fatal(err)
			}
			accounts, err := server.store.AllAccounts(t.Context())
			if err != nil || len(accounts) != 1 {
				t.Fatalf("accounts=%v err=%v", accounts, err)
			}
			set := func(limit string) {
				t.Helper()
				w := adminRequest(t, server, http.MethodPost, "/admin/v1/routing/concurrency", `{"max_inflight_per_account":`+limit+`}`)
				if w.Code != 200 {
					t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
				}
			}
			request := func(path string, status int) {
				t.Helper()
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"claude-test","messages":[]}`))
				r.Header.Set("x-api-key", "downstream-key")
				w := httptest.NewRecorder()
				server.routes().ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, status, w.Body.String())
				}
			}
			first := server.load.reserve(accounts[0].ID)
			defer first()
			second := server.load.reserve(accounts[0].ID)
			defer second()
			set("1")
			if current := server.load.snapshot()[accounts[0].ID]; current != 2 {
				t.Fatalf("lowering the limit released running requests: %d", current)
			}
			request("/v1/messages", 429)
			if calls.Load() != 0 {
				t.Fatal("blocked Messages reached upstream")
			}
			request("/v1/messages/count_tokens", 200)
			first()
			request("/v1/messages", 429)
			second()
			request("/v1/messages", 200)
			if calls.Load() != 2 {
				t.Fatalf("upstream calls=%d want=2", calls.Load())
			}
			// Raising the limit admits another request while an existing slot is occupied.
			held := server.load.reserve(accounts[0].ID)
			defer held()
			set("2")
			request("/v1/messages", 200)
			overview := adminRequest(t, server, http.MethodGet, "/admin/v1/overview", "")
			if overview.Code != 200 || !strings.Contains(overview.Body.String(), `"max_inflight_per_account":2`) {
				t.Fatalf("overview does not expose live limit: %s", overview.Body.String())
			}
			cfg := server.cfg
			cfg.MaxInflightPerAccount = 7
			reloaded, err := NewServer(cfg, server.store)
			if err != nil || reloaded.selector.concurrency.current() != 2 {
				t.Fatalf("saved limit did not override startup config: server=%v err=%v", reloaded, err)
			}
		})
	}
}

func TestAccountConcurrencyRejectsInvalidValuesAndFailedWrites(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 4096)
	initial := server.selector.concurrency.current()
	for _, body := range []string{`{}`, `{"max_inflight_per_account":null}`, `{"max_inflight_per_account":0}`, `{"max_inflight_per_account":-1}`, `{"max_inflight_per_account":1.5}`, `{"max_inflight_per_account":"6"}`} {
		w := adminRequest(t, server, http.MethodPost, "/admin/v1/routing/concurrency", body)
		if w.Code != 400 || server.selector.concurrency.current() != initial {
			t.Fatalf("invalid body changed setting: %s status=%d", body, w.Code)
		}
	}
	if err := server.store.Close(); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(t, server, http.MethodPost, "/admin/v1/routing/concurrency", `{"max_inflight_per_account":7}`)
	if w.Code != 500 || server.selector.concurrency.current() != initial {
		t.Fatalf("failed persistence changed live setting: %d %s", w.Code, w.Body.String())
	}
}

func TestAccountConcurrencyRejectsCorruptPersistedSetting(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 4096)
	for _, value := range []string{"", "garbage", "0", "-1"} {
		if err := server.store.SetRuntimeSetting(t.Context(), accountConcurrencySetting, value); err != nil {
			t.Fatal(err)
		}
		if _, err := NewServer(server.cfg, server.store); err == nil {
			t.Fatalf("accepted corrupt persisted limit %q", value)
		}
	}
}

func TestAccountConcurrencyConcurrentReadsAndSaves(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 4096)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 1000 {
				_, limit := server.selector.loadPolicy(requestRoute{})
				if limit < 1 {
					t.Errorf("invalid concurrent admission limit %d", limit)
				}
			}
		})
	}
	var writers sync.WaitGroup
	for limit := 1; limit <= 7; limit++ {
		writers.Go(func() {
			if err := server.selector.concurrency.persist(t.Context(), server.store, limit); err != nil {
				t.Error(err)
			}
		})
	}
	readers.Wait()
	writers.Wait()
	reloaded, err := loadAccountConcurrencyPolicy(server.store, 8)
	if err != nil || reloaded.current() != server.selector.concurrency.current() {
		t.Fatalf("persisted/live limits diverged: err=%v", err)
	}
}
