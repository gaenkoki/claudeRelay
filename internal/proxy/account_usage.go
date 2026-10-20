package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	accountUsageCacheTTL = 2 * time.Minute
	accountUsageTimeout  = 20 * time.Second
	accountUsageMaxBody  = 1 << 20
)

type accountUsageWindow struct {
	ID               string  `json:"id"`
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
	ObservedAt       int64   `json:"observed_at,omitempty"`
}

type accountExtraUsage struct {
	Enabled           bool     `json:"enabled"`
	MonthlyLimitCents float64  `json:"monthly_limit_cents"`
	UsedCreditsCents  float64  `json:"used_credits_cents"`
	Utilization       *float64 `json:"utilization,omitempty"`
}

type accountUsageView struct {
	Alias      string               `json:"alias"`
	FetchedAt  int64                `json:"fetched_at"`
	Cached     bool                 `json:"cached"`
	PlanType   string               `json:"plan_type,omitempty"`
	Windows    []accountUsageWindow `json:"windows"`
	ExtraUsage *accountExtraUsage   `json:"extra_usage,omitempty"`
}

type rawUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type rawExtraUsage struct {
	Enabled      bool     `json:"is_enabled"`
	MonthlyLimit float64  `json:"monthly_limit"`
	UsedCredits  float64  `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

type rawUsagePayload struct {
	FiveHour       *rawUsageWindow `json:"five_hour"`
	SevenDay       *rawUsageWindow `json:"seven_day"`
	SevenDayOAuth  *rawUsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus   *rawUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *rawUsageWindow `json:"seven_day_sonnet"`
	SevenDayCowork *rawUsageWindow `json:"seven_day_cowork"`
	Fable          *rawUsageWindow `json:"iguana_necktie"`
	ExtraUsage     *rawExtraUsage  `json:"extra_usage"`
}

type rawProfilePayload struct {
	Account struct {
		HasClaudeMax *bool `json:"has_claude_max"`
		HasClaudePro *bool `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType   string `json:"organization_type"`
		SubscriptionStatus string `json:"subscription_status"`
	} `json:"organization"`
}

type accountUsageCacheEntry struct {
	view      accountUsageView
	expiresAt time.Time
}

type accountUsageManager struct {
	store    *store.Store
	tokens   *tokenManager
	client   *http.Client
	upstream *url.URL

	cacheMu sync.RWMutex
	cache   map[string]accountUsageCacheEntry
	locks   sync.Map
}

func newAccountUsageManager(database *store.Store, tokens *tokenManager, client *http.Client, upstream *url.URL) *accountUsageManager {
	return &accountUsageManager{
		store: database, tokens: tokens, client: client, upstream: upstream,
		cache: make(map[string]accountUsageCacheEntry),
	}
}

func (m *accountUsageManager) get(ctx context.Context, account store.Account, force bool) (accountUsageView, error) {
	cacheKey := accountUsageCacheKey(account)
	if !force {
		if view, ok := m.cached(cacheKey, time.Now()); ok {
			view.Alias = account.Alias
			view.Cached = true
			return view, nil
		}
	}
	lockValue, _ := m.locks.LoadOrStore(cacheKey, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if !force {
		if view, ok := m.cached(cacheKey, time.Now()); ok {
			view.Alias = account.Alias
			view.Cached = true
			return view, nil
		}
	}

	current, found, err := m.store.AccountByID(ctx, account.ID)
	if err != nil {
		return accountUsageView{}, err
	}
	if !found {
		return accountUsageView{}, fmt.Errorf("account %q was removed", account.Alias)
	}
	if current.Enabled {
		current, err = m.tokens.ensureFresh(ctx, current)
		if err != nil {
			return accountUsageView{}, err
		}
	} else if current.Credential.IsExpired(time.Now()) {
		return accountUsageView{}, fmt.Errorf("account %q access token has expired; enable or refresh it before reading usage", current.Alias)
	}

	requestCtx, cancel := context.WithTimeout(ctx, accountUsageTimeout)
	defer cancel()
	var usage rawUsagePayload
	if err := m.getJSON(withAccountExit(requestCtx, current), "/api/oauth/usage", current.AccessToken, &usage); err != nil {
		return accountUsageView{}, fmt.Errorf("read account usage: %w", err)
	}
	view := accountUsageView{
		Alias: current.Alias, FetchedAt: time.Now().UnixMilli(), Windows: buildUsageWindows(usage),
	}
	if usage.ExtraUsage != nil {
		view.ExtraUsage = &accountExtraUsage{
			Enabled: usage.ExtraUsage.Enabled, MonthlyLimitCents: usage.ExtraUsage.MonthlyLimit,
			UsedCreditsCents: usage.ExtraUsage.UsedCredits, Utilization: usage.ExtraUsage.Utilization,
		}
	}
	var profile rawProfilePayload
	if err := m.getJSON(withAccountExit(requestCtx, current), "/api/oauth/profile", current.AccessToken, &profile); err == nil {
		view.PlanType = resolveClaudePlan(profile)
	}
	m.cacheMu.Lock()
	m.cache[accountUsageCacheKey(current)] = accountUsageCacheEntry{view: view, expiresAt: time.Now().Add(accountUsageCacheTTL)}
	m.cacheMu.Unlock()
	return view, nil
}

func accountUsageCacheKey(account store.Account) string {
	if uuid := strings.TrimSpace(account.AccountUUID); uuid != "" {
		return "uuid:" + strings.ToLower(uuid)
	}
	return fmt.Sprintf("id:%d", account.ID)
}

func (m *accountUsageManager) cached(cacheKey string, now time.Time) (accountUsageView, bool) {
	m.cacheMu.RLock()
	entry, ok := m.cache[cacheKey]
	m.cacheMu.RUnlock()
	return entry.view, ok && now.Before(entry.expiresAt)
}

func (m *accountUsageManager) getJSON(ctx context.Context, path, accessToken string, destination any) error {
	target := *m.upstream
	target.Path = strings.TrimRight(m.upstream.Path, "/") + path
	target.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	response, err := m.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, accountUsageMaxBody))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s", upstreamErrorMessage(body, response.StatusCode))
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func buildUsageWindows(payload rawUsagePayload) []accountUsageWindow {
	ordered := []struct {
		id     string
		window *rawUsageWindow
	}{
		{"five_hour", payload.FiveHour},
		{"seven_day", payload.SevenDay},
		{"seven_day_oauth_apps", payload.SevenDayOAuth},
		{"seven_day_opus", payload.SevenDayOpus},
		{"seven_day_sonnet", payload.SevenDaySonnet},
		{"seven_day_cowork", payload.SevenDayCowork},
		{"seven_day_fable", payload.Fable},
	}
	windows := make([]accountUsageWindow, 0, len(ordered))
	for _, candidate := range ordered {
		if candidate.window == nil || candidate.window.Utilization == nil || math.IsNaN(*candidate.window.Utilization) || math.IsInf(*candidate.window.Utilization, 0) {
			continue
		}
		used := math.Max(0, math.Min(100, *candidate.window.Utilization))
		window := accountUsageWindow{ID: candidate.id, UsedPercent: used, RemainingPercent: 100 - used}
		if candidate.window.ResetsAt != nil {
			window.ResetsAt = strings.TrimSpace(*candidate.window.ResetsAt)
		}
		windows = append(windows, window)
	}
	return windows
}

func resolveClaudePlan(profile rawProfilePayload) string {
	if profile.Account.HasClaudeMax != nil && *profile.Account.HasClaudeMax {
		return "max"
	}
	if profile.Account.HasClaudePro != nil && *profile.Account.HasClaudePro {
		return "pro"
	}
	// Organization plans leave the account-level Max/Pro flags false, so this has
	// to run before the free check below or every org account reports as free.
	if plan := organizationPlan(profile.Organization.OrganizationType); plan != "" &&
		strings.EqualFold(profile.Organization.SubscriptionStatus, "active") {
		return plan
	}
	if profile.Account.HasClaudeMax != nil && profile.Account.HasClaudePro != nil &&
		!*profile.Account.HasClaudeMax && !*profile.Account.HasClaudePro {
		return "free"
	}
	return ""
}

// organizationPlan turns an organization_type such as "claude_team" or
// "claude_enterprise" into the short label the console displays.
//
// An unrecognized type keeps its own name rather than being discarded. Only
// "claude_team" was confirmed against a live account, and reporting a plan this
// build has not seen is more useful than reporting none: the operator sees the
// actual value instead of a blank field, which is also what they need in order
// to have it added here.
func organizationPlan(organizationType string) string {
	normalized := strings.ToLower(strings.TrimSpace(organizationType))
	if normalized == "" {
		return ""
	}
	return strings.TrimPrefix(normalized, "claude_")
}

func (s *Server) accountUsage(w http.ResponseWriter, r *http.Request, force bool) {
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
	view, err := s.usage.get(r.Context(), account, force)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if force {
		if err := s.recordAccountUsageObservation(r.Context(), account, view, requestIDFromContext(r.Context())); err != nil {
			writeError(w, http.StatusInternalServerError, "api_error", "failed to save five-hour observation")
			return
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) recordAccountUsageObservation(ctx context.Context, account store.Account, view accountUsageView, eventID string) error {
	for _, window := range view.Windows {
		if window.ID != "five_hour" {
			continue
		}
		reset := normalizeResetIdentity(window.ResetsAt)
		event := store.FiveHourEvent{
			EventKey: "oauth:" + eventID + ":" + strconv.FormatInt(view.FetchedAt, 10), AccountID: account.ID,
			ResetsAt: reset, Kind: store.FiveHourEventOAuth, ObservedAt: view.FetchedAt,
			CompletedAt: view.FetchedAt, UsedPercent: window.UsedPercent, Complete: true,
		}
		if err := s.store.AddFiveHourEvents(ctx, []store.FiveHourEvent{event}); err != nil {
			return err
		}
		if resetSeconds, parseErr := strconv.ParseInt(reset, 10, 64); parseErr == nil && time.Now().Before(time.Unix(resetSeconds, 0)) {
			s.sampler.observe(account, fiveHourReading{resetsAt: reset, usedPercent: window.UsedPercent, observedAt: view.FetchedAt})
		}
		break
	}
	return nil
}
