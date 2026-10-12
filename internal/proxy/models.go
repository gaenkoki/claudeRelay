package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	modelCatalogTTL = time.Hour
	// A refused or failed read is not repeated for this long, so an upstream that
	// rejects the read is not asked again by every model-list request.
	modelCatalogRetryDelay = 5 * time.Minute
	// Claude Code abandons gateway model discovery after three seconds by default.
	modelCatalogWait      = 2 * time.Second
	modelCatalogTimeout   = 20 * time.Second
	modelCatalogMaxBody   = 4 << 20
	modelCatalogMaxPages  = 10
	modelCatalogAccounts  = 2
	modelListDefaultLimit = 20
	modelListMaxLimit     = 1000
	modelsSourceHeader    = "X-Claude-Relay-Models-Source"
)

const (
	modelsSourceUpstream = "upstream"
	modelsSourceStale    = "stale"
	modelsSourceBuiltin  = "builtin"
)

var errNoModelAccount = errors.New("no healthy account may read the model list")

// catalogModel keeps the upstream object verbatim, so fields this build does not
// interpret, such as capabilities or lifecycle, reach callers unchanged.
type catalogModel struct {
	ID  string
	Raw json.RawMessage
}

type modelCatalogEntry struct {
	models    []catalogModel
	fetchedAt time.Time
}

// modelCatalog shares one upstream model list per account access policy, because
// the policy decides which accounts may be asked and therefore what is visible.
type modelCatalog struct {
	fetch func(context.Context, store.AccountAccess) ([]catalogModel, error)
	now   func() time.Time

	mu       sync.Mutex
	entries  map[store.AccountAccess]modelCatalogEntry
	failedAt map[store.AccountAccess]time.Time
	// lastErr is the most recent read failure, kept until a later read succeeds.
	lastErr  map[store.AccountAccess]error
	inflight map[store.AccountAccess]chan struct{}
}

func newModelCatalog(fetch func(context.Context, store.AccountAccess) ([]catalogModel, error)) *modelCatalog {
	return &modelCatalog{
		fetch: fetch, now: time.Now,
		entries:  make(map[store.AccountAccess]modelCatalogEntry),
		failedAt: make(map[store.AccountAccess]time.Time),
		lastErr:  make(map[store.AccountAccess]error),
		inflight: make(map[store.AccountAccess]chan struct{}),
	}
}

// list returns the newest known list for a policy and where it came from. An
// expired list is answered at once while one shared refresh runs; without any
// list the caller waits briefly for that refresh before using the built-in list.
func (c *modelCatalog) list(ctx context.Context, access store.AccountAccess) ([]catalogModel, string) {
	c.mu.Lock()
	entry, cached := c.entries[access]
	if cached && c.now().Sub(entry.fetchedAt) < modelCatalogTTL {
		c.mu.Unlock()
		return entry.models, modelsSourceUpstream
	}
	done := c.refreshLocked(access)
	c.mu.Unlock()
	if cached {
		return entry.models, modelsSourceStale
	}
	if done != nil {
		timer := time.NewTimer(modelCatalogWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	entry, cached = c.entries[access]
	c.mu.Unlock()
	if cached {
		return entry.models, modelsSourceUpstream
	}
	return builtinModels, modelsSourceBuiltin
}

// refreshLocked starts the policy's refresh unless one is running or the last
// failure is recent, and returns the channel closed when the running one ends.
func (c *modelCatalog) refreshLocked(access store.AccountAccess) <-chan struct{} {
	if done, running := c.inflight[access]; running {
		return done
	}
	if failed, ok := c.failedAt[access]; ok && c.now().Sub(failed) < modelCatalogRetryDelay {
		return nil
	}
	done := make(chan struct{})
	c.inflight[access] = done
	go func() {
		// Detached from the triggering request: a client that gives up must not
		// cancel a read that later callers are waiting for.
		ctx, cancel := context.WithTimeout(context.Background(), modelCatalogTimeout)
		defer cancel()
		models, err := c.fetch(ctx, access)
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case err == nil:
			c.entries[access] = modelCatalogEntry{models: models, fetchedAt: c.now()}
			delete(c.failedAt, access)
			delete(c.lastErr, access)
		case errors.Is(err, errNoModelAccount):
			// Nothing reached upstream, so the next request may use a newly enabled account.
			c.lastErr[access] = err
		default:
			c.failedAt[access] = c.now()
			c.lastErr[access] = err
		}
		delete(c.inflight, access)
		close(done)
	}()
	return done
}

// forceRefresh starts a read even when the list is fresh or a recent read failed.
// A read that is already running is shared, since its result is equally new.
func (c *modelCatalog) forceRefresh(access store.AccountAccess) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.failedAt, access)
	return c.refreshLocked(access)
}

type modelCatalogStatus struct {
	Access     store.AccountAccess `json:"access"`
	Source     string              `json:"source"`
	Models     int                 `json:"models"`
	Items      []modelListItem     `json:"items"`
	FetchedAt  int64               `json:"fetched_at,omitempty"`
	Refreshing bool                `json:"refreshing"`
	Error      string              `json:"error,omitempty"`
}

// modelListItem is the part of a model object the console displays.
type modelListItem struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	Lifecycle      string `json:"lifecycle,omitempty"`
	MaxInputTokens int64  `json:"max_input_tokens,omitempty"`
	MaxTokens      int64  `json:"max_tokens,omitempty"`
}

// status reports what list would serve for a policy without starting a read.
func (c *modelCatalog) status(access store.AccountAccess) modelCatalogStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := modelCatalogStatus{Access: access, Source: modelsSourceBuiltin}
	models := builtinModels
	if entry, cached := c.entries[access]; cached {
		status.Source = modelsSourceUpstream
		if c.now().Sub(entry.fetchedAt) >= modelCatalogTTL {
			status.Source = modelsSourceStale
		}
		status.FetchedAt = entry.fetchedAt.UnixMilli()
		models = entry.models
	}
	status.Models = len(models)
	status.Items = make([]modelListItem, 0, len(models))
	for _, model := range models {
		var item modelListItem
		// Every displayed field is optional, so a field of an unexpected type
		// only leaves that column empty.
		_ = json.Unmarshal(model.Raw, &item)
		item.ID = model.ID
		status.Items = append(status.Items, item)
	}
	_, status.Refreshing = c.inflight[access]
	if err := c.lastErr[access]; err != nil {
		status.Error = err.Error()
	}
	return status
}

// fetchModelCatalog reads the list with the first healthy accounts the policy may
// use. Like a usage read it is account traffic: the account's exit and OAuth
// refresh apply, but it takes no admission slot and creates no request record.
func (s *Server) fetchModelCatalog(ctx context.Context, access store.AccountAccess) ([]catalogModel, error) {
	accounts, err := s.store.Accounts(ctx, access, "", time.Now())
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errNoModelAccount
	}
	var lastErr error
	for _, account := range accounts[:min(len(accounts), modelCatalogAccounts)] {
		started := time.Now()
		models, err := s.readUpstreamModels(ctx, account)
		if err == nil {
			slog.Info("refreshed model list", "account", account.Alias, "access", access,
				"models", len(models), "duration_ms", time.Since(started).Milliseconds())
			return models, nil
		}
		slog.Warn("read upstream model list", "account", account.Alias, "access", access, "error", err)
		lastErr = fmt.Errorf("account %q: %w", account.Alias, err)
	}
	return nil, lastErr
}

func (s *Server) readUpstreamModels(ctx context.Context, account store.Account) ([]catalogModel, error) {
	account, err := s.tokens.ensureFresh(ctx, account)
	if err != nil {
		return nil, err
	}
	var models []catalogModel
	seen := make(map[string]bool)
	afterID := ""
	for page := 0; page < modelCatalogMaxPages; page++ {
		target := *s.upstream
		target.Path = strings.TrimRight(s.upstream.Path, "/") + "/v1/models"
		query := url.Values{"limit": {strconv.Itoa(modelListMaxLimit)}}
		if afterID != "" {
			query.Set("after_id", afterID)
		}
		target.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(withAccountExit(ctx, account), http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+account.AccessToken)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("anthropic-version", "2023-06-01")
		request.Header.Set("anthropic-beta", "oauth-2025-04-20")
		request.Header.Set("User-Agent", defaultClientUserAgent)
		request.Host = s.upstream.Host
		response, err := s.client.Do(request)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, modelCatalogMaxBody+1))
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("upstream status %d: %s", response.StatusCode, upstreamErrorMessage(body, response.StatusCode))
		}
		if len(body) > modelCatalogMaxBody {
			return nil, fmt.Errorf("upstream model list exceeds %d bytes", modelCatalogMaxBody)
		}
		var payload struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
			LastID  string            `json:"last_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode upstream model list: %w", err)
		}
		for _, raw := range payload.Data {
			var model struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &model); err != nil {
				return nil, fmt.Errorf("decode upstream model: %w", err)
			}
			id := strings.TrimSpace(model.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, catalogModel{ID: id, Raw: raw})
		}
		if !payload.HasMore || payload.LastID == "" || payload.LastID == afterID {
			break
		}
		afterID = payload.LastID
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("upstream returned no models")
	}
	return models, nil
}

type modelListResponse struct {
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	FirstID *string           `json:"first_id"`
	LastID  *string           `json:"last_id"`
}

// listModels answers the Models API from the shared list. It is a metadata read,
// so the official Claude Code shape check does not apply: Claude Code's gateway
// model discovery sends neither X-App nor a session header.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := modelListDefaultLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > modelListMaxLimit {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("limit must be an integer from 1 to %d", modelListMaxLimit))
			return
		}
		limit = parsed
	}
	afterID := strings.TrimSpace(query.Get("after_id"))
	beforeID := strings.TrimSpace(query.Get("before_id"))
	if afterID != "" && beforeID != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "after_id and before_id cannot be combined")
		return
	}
	models, source := s.models.list(r.Context(), requestIngress(r.Context()).AccountAccess)
	page, hasMore, err := pageModels(models, limit, afterID, beforeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	response := modelListResponse{Data: make([]json.RawMessage, 0, len(page)), HasMore: hasMore}
	for _, model := range page {
		response.Data = append(response.Data, model.Raw)
	}
	if len(page) > 0 {
		first, last := page[0].ID, page[len(page)-1].ID
		response.FirstID, response.LastID = &first, &last
	}
	w.Header().Set(modelsSourceHeader, source)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("model_id"))
	models, source := s.models.list(r.Context(), requestIngress(r.Context()).AccountAccess)
	w.Header().Set(modelsSourceHeader, source)
	index := modelIndex(models, id)
	if index < 0 {
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q was not found", id))
		return
	}
	writeJSON(w, http.StatusOK, models[index].Raw)
}

// modelPolicies lists the account policies a configured model key can reach.
func (s *Server) modelPolicies() []store.AccountAccess {
	policies := []store.AccountAccess{store.AccountAccessCompatibleOnly}
	if s.cfg.OfficialAPIKey != "" {
		policies = append(policies, store.AccountAccessAll)
	}
	return policies
}

// modelCatalogView reports the lists callers currently receive without reading
// upstream, so opening the console causes no Anthropic traffic.
func (s *Server) modelCatalogView(w http.ResponseWriter, _ *http.Request) {
	s.writeModelStatuses(w, s.modelPolicies())
}

func (s *Server) writeModelStatuses(w http.ResponseWriter, policies []store.AccountAccess) {
	statuses := make([]modelCatalogStatus, 0, len(policies))
	for _, access := range policies {
		statuses = append(statuses, s.models.status(access))
	}
	writeJSON(w, http.StatusOK, struct {
		Policies []modelCatalogStatus `json:"policies"`
	}{statuses})
}

// refreshModels lets an operator pick up a newly released model at once instead
// of waiting for the hourly refresh or restarting.
func (s *Server) refreshModels(w http.ResponseWriter, r *http.Request) {
	policies := s.modelPolicies()
	pending := make([]<-chan struct{}, 0, len(policies))
	for _, access := range policies {
		pending = append(pending, s.models.forceRefresh(access))
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelCatalogTimeout)
	defer cancel()
	for _, done := range pending {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	s.writeModelStatuses(w, policies)
}

// pageModels applies the Models API cursors to a newest-first list: after_id
// pages towards older models and before_id towards newer ones.
func pageModels(models []catalogModel, limit int, afterID, beforeID string) ([]catalogModel, bool, error) {
	start, end := 0, len(models)
	if afterID != "" {
		index := modelIndex(models, afterID)
		if index < 0 {
			return nil, false, fmt.Errorf("after_id %q is not in the model list", afterID)
		}
		start = index + 1
	}
	if beforeID != "" {
		index := modelIndex(models, beforeID)
		if index < 0 {
			return nil, false, fmt.Errorf("before_id %q is not in the model list", beforeID)
		}
		end = index
	}
	if end-start <= limit {
		return models[start:end], false, nil
	}
	if beforeID != "" {
		return models[end-limit : end], true, nil
	}
	return models[start : start+limit], true, nil
}

func modelIndex(models []catalogModel, id string) int {
	for index, model := range models {
		if model.ID == id {
			return index
		}
	}
	return -1
}

// builtinModels is served for a policy until its first upstream read succeeds.
// It follows Anthropic's Models overview page of 2026-10-12, current models first
// and then legacy models that remain callable; update it together with that page.
// The epoch created_at is the Models API's value for an unknown release date.
var builtinModels = buildModelList([]struct{ id, name string }{
	{"claude-fable-5-1", "Claude Fable 5.1"},
	{"claude-opus-5-5", "Claude Opus 5.5"},
	{"claude-sonnet-5-5", "Claude Sonnet 5.5"},
	{"claude-haiku-5-5", "Claude Haiku 5.5"},
	{"claude-fable-5", "Claude Fable 5"},
	{"claude-opus-5", "Claude Opus 5"},
	{"claude-opus-4-8", "Claude Opus 4.8"},
	{"claude-opus-4-7", "Claude Opus 4.7"},
	{"claude-opus-4-6", "Claude Opus 4.6"},
	{"claude-opus-4-5-20251101", "Claude Opus 4.5"},
	{"claude-sonnet-5", "Claude Sonnet 5"},
	{"claude-sonnet-4-6", "Claude Sonnet 4.6"},
	{"claude-haiku-4-5-20251001", "Claude Haiku 4.5"},
})

func buildModelList(entries []struct{ id, name string }) []catalogModel {
	models := make([]catalogModel, 0, len(entries))
	for _, entry := range entries {
		raw, _ := json.Marshal(struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		}{"model", entry.id, entry.name, "1970-01-01T00:00:00Z"})
		models = append(models, catalogModel{ID: entry.id, Raw: raw})
	}
	return models
}
