package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/credential"
	"github.com/local/claude-relay/internal/metrics"
	"github.com/local/claude-relay/internal/store"
)

// checkModel is the cheapest model accepted by the OAuth subscription surface,
// used only to prove that an account's access token still reaches upstream.
const checkModel = "claude-haiku-4-5-20251001"

// Every timestamp in an administration response is epoch milliseconds, so the
// console never has to track which field uses which unit.
func epochMillis(unixSeconds int64) int64 {
	if unixSeconds == 0 {
		return 0
	}
	return unixSeconds * 1000
}

type cooldownView struct {
	Model   string `json:"model,omitempty"`
	UntilAt int64  `json:"until_at"`
	Reason  string `json:"reason,omitempty"`
}

type accountView struct {
	ProxyMode           string                       `json:"proxy_mode"`
	ProxyURL            string                       `json:"proxy_url,omitempty"`
	HasProxyAuth        bool                         `json:"has_proxy_auth"`
	Alias               string                       `json:"alias"`
	Enabled             bool                         `json:"enabled"`
	Pool                string                       `json:"pool"`
	Email               string                       `json:"email,omitempty"`
	AccountUUID         string                       `json:"account_uuid"`
	ExpiresAt           string                       `json:"expires_at,omitempty"`
	HasRefreshToken     bool                         `json:"has_refresh_token"`
	CreatedAt           int64                        `json:"created_at,omitempty"`
	LastRefreshAt       int64                        `json:"last_refresh_at,omitempty"`
	Cooldown            *cooldownView                `json:"cooldown,omitempty"`
	StickySessions      int                          `json:"sticky_sessions"`
	ActiveSessions      int                          `json:"active_sessions"`
	InFlight            int                          `json:"in_flight"`
	CountTokensInFlight int                          `json:"count_tokens_in_flight"`
	Stats               metrics.AccountStat          `json:"stats"`
	FiveHourWindow      *accountUsageWindow          `json:"five_hour_window,omitempty"`
	Refusal             *store.AccountRefusalSummary `json:"refusal,omitempty"`
}

func accountResponse(account store.Account) accountView {
	return accountView{
		Alias:           account.Alias,
		ProxyMode:       account.ProxyMode,
		ProxyURL:        store.ProxyDisplay(account.ProxyURL),
		HasProxyAuth:    proxyHasAuth(account.ProxyURL),
		Enabled:         account.Enabled,
		Pool:            account.Pool,
		Email:           account.Email,
		AccountUUID:     account.AccountUUID,
		ExpiresAt:       account.ExpiresAt,
		HasRefreshToken: strings.TrimSpace(account.RefreshToken) != "",
		CreatedAt:       epochMillis(account.CreatedAt),
		LastRefreshAt:   epochMillis(account.LastRefreshAt),
	}
}

// accountViews decorates stored accounts with the live routing state that makes
// "enabled" different from "actually receiving traffic".
func (s *Server) accountViews(r *http.Request, accounts []store.Account) ([]accountView, error) {
	now := time.Now()
	cooldowns, err := s.store.ActiveCooldowns(r.Context(), now)
	if err != nil {
		return nil, err
	}
	latest := make(map[int64]cooldownView, len(cooldowns))
	for _, cooldown := range cooldowns {
		if existing, seen := latest[cooldown.AccountID]; seen && existing.UntilAt >= cooldown.UntilAt {
			continue
		}
		latest[cooldown.AccountID] = cooldownView{
			Model:   cooldown.Model,
			UntilAt: epochMillis(cooldown.UntilAt),
			Reason:  cooldown.Reason,
		}
	}
	bindings, err := s.store.SessionBindingCounts(r.Context(), now)
	if err != nil {
		return nil, err
	}
	active, err := s.store.ActiveSessionCounts(r.Context(), now, now.Add(-activeSessionWindow))
	if err != nil {
		return nil, err
	}
	inFlight := s.load.snapshot()
	countTokensInFlight := s.countTokensLoad.snapshot()
	stats := s.metrics.AccountStats()
	refusals, err := s.store.RecentAccountRefusals(r.Context(), now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}

	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountResponse(account)
		if cooldown, cooling := latest[account.ID]; cooling {
			view.Cooldown = &cooldown
		}
		view.StickySessions = bindings[account.ID]
		view.ActiveSessions = active[account.ID]
		view.InFlight = inFlight[account.ID]
		view.CountTokensInFlight = countTokensInFlight[account.ID]
		view.Stats = stats[account.Alias]
		if refusal, seen := refusals[account.ID]; seen {
			view.Refusal = &refusal
		}
		if s.sampler != nil {
			if reading, ok := s.sampler.current(account, now); ok {
				reset, _ := strconv.ParseInt(reading.resetsAt, 10, 64)
				window := accountUsageWindow{
					ID:               "five_hour",
					UsedPercent:      reading.usedPercent,
					RemainingPercent: 100 - reading.usedPercent,
					ResetsAt:         time.Unix(reset, 0).UTC().Format(time.RFC3339),
					ObservedAt:       reading.observedAt,
				}
				view.FiveHourWindow = &window
			}
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.AllAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to list accounts")
		return
	}
	views, err := s.accountViews(r, accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to read account routing state")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views, "auto_refresh_enabled": s.cfg.AutoRefresh})
}

type overviewResponse struct {
	Version               string                `json:"version"`
	StartedAt             int64                 `json:"started_at"`
	Listen                string                `json:"listen"`
	Endpoint              string                `json:"endpoint"`
	Upstream              string                `json:"upstream"`
	UpstreamProxy         string                `json:"upstream_proxy,omitempty"`
	AutoRefresh           bool                  `json:"auto_refresh_enabled"`
	MaxRequestBytes       int64                 `json:"max_request_bytes"`
	MaxInflightPerAccount int                   `json:"max_inflight_per_account"`
	RelayAPIKey           string                `json:"relay_api_key"`
	ExperimentalAPIKey    string                `json:"experimental_api_key,omitempty"`
	OfficialAPIKey        string                `json:"official_api_key,omitempty"`
	Accounts              accountTotals         `json:"accounts"`
	StickySessions        int                   `json:"sticky_sessions"`
	Requests              metrics.Summary       `json:"requests"`
	RoutingPolicy         string                `json:"routing_policy"`
	OfficialMinVersion    string                `json:"official_min_cli_version,omitempty"`
	OfficialMaxVersion    string                `json:"official_max_cli_version,omitempty"`
	AllowA6APIProbes      bool                  `json:"allow_a6api_probes"`
	RefusalBilling        refusalBillingOptions `json:"refusal_billing"`
}

type accountTotals struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	Available int `json:"available"`
	Cooling   int `json:"cooling"`
	Expired   int `json:"expired"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	accounts, err := s.store.AllAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to list accounts")
		return
	}
	cooldowns, err := s.store.ActiveCooldowns(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to read account cooldowns")
		return
	}
	cooling := make(map[int64]bool, len(cooldowns))
	for _, cooldown := range cooldowns {
		cooling[cooldown.AccountID] = true
	}
	bindings, err := s.store.SessionBindingCounts(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to count session bindings")
		return
	}

	totals := accountTotals{Total: len(accounts)}
	for _, account := range accounts {
		expired := account.Credential.IsExpired(now)
		if account.Enabled {
			totals.Enabled++
		}
		if account.Enabled && cooling[account.ID] {
			totals.Cooling++
		}
		if expired {
			totals.Expired++
		}
		refreshable := s.cfg.AutoRefresh && strings.TrimSpace(account.RefreshToken) != ""
		if account.Enabled && !cooling[account.ID] && (!expired || refreshable) {
			totals.Available++
		}
	}
	sticky := 0
	for _, count := range bindings {
		sticky += count
	}

	writeJSON(w, http.StatusOK, overviewResponse{
		Version:               buildVersion(),
		StartedAt:             s.startedAt.UnixMilli(),
		Listen:                s.cfg.Listen,
		Endpoint:              relayEndpoint(r),
		Upstream:              s.upstream.String(),
		UpstreamProxy:         store.ProxyDisplay(s.cfg.UpstreamProxy),
		AutoRefresh:           s.cfg.AutoRefresh,
		MaxRequestBytes:       s.cfg.MaxRequestBytes,
		MaxInflightPerAccount: s.selector.concurrency.current(),
		RelayAPIKey:           s.cfg.RelayAPIKey,
		ExperimentalAPIKey:    s.cfg.ExperimentalAPIKey,
		OfficialAPIKey:        s.cfg.OfficialAPIKey,
		Accounts:              totals,
		StickySessions:        sticky,
		Requests:              s.metrics.Summary(now),
		RoutingPolicy:         s.routingPolicy.current(),
		OfficialMinVersion:    s.officialVersion.current().Min,
		OfficialMaxVersion:    s.officialVersion.current().Max,
		AllowA6APIProbes:      s.allowA6APIProbes.Load(),
		RefusalBilling:        s.refusalBilling.current(),
	})
}

func (s *Server) setA6APIProbes(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "enabled is required")
		return
	}
	s.a6APIProbeMu.Lock()
	defer s.a6APIProbeMu.Unlock()
	if err := s.store.SetRuntimeSetting(r.Context(), a6APIProbeSetting, strconv.FormatBool(*request.Enabled)); err != nil {
		slog.Error("persist A6API probe setting", "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist A6API probe setting")
		return
	}
	s.allowA6APIProbes.Store(*request.Enabled)
	slog.Info("A6API probe admission changed", "enabled", *request.Enabled, "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"allow_a6api_probes": *request.Enabled})
}

type officialVersionBoundsRequest struct {
	MinVersion string `json:"min_version"`
	MaxVersion string `json:"max_version"`
}

func (s *Server) setOfficialVersionBounds(w http.ResponseWriter, r *http.Request) {
	var request officialVersionBoundsRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.MinVersion = strings.TrimSpace(request.MinVersion)
	request.MaxVersion = strings.TrimSpace(request.MaxVersion)
	if err := validateOfficialVersionBounds(request.MinVersion, request.MaxVersion); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	bounds := officialVersionBounds{Min: request.MinVersion, Max: request.MaxVersion}
	if err := s.officialVersion.persist(r.Context(), s.store, bounds); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist official CLI version bounds")
		return
	}
	slog.Info("official CLI version bounds changed", "min_version", bounds.Min, "max_version", bounds.Max, "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"official_min_cli_version": bounds.Min,
		"official_max_cli_version": bounds.Max,
		"persistent":               true,
	})
}

type routingPolicyRequest struct {
	Policy string `json:"policy"`
}

func (s *Server) setRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	var request routingPolicyRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	previous := s.routingPolicy.current()
	policy, err := s.routingPolicy.set(request.Policy)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if policy != previous {
		slog.Info("routing policy changed", "from", previous, "to", policy,
			"remote_addr", r.RemoteAddr)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routing_policy": policy,
		"persistent":     false,
	})
}

// relayEndpoint reconstructs the address a model client should target, honouring
// the forwarded headers a reverse proxy in front of the relay would set.
func relayEndpoint(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.SplitN(forwarded, ",", 2)[0])
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0])
	}
	return scheme + "://" + host
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		return revision + "+dirty"
	}
	return revision
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "limit must be a positive integer")
			return
		}
		limit = min(parsed, 1000)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": s.metrics.Recent(limit),
		"summary":  s.metrics.Summary(time.Now()),
	})
}

func (s *Server) setAccountEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, err := s.store.SetAccountEnabled(r.Context(), alias, enabled)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	slog.Info("account activation changed", "account", account.Alias, "enabled", enabled)
	writeJSON(w, http.StatusOK, accountResponse(account))
}

type accountPoolRequest struct {
	Pool string `json:"pool"`
}

func (s *Server) setAccountPool(w http.ResponseWriter, r *http.Request) {
	var request accountPoolRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Pool = strings.TrimSpace(request.Pool)
	if err := store.ValidateAccountPool(request.Pool); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, err := s.store.SetAccountPool(r.Context(), alias, request.Pool)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountResponse(account))
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, err := s.store.DeleteAccount(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}
	s.exits.forget(account.ID)
	s.metrics.Forget(account.Alias)
	s.sampler.forget(account)
	writeJSON(w, http.StatusOK, map[string]any{"alias": account.Alias, "deleted": true})
}

type renameRequest struct {
	Alias string `json:"alias"`
}

func (s *Server) renameAccount(w http.ResponseWriter, r *http.Request) {
	var request renameRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	current := strings.TrimSpace(r.PathValue("alias"))
	account, err := s.store.RenameAccount(r.Context(), current, request.Alias)
	if err != nil {
		writeError(w, http.StatusConflict, "invalid_request_error", err.Error())
		return
	}
	s.metrics.Forget(current)
	writeJSON(w, http.StatusOK, accountResponse(account))
}

func (s *Server) clearAccountCooldown(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, found, err := s.store.AccountByAlias(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to read account")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("account %q was not found", alias))
		return
	}
	removed, err := s.store.ClearCooldowns(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to clear cooldowns")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alias": account.Alias, "cleared": removed})
}

// refreshAccount performs an operator-triggered token rotation. It obeys the same
// ownership rules as automatic refresh: only an enabled account may rotate, and
// the global emergency stop still wins.
func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, found, err := s.store.AccountByAlias(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to read account")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("account %q was not found", alias))
		return
	}
	if !s.cfg.AutoRefresh {
		writeError(w, http.StatusConflict, "invalid_request_error", "automatic refresh is globally disabled")
		return
	}
	if !account.Enabled {
		writeError(w, http.StatusConflict, "invalid_request_error", "enable the account before it may refresh its tokens")
		return
	}
	refreshed, err := s.tokens.refreshNow(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadGateway, "authentication_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountResponse(refreshed))
}

type checkResponse struct {
	Alias      string `json:"alias"`
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

// checkAccount proves an account still reaches upstream by counting tokens for a
// trivial prompt. It never refreshes, so a disabled account can be verified
// without this relay taking ownership of its refresh-token chain.
func (s *Server) checkAccount(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	account, found, err := s.store.AccountByAlias(r.Context(), alias)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to read account")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("account %q was not found", alias))
		return
	}
	if account.Credential.IsExpired(time.Now()) {
		writeJSON(w, http.StatusOK, checkResponse{
			Alias:  account.Alias,
			OK:     false,
			Detail: "access token has expired; refresh the account before checking",
		})
		return
	}

	body := []byte(`{"model":"` + checkModel + `","messages":[{"role":"user","content":"ping"}]}`)
	attributed, err := addSubscriptionAttribution(body, http.Header{}, account.Credential, attributionOptions{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to build check request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + "/v1/messages/count_tokens"
	target.RawQuery = "beta=true"
	request, err := http.NewRequestWithContext(withAccountExit(ctx, account), http.MethodPost, target.String(), bytes.NewReader(attributed.Body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to build check request")
		return
	}
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("User-Agent", defaultClientUserAgent)
	request.Host = s.upstream.Host

	started := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusOK, checkResponse{
			Alias:      account.Alias,
			OK:         false,
			DurationMS: time.Since(started).Milliseconds(),
			Detail:     "upstream request failed: " + err.Error(),
		})
		return
	}
	defer func() { _ = response.Body.Close() }()
	preview, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	result := checkResponse{
		Alias:      account.Alias,
		OK:         response.StatusCode < 400,
		Status:     response.StatusCode,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if !result.OK {
		result.Detail = upstreamErrorMessage(preview, response.StatusCode)
	}
	writeJSON(w, http.StatusOK, result)
}

func upstreamErrorMessage(body []byte, status int) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return payload.Error.Message
	}
	return http.StatusText(status)
}

type importRequest struct {
	Alias      string `json:"alias"`
	Credential string `json:"credential"`
	Replace    bool   `json:"replace"`
}

// importAccount accepts a pasted CLIProxyAPI credential document so migration
// does not require shell access to the server. The account arrives disabled,
// exactly like the command-line importer.
func (s *Server) importAccount(w http.ResponseWriter, r *http.Request) {
	var request importRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Alias = strings.TrimSpace(request.Alias)
	if err := store.ValidateAlias(request.Alias); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	cred, err := credential.ParseImport([]byte(request.Credential))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !request.Replace {
		accounts, listErr := s.store.AllAccounts(r.Context())
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "api_error", "failed to check existing accounts")
			return
		}
		for _, existing := range accounts {
			if strings.EqualFold(existing.Alias, request.Alias) || existing.AccountUUID == cred.AccountUUID {
				writeError(w, http.StatusConflict, "invalid_request_error",
					fmt.Sprintf("account %q already exists; explicit replacement is required", existing.Alias))
				return
			}
		}
	}
	account, err := s.store.ImportAccount(r.Context(), request.Alias, cred)
	if err != nil {
		writeError(w, http.StatusConflict, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, accountResponse(account))
}

type oauthStartRequest struct {
	ProxyMode string `json:"proxy_mode"`
	ProxyURL  string `json:"proxy_url"`
	Alias     string `json:"alias"`
}

func (s *Server) startClaudeOAuth(w http.ResponseWriter, r *http.Request) {
	var request oauthStartRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	request.Alias = strings.TrimSpace(request.Alias)
	if err := store.ValidateAlias(request.Alias); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if request.ProxyMode == "" {
		request.ProxyMode = "direct"
	}
	request.ProxyURL = strings.TrimSpace(request.ProxyURL)
	if err := store.ValidateAccountProxy(request.ProxyMode, request.ProxyURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	result, err := s.oauth.StartWithProxy(request.Alias, request.ProxyMode, request.ProxyURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to start OAuth login")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type oauthExchangeRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

func (s *Server) exchangeClaudeOAuth(w http.ResponseWriter, r *http.Request) {
	var request oauthExchangeRequest
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	session, err := s.oauth.PendingSession(strings.TrimSpace(request.SessionID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "authentication_error", err.Error())
		return
	}
	exit := store.Account{ProxyMode: session.ProxyMode, ProxyURL: session.ProxyURL}
	alias, cred, err := s.oauth.Exchange(withAccountExit(r.Context(), exit), strings.TrimSpace(request.SessionID), request.Code)
	if err != nil {
		slog.Warn("OAuth authorization exchange failed", "error", err)
		writeError(w, http.StatusBadRequest, "authentication_error", err.Error())
		return
	}
	account, err := s.store.ImportAccount(r.Context(), alias, cred)
	if err != nil {
		slog.Error("persist OAuth authorization failed", "account", alias, "error", err)
		writeError(w, http.StatusConflict, "api_error", err.Error())
		return
	}
	account, err = s.store.SetAccountProxy(r.Context(), account.Alias, session.ProxyMode, session.ProxyURL)
	if err != nil {
		slog.Error("persist OAuth exit failed", "account", alias, "error", err)
		writeError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	slog.Info("OAuth authorization succeeded", "account", account.Alias, "enabled", account.Enabled, "expires_at", account.ExpiresAt)
	writeJSON(w, http.StatusCreated, accountResponse(account))
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	limited := http.MaxBytesReader(w, r.Body, 1<<20)
	defer limited.Close()
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode JSON body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode JSON body: trailing JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.Copy(w, &buffer)
}
