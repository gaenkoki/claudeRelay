package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/local/claude-relay/internal/store"
)

const accountConcurrencySetting = "max_inflight_per_account"

// All selectors share this limit so an admin update never races with admission.
// Existing reservations remain valid when the limit is lowered.
type accountConcurrencyPolicy struct {
	mu    sync.Mutex // Serialize persistence before publishing a new limit.
	limit atomic.Int64
}

func loadAccountConcurrencyPolicy(database *store.Store, configured int) (*accountConcurrencyPolicy, error) {
	value, found, err := database.RuntimeSetting(context.Background(), accountConcurrencySetting)
	if err != nil {
		return nil, err
	}
	if found {
		configured, err = strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("decode persisted Messages concurrency limit: %w", err)
		}
	}
	if configured < 1 {
		return nil, fmt.Errorf("Messages concurrency limit must be a positive integer")
	}
	policy := &accountConcurrencyPolicy{}
	policy.limit.Store(int64(configured))
	return policy, nil
}

func (p *accountConcurrencyPolicy) current() int {
	return int(p.limit.Load())
}

func (p *accountConcurrencyPolicy) persist(ctx context.Context, database *store.Store, limit int) error {
	if limit < 1 {
		return fmt.Errorf("Messages concurrency limit must be a positive integer")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := database.SetRuntimeSetting(ctx, accountConcurrencySetting, strconv.Itoa(limit)); err != nil {
		return err
	}
	p.limit.Store(int64(limit))
	return nil
}

func (s *Server) setAccountConcurrency(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Limit int `json:"max_inflight_per_account"`
	}
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if request.Limit < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "max_inflight_per_account must be a positive integer")
		return
	}
	if err := s.selector.concurrency.persist(r.Context(), s.store, request.Limit); err != nil {
		slog.Error("persist Messages concurrency limit", "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist Messages concurrency limit")
		return
	}
	slog.Info("Messages concurrency limit changed", "max_inflight_per_account", request.Limit, "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"max_inflight_per_account": request.Limit})
}
