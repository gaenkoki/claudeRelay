package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
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
	"sync/atomic"
	"time"

	"github.com/local/claude-relay/internal/accounting"
	"github.com/local/claude-relay/internal/claudeoauth"
	"github.com/local/claude-relay/internal/config"
	"github.com/local/claude-relay/internal/metrics"
	"github.com/local/claude-relay/internal/store"
	"github.com/local/claude-relay/internal/webui"
)

var allowedPaths = map[string]struct{}{
	"/v1/messages":              {},
	"/v1/messages/count_tokens": {},
}

const missingUsageWarningInterval = 5 * time.Minute
const a6APIProbeSetting = "allow_a6api_probes"

type Server struct {
	cfg              config.Config
	store            *store.Store
	selector         accountSelector
	load             *accountLoadTracker
	countTokensLoad  *accountLoadTracker
	upstream         *url.URL
	httpServer       *http.Server
	exits            *accountTransport
	client           *http.Client
	oauth            *claudeoauth.Client
	tokens           *tokenManager
	metrics          *metrics.Recorder
	usage            *accountUsageManager
	models           *modelCatalog
	accounting       *accounting.Manager
	sampler          *subscriptionSampler
	routingPolicy    *routingPolicyState
	officialVersion  *officialVersionPolicy
	refusalBilling   *refusalBillingPolicy
	allowA6APIProbes atomic.Bool
	a6APIProbeMu     sync.Mutex // Serialize persistence and publication of admin changes.
	// missingUsageWarningAt rate-limits diagnostics for successful Messages
	// responses whose decoded body did not contain Anthropic usage metadata.
	missingUsageWarningAt atomic.Int64
	startedAt             time.Time
}

func NewServer(cfg config.Config, database *store.Store) (*Server, error) {
	if database == nil {
		return nil, fmt.Errorf("account store is required")
	}
	if cfg.MaxInflightPerAccount == 0 {
		cfg.MaxInflightPerAccount = config.DefaultMaxInflightPerAccount
	}
	if cfg.MaxInflightPerAccount < 1 {
		return nil, fmt.Errorf("config max_inflight_per_account must be positive")
	}
	if cfg.MaxCountTokensInflightPerAccount == 0 {
		cfg.MaxCountTokensInflightPerAccount = config.DefaultMaxCountTokensInflightPerAccount
	}
	if cfg.MaxCountTokensInflightPerAccount < 1 {
		return nil, fmt.Errorf("config max_count_tokens_inflight_per_account must be positive")
	}
	if cfg.MaxActiveSessionsPerAccount == 0 {
		cfg.MaxActiveSessionsPerAccount = config.DefaultMaxActiveSessionsPerAccount
	}
	if cfg.MaxActiveSessionsPerAccount < 1 {
		return nil, fmt.Errorf("config max_active_sessions_per_account must be positive")
	}
	officialVersion, err := loadOfficialVersionPolicy(database)
	if err != nil {
		return nil, err
	}
	refusalBilling, err := loadRefusalBillingPolicy(database)
	if err != nil {
		return nil, err
	}
	upstream, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.UpstreamProxy != "" {
		proxyURL, parseErr := url.Parse(cfg.UpstreamProxy)
		if parseErr != nil {
			return nil, fmt.Errorf("parse upstream proxy URL: %w", parseErr)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	exits := &accountTransport{global: transport, globalURL: cfg.UpstreamProxy, accounts: make(map[int64]accountTransportEntry)}
	oauthClient := claudeoauth.New(&http.Client{Transport: exits, Timeout: 60 * time.Second})
	load := newAccountLoadTracker()
	countTokensLoad := newAccountLoadTracker()
	sessions := newSessionAdmissionTracker(database, cfg.MaxActiveSessionsPerAccount)
	sampler := newSubscriptionSampler(cfg.MaxInflightPerAccount)
	routingPolicy := &routingPolicyState{}
	server := &Server{
		cfg:             cfg,
		store:           database,
		load:            load,
		countTokensLoad: countTokensLoad,
		selector: accountSelector{
			store: database, load: load, countTokensLoad: countTokensLoad,
			maxInflightPerAcct:            cfg.MaxInflightPerAccount,
			maxCountTokensInflightPerAcct: cfg.MaxCountTokensInflightPerAccount,
			sessions:                      sessions,
			sampler:                       sampler,
			policy:                        routingPolicy,
		},
		upstream:        upstream,
		client:          &http.Client{Transport: exits},
		exits:           exits,
		oauth:           oauthClient,
		metrics:         metrics.New(cfg.RequestLogSize),
		sampler:         sampler,
		routingPolicy:   routingPolicy,
		officialVersion: officialVersion,
		refusalBilling:  refusalBilling,
		startedAt:       time.Now(),
	}
	server.tokens = &tokenManager{store: database, oauth: oauthClient, autoRefresh: cfg.AutoRefresh}
	probeValue, probeFound, err := database.RuntimeSetting(context.Background(), a6APIProbeSetting)
	if err != nil {
		return nil, err
	}
	if probeFound {
		allowed, err := strconv.ParseBool(probeValue)
		if err != nil {
			return nil, fmt.Errorf("decode persisted A6API probe setting: %w", err)
		}
		server.allowA6APIProbes.Store(allowed)
	}
	server.usage = newAccountUsageManager(database, server.tokens, server.client, upstream)
	server.models = newModelCatalog(server.fetchModelCatalog)
	server.accounting = accounting.NewManager(database)
	server.httpServer = &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return server, nil
}

func (s *Server) Run(ctx context.Context) error {
	defer s.exits.CloseIdleConnections()
	accountingCtx, stopAccounting := context.WithCancel(context.Background())
	defer stopAccounting()
	go s.accounting.Run(accountingCtx)
	errCh := make(chan error, 1)
	go func() {
		slog.Info("relay listening", "address", s.cfg.Listen, "upstream", s.upstream.String())
		errCh <- s.httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		stopAccounting()
		flushCtx, cancel := context.WithTimeout(context.Background(), accounting.FlushInterval)
		defer cancel()
		_ = s.accounting.Flush(flushCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		stopAccounting()
		if err := s.accounting.Flush(shutdownCtx); err != nil {
			return fmt.Errorf("flush usage accounting: %w", err)
		}
		return nil
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", webui.Index)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", webui.Assets()))
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /ops/v1/availability", s.availability)
	mux.HandleFunc("POST /v1/messages", s.forward)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.forward)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("GET /v1/models/{model_id}", s.getModel)
	mux.HandleFunc("GET /admin/v1/overview", s.overview)
	mux.HandleFunc("POST /admin/v1/routing/policy", s.setRoutingPolicy)
	mux.HandleFunc("POST /admin/v1/official/version-bounds", s.setOfficialVersionBounds)
	mux.HandleFunc("POST /admin/v1/official/a6api-probes", s.setA6APIProbes)
	mux.HandleFunc("POST /admin/v1/refusal-billing", s.setRefusalBilling)
	mux.HandleFunc("GET /admin/v1/models", s.modelCatalogView)
	mux.HandleFunc("POST /admin/v1/models/refresh", s.refreshModels)
	mux.HandleFunc("GET /admin/v1/requests", s.listRequests)
	mux.HandleFunc("GET /admin/v1/accounts", s.listAccounts)
	mux.HandleFunc("POST /admin/v1/accounts/import", s.importAccount)
	mux.HandleFunc("DELETE /admin/v1/accounts/{alias}", s.deleteAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/enable", func(w http.ResponseWriter, r *http.Request) { s.setAccountEnabled(w, r, true) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/disable", func(w http.ResponseWriter, r *http.Request) { s.setAccountEnabled(w, r, false) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/rename", s.renameAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/pool", s.setAccountPool)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/refresh", s.refreshAccount)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/cooldown/clear", s.clearAccountCooldown)
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/check", s.checkAccount)
	mux.HandleFunc("GET /admin/v1/accounts/{alias}/usage", func(w http.ResponseWriter, r *http.Request) { s.accountUsage(w, r, false) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/usage/refresh", func(w http.ResponseWriter, r *http.Request) { s.accountUsage(w, r, true) })
	mux.HandleFunc("POST /admin/v1/accounts/{alias}/proxy", s.setAccountProxy)
	mux.HandleFunc("POST /admin/v1/oauth/claude/start", s.startClaudeOAuth)
	mux.HandleFunc("POST /admin/v1/oauth/claude/exchange", s.exchangeClaudeOAuth)
	mux.HandleFunc("GET /admin/v1/usage", s.usageDashboard)
	mux.HandleFunc("GET /admin/v1/usage/prices", s.listModelPrices)
	mux.HandleFunc("POST /admin/v1/usage/prices", s.saveModelPrice)
	mux.HandleFunc("GET /admin/v1/usage/five-hour/export", s.exportFiveHourObservations)
	mux.HandleFunc("DELETE /admin/v1/usage/five-hour", s.clearFiveHourObservations)
	mux.HandleFunc("DELETE /admin/v1/usage", s.clearUsageAccounting)
	return withRequestID(s.securityHeaders(s.authenticate(mux)))
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/ops/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("x-api-key"))
		if provided == "" {
			provided = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			if !secureKeyEqual(provided, s.cfg.AdminAPIKey) {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ops/") {
			if s.cfg.AvailabilityAPIKey == "" || !secureKeyEqual(provided, s.cfg.AvailabilityAPIKey) {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		var policy ingressPolicy
		switch {
		case secureKeyEqual(provided, s.cfg.RelayAPIKey):
			policy = compatibleIngress
		case s.cfg.ExperimentalAPIKey != "" && secureKeyEqual(provided, s.cfg.ExperimentalAPIKey):
			policy = experimentalIngress
		case s.cfg.OfficialAPIKey != "" && secureKeyEqual(provided, s.cfg.OfficialAPIKey):
			policy = officialIngress
		default:
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		ctx := context.WithValue(r.Context(), ingressContextKey{}, policy)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ingressContextKey struct{}

func secureKeyEqual(provided, expected string) bool {
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

// requestIngress reports the policy selected by the authenticating API key.
func requestIngress(ctx context.Context) ingressPolicy {
	policy, _ := ctx.Value(ingressContextKey{}).(ingressPolicy)
	return policy
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (s *Server) forward(w http.ResponseWriter, incoming *http.Request) {
	requestID := requestIDFromContext(incoming.Context())
	started := time.Now()
	refusalOptions := s.refusalBilling.current()
	event := metrics.Event{RequestID: requestID, Time: started, Path: incoming.URL.Path}
	defer func() {
		event.Duration = time.Since(started)
		s.metrics.Record(event)
	}()
	fail := func(status int, errorType, message string) {
		event.Status = status
		event.Error = message
		writeError(w, status, errorType, message)
	}

	if _, ok := allowedPaths[incoming.URL.Path]; !ok {
		fail(http.StatusNotFound, "not_found_error", "unsupported endpoint")
		return
	}
	ingress := requestIngress(incoming.Context())
	event.Ingress = ingress.Name
	// A6API sends non-CC health probes and pelican self-tests. This temporary, opt-in exception only
	// applies after official-key authentication; the caller-controlled header
	// is a marker, not proof of platform identity. Preserve the classification.
	allowA6APIProbe := ingress.RequireClaudeCode && s.allowA6APIProbes.Load() &&
		(strings.TrimSpace(incoming.Header.Get("X-A6API-Probe-Id")) != "" ||
			strings.TrimSpace(incoming.Header.Get("X-A6API-Self-Test-Kind")) == "pelican")
	if ingress.RequireClaudeCode && !allowA6APIProbe {
		// Requests without complete Claude Code headers can never be admitted,
		// so reject them before reading or parsing the body.
		if client := classifyClientHeaders(incoming.Header); !client.Evidence.hasCompleteHeaders() {
			recordClientObservation(&event, client)
			slog.Warn("rejected non-Claude-Code request on official ingress", "request_id", requestID, "path", incoming.URL.Path,
				"ingress", ingress.Name, "client_class", client.Class, "client_kind", client.Kind,
				"content_length", incoming.ContentLength, "body_read", false)
			fail(http.StatusForbidden, "permission_error", "official ingress requires a Claude Code-shaped request")
			return
		}
	}
	limited := http.MaxBytesReader(w, incoming.Body, s.cfg.MaxRequestBytes)
	body, err := io.ReadAll(limited)
	if closeErr := limited.Close(); closeErr != nil {
		slog.Warn("close incoming request", "request_id", requestID, "error", closeErr)
	}
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
			return
		}
		fail(http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	includeMetadata := incoming.URL.Path == "/v1/messages"
	normalizedToolNames := 0
	var toolNames toolNameMapping
	if ingress.NormalizeToolNames {
		body, normalizedToolNames, toolNames, err = normalizeExperimentalToolNames(body)
		if err != nil {
			fail(http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if normalizedToolNames > 0 {
			slog.Info("normalized experimental tool names", "request_id", requestID, "path", incoming.URL.Path,
				"ingress", ingress.Name, "names_changed", normalizedToolNames)
		}
	}
	route, routeErr := deriveRequestRoute(body, incoming.Header, ingress, incoming.URL.Path)
	if routeErr != nil {
		fail(http.StatusBadRequest, "invalid_request_error", routeErr.Error())
		return
	}
	event.Model = route.Model
	recordClientObservation(&event, route.Client)
	if allowA6APIProbe {
		slog.Warn("temporary A6API probe admission bypasses Claude Code checks",
			"request_id", requestID, "path", incoming.URL.Path, "ingress", ingress.Name,
			"client_class", route.Client.Class, "client_kind", route.Client.Kind)
	}
	if ingress.RequireClaudeCode && !allowA6APIProbe && route.Client.Class != clientClassCCCandidate {
		slog.Warn("rejected non-Claude-Code request on official ingress", "request_id", requestID, "path", incoming.URL.Path, "ingress", ingress.Name, "client_class", route.Client.Class, "client_kind", route.Client.Kind)
		fail(http.StatusForbidden, "permission_error", "official ingress requires a Claude Code-shaped request")
		return
	}
	if ingress.RequireClaudeCode && !allowA6APIProbe {
		if err := s.officialVersion.check(incoming.Header.Get("User-Agent")); err != nil {
			slog.Warn("rejected Claude Code version on official ingress", "request_id", requestID,
				"client_version", extractClaudeCodeUAVersion(incoming.Header.Get("User-Agent")), "error", err)
			fail(http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
	}
	forcedAlias := incoming.Header.Get(accountHeader)
	excluded := make(map[int64]bool)
	var response *http.Response
	var selected selection
	var releaseLoad func()
	var releaseSession func()
	defer func() {
		if releaseLoad != nil {
			releaseLoad()
		}
		if releaseSession != nil {
			releaseSession()
		}
	}()
	for attempt := 0; attempt < 2; attempt++ {
		selected, err = s.selector.selectAccount(incoming.Context(), route, forcedAlias, excluded)
		if err != nil {
			// A failed alternate selection has no final account. Keep the first
			// failed attempt only in Failover so per-account metrics do not count
			// the same account twice.
			if event.Failover != nil {
				event.Account = ""
				event.Selection = ""
			}
			status := http.StatusServiceUnavailable
			errorType := "api_error"
			clientMessage := accountUnavailableMessage
			var rateLimit localRateLimitError
			if errors.As(err, &rateLimit) {
				status = http.StatusTooManyRequests
				errorType = "rate_limit_error"
			}
			var safeError selectionClientError
			if errors.As(err, &safeError) {
				clientMessage = safeError.ClientMessage()
			}
			event.Status = status
			event.Error = err.Error()
			slog.Warn("account selection failed", "request_id", requestID, "path", incoming.URL.Path,
				"ingress", ingress.Name, "status", status, "error", err)
			writeError(w, status, errorType, clientMessage)
			return
		}
		releaseLoad = selected.release
		releaseSession = selected.releaseSession
		event.Account = selected.Account.Alias
		event.Selection = selected.Source
		if selected.RoutingPolicy == routingPolicyQuotaAware {
			if selected.QuotaPriority.known {
				slog.Info("quota-aware account selected", "request_id", requestID, "account", selected.Account.Alias,
					"remaining_percent", selected.QuotaPriority.remaining,
					"resets_at", selected.QuotaPriority.resetUnix,
					"urgency_per_second", selected.QuotaPriority.urgency)
			} else if !route.CountTokens {
				slog.Info("quota-aware account selected for sampling", "request_id", requestID, "account", selected.Account.Alias)
			}
		}
		freshAccount, refreshErr := s.tokens.ensureFresh(incoming.Context(), selected.Account)
		if refreshErr != nil {
			slog.Warn("account authentication failed", "request_id", requestID, "path", incoming.URL.Path,
				"ingress", ingress.Name, "account", selected.Account.Alias, "error", refreshErr)
			if cooldownErr := s.store.Cooldown(incoming.Context(), selected.Account.ID, "", time.Now().Add(time.Minute), "oauth_refresh_failed"); cooldownErr != nil {
				slog.Warn("persist OAuth refresh cooldown", "request_id", requestID, "account", selected.Account.Alias, "error", cooldownErr)
			}
			if selected.Pinned || strings.TrimSpace(forcedAlias) != "" || attempt == 1 {
				event.Status = http.StatusServiceUnavailable
				event.Error = refreshErr.Error()
				writeError(w, http.StatusServiceUnavailable, "authentication_error", "selected account authentication is temporarily unavailable")
				return
			}
			releaseLoad()
			releaseLoad = nil
			if releaseSession != nil {
				releaseSession()
				releaseSession = nil
			}
			excluded[selected.Account.ID] = true
			event.Failover = &metrics.Failover{Account: selected.Account.Alias, Error: refreshErr.Error()}
			continue
		}
		selected.Account = freshAccount
		transformedBody, changed, transformErr := addSubscriptionAttribution(body, incoming.Header, selected.Account.Credential, includeMetadata, route.ConversationKey)
		if transformErr != nil {
			fail(http.StatusBadRequest, "invalid_request_error", transformErr.Error())
			return
		}
		switch {
		case changed && normalizedToolNames > 0:
			event.RelayAction = "minimal_attribution_and_tool_name_normalization"
		case changed:
			event.RelayAction = "minimal_attribution"
		case normalizedToolNames > 0:
			event.RelayAction = "tool_name_normalization"
		default:
			event.RelayAction = "unchanged"
		}
		if changed {
			slog.Info("added subscription attribution", "request_id", requestID, "path", incoming.URL.Path, "ingress", ingress.Name, "account", selected.Account.Alias)
		}
		response, err = s.doUpstream(incoming, transformedBody, selected.Account)
		if err == nil && !retryableStatus(response.StatusCode) {
			break
		}
		if err == nil {
			observedAt := time.Now()
			if decision, cool := cooldownDecisionForResponse(response.Header, response.StatusCode, route.Model, observedAt); cool {
				if cooldownErr := s.store.CooldownObservedAt(incoming.Context(), selected.Account.ID, decision.model, decision.until, decision.reason, observedAt); cooldownErr != nil {
					slog.Warn("persist upstream account cooldown", "request_id", requestID, "account", selected.Account.Alias,
						"model", decision.model, "reason", decision.reason, "until", decision.until, "error", cooldownErr)
				}
			}
			if response.StatusCode == http.StatusTooManyRequests && anthropicWindowExhausted(response.Header, "5h") {
				s.markFiveHourExhausted(incoming.Context(), requestID, selected.Account, route.Model,
					response.StatusCode, response.Header, observedAt)
			}
		}
		if selected.Pinned || strings.TrimSpace(forcedAlias) != "" || attempt == 1 {
			break
		}
		releaseLoad()
		releaseLoad = nil
		if releaseSession != nil {
			releaseSession()
			releaseSession = nil
		}
		failover := metrics.Failover{Account: selected.Account.Alias}
		if response != nil {
			failover.Status = response.StatusCode
			_ = response.Body.Close()
			response = nil
		} else if err != nil {
			failover.Error = err.Error()
		}
		event.Failover = &failover
		excluded[selected.Account.ID] = true
	}
	if err != nil {
		slog.Error("upstream request failed", "request_id", requestID, "path", incoming.URL.Path, "ingress", ingress.Name, "account", selected.Account.Alias, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		fail(http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	if response == nil {
		fail(http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	event.Status = response.StatusCode
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Warn("close upstream response", "request_id", requestID, "error", err)
		}
	}()

	// Read while the response headers are the newest thing known about the
	// account. A streaming body runs for anything up to minutes, so a window read
	// after the copy would carry the moment this response ended rather than the
	// moment upstream reported it, and two responses could be recorded in the
	// opposite order to the readings they carry.
	observedAt := time.Now()
	window, sampled := readFiveHourWindow(response.Header, observedAt)
	successful := response.StatusCode >= 200 && response.StatusCode < 300
	if successful {
		s.recoverCooldownsFromSuccess(incoming.Context(), selected.Account.ID, route.Model, response.Header, observedAt)
	}
	if incoming.URL.Path == "/v1/messages" && successful && sampled {
		// Publish the response-derived balance before streaming the body. The
		// request-level observation still waits until usage parsing has finished.
		s.sampler.observe(selected.Account, window)
	}

	contentType := response.Header.Get("Content-Type")
	var authPreview authenticationPreview
	var responseReader io.Reader = response.Body
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		responseReader = io.TeeReader(response.Body, &authPreview)
	}
	observer := accounting.NewObserver(responseReader, contentType)
	responseWriter := flushWriter{ResponseWriter: w}
	restoreJSON := len(toolNames) > 0 && strings.Contains(strings.ToLower(contentType), "json")
	restoreSSE := len(toolNames) > 0 && strings.Contains(strings.ToLower(contentType), "text/event-stream")
	adjustUsage := incoming.URL.Path == "/v1/messages" && successful && refusalOptions.Enabled
	adjustJSON := adjustUsage && strings.Contains(strings.ToLower(contentType), "json")
	adjustSSE := adjustUsage && strings.Contains(strings.ToLower(contentType), "text/event-stream")
	var adjustment *refusalResponseAdapter
	if adjustJSON || adjustSSE {
		adjustment = &refusalResponseAdapter{options: refusalOptions, requestID: requestID, nonStreaming: adjustJSON, usage: map[string]json.RawMessage{}}
	}
	var copyErr error
	var responseBody []byte
	restoredToolNames := 0
	if restoreJSON || adjustJSON {
		var reader io.Reader = observer
		if adjustJSON {
			reader = io.LimitReader(observer, maxAdaptedResponseBytes+1)
		}
		responseBody, copyErr = io.ReadAll(reader)
		if copyErr == nil && adjustJSON && len(responseBody) > maxAdaptedResponseBytes {
			copyErr = fmt.Errorf("upstream JSON response exceeds %d bytes", maxAdaptedResponseBytes)
		}
		if copyErr == nil && restoreJSON {
			responseBody, restoredToolNames, copyErr = restoreNonStreamingToolNames(responseBody, toolNames)
		}
		if copyErr == nil && adjustJSON {
			responseBody, _, copyErr = adjustment.adjust(responseBody)
		}
		if copyErr != nil {
			slog.Error("adapt upstream JSON response", "request_id", requestID,
				"ingress", ingress.Name, "account", selected.Account.Alias, "error", copyErr)
			fail(http.StatusBadGateway, "api_error", "upstream response could not be adapted")
			return
		}
	}

	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set(requestIDHeader, requestID)
	w.Header().Set("X-Claude-Relay-Account", selected.Account.Alias)
	if restoreSSE || adjustSSE || restoredToolNames > 0 || (adjustment != nil && adjustment.billing != nil) {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(response.StatusCode)
	switch {
	case restoreJSON || adjustJSON:
		_, copyErr = writeAll(responseWriter, responseBody)
	case restoreSSE || adjustSSE:
		_, restoredToolNames, copyErr = copySSEWithResponseTransforms(responseWriter, observer, toolNames, adjustment)
	default:
		_, copyErr = io.Copy(responseWriter, observer)
	}
	if adjustment != nil {
		event.RefusalBilling = adjustment.billing
	}
	if restoredToolNames > 0 {
		slog.Info("restored experimental response tool names", "request_id", requestID,
			"ingress", ingress.Name, "account", selected.Account.Alias, "names_changed", restoredToolNames)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		code, reason := claudeoauth.ErrorDiagnostics(authPreview.body)
		event.Error = fmt.Sprintf("upstream authentication rejected (code=%s reason=%s)", code, reason)
		slog.Warn("upstream authentication rejected", "request_id", requestID, "account", selected.Account.Alias,
			"path", incoming.URL.Path, "ingress", ingress.Name, "status", response.StatusCode,
			"code", code, "reason", reason, "expires_at", selected.Account.ExpiresAt,
			"last_refresh_at", selected.Account.LastRefreshAt, "body_truncated", authPreview.truncated)
	}
	observedUsage, servedModel, refusal := observer.Result(copyErr, route.Model)
	if incoming.URL.Path == "/v1/messages" && refusal.Seen {
		event.Refusal = true
		event.RefusalCategory = refusal.Category
		refusalObservedAt := time.Now()
		s.accounting.RecordRefusal(selected.Account.ID, refusalObservedAt, refusal)
		slog.Warn("upstream refused request", "request_id", requestID, "account", selected.Account.Alias,
			"model", servedModel, "category", refusal.Category)
	}
	if incoming.URL.Path == "/v1/messages" && response.StatusCode >= 200 && response.StatusCode < 300 {
		if !observedUsage.Seen && copyErr == nil {
			s.warnMissingUsage(requestID, servedModel, response)
		}
		fiveHour := accounting.FiveHourContext{
			EventKey:   requestID + ":" + strconv.FormatInt(selected.Account.ID, 10) + ":" + strconv.FormatInt(observedAt.UnixNano(), 10),
			ObservedAt: observedAt, CompletedAt: time.Now(),
			Status: response.StatusCode, UsedPercent: -1,
		}
		if sampled {
			fiveHour.ResetsAt = window.resetsAt
			fiveHour.UsedPercent = window.usedPercent
		}
		s.accounting.Record(selected.Account.ID, servedModel, ingress.Name, started, observedUsage, fiveHour)
	}
	if copyErr != nil {
		slog.Warn("relay response interrupted", "request_id", requestID, "path", incoming.URL.Path, "status", response.StatusCode, "error", copyErr)
		event.Error = "response interrupted"
		return
	}
	if response.StatusCode < 400 && !route.CountTokens && selected.RoutingPolicy == routingPolicyLegacy {
		if ttl := stickyTTLAtCompletion(route, observedAt, time.Now()); ttl > 0 {
			if bindErr := s.store.Bind(incoming.Context(), route.ConversationKey, selected.Account.ID, ttl); bindErr != nil {
				slog.Warn("persist routing affinity", "request_id", requestID, "ttl", ttl, "error", bindErr)
			}
		}
	}
	slog.Info("request completed", "request_id", requestID, "path", incoming.URL.Path, "ingress", ingress.Name, "account", selected.Account.Alias, "selection", selected.Source, "status", response.StatusCode, "duration_ms", time.Since(started).Milliseconds())
}

func recordClientObservation(event *metrics.Event, client clientObservation) {
	event.ClientClass = client.Class
	event.ClientKind = client.Kind
	event.ClassificationVersion = client.Version
	event.Client = &metrics.ClientEvidence{
		BillingBlock:       client.Evidence.BillingBlock,
		CCVersion:          client.Evidence.CCVersion,
		KnownEntrypoint:    client.Evidence.KnownEntrypoint,
		OfficialPrompt:     client.Evidence.OfficialPrompt,
		StructuredMetadata: client.Evidence.StructuredMetadata,
		ClaudeUserAgent:    client.Evidence.ClaudeUserAgent,
		ClaudeCodeSession:  client.Evidence.ClaudeCodeSession,
		XAppCLI:            client.Evidence.XAppCLI,
		AnthropicBeta:      client.Evidence.AnthropicBeta,
		AnthropicVersion:   client.Evidence.AnthropicVersion,
	}
}

// Capture a bounded preview while forwarding the original response unchanged.
// A large or incomplete third-party error is diagnosed as unknown, never dumped.
type authenticationPreview struct {
	body      []byte
	truncated bool
}

func (p *authenticationPreview) Write(data []byte) (int, error) {
	n := len(data)
	remaining := (8 << 10) - len(p.body)
	if len(data) > remaining {
		p.truncated = true
		data = data[:remaining]
	}
	p.body = append(p.body, data...)
	return n, nil
}

func (s *Server) markFiveHourExhausted(ctx context.Context, requestID string, account store.Account,
	model string, status int, headers http.Header, observedAt time.Time,
) {
	reset := normalizeResetIdentity(headers.Get(fiveHourResetHeader))
	if reset == "" {
		if aggregate, ok := anthropicAggregateReset(headers, observedAt, 6*time.Hour); ok {
			reset = strconv.FormatInt(aggregate.Unix(), 10)
		} else {
			return
		}
	}
	usedPercent := -1.0
	if reading, ok := readFiveHourWindow(headers, observedAt); ok {
		usedPercent = reading.usedPercent
	}
	event := store.FiveHourEvent{
		EventKey: "exhaustion:" + requestID + ":" + strconv.FormatInt(account.ID, 10) + ":" +
			strconv.FormatInt(observedAt.UnixNano(), 10),
		AccountID: account.ID, ResetsAt: reset, ObservedAt: observedAt.UnixMilli(),
		CompletedAt: observedAt.UnixMilli(), Model: model, Status: status,
		UsedPercent: usedPercent,
	}
	if err := s.store.MarkFiveHourExhausted(ctx, event, cooldownReasonFiveHourExhausted); err != nil {
		slog.Warn("persist five-hour exhaustion", "request_id", requestID, "account", account.Alias, "error", err)
	}
}

func stickyTTLAtCompletion(route requestRoute, observedAt, completedAt time.Time) time.Duration {
	if route.StickyTTL <= 0 {
		return 0
	}
	if isExplicitSessionRoute(route.ConversationKey) {
		return route.StickyTTL
	}
	remaining := observedAt.Add(route.StickyTTL).Sub(completedAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (s *Server) doUpstream(incoming *http.Request, body []byte, account store.Account) (*http.Response, error) {
	target := *s.upstream
	target.Path = strings.TrimRight(s.upstream.Path, "/") + incoming.URL.Path
	query := incoming.URL.Query()
	query.Set("beta", "true")
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(withAccountExit(incoming.Context(), account), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(request.Header, incoming.Header)
	// Let net/http own content negotiation so it can transparently decode gzip
	// before the response reaches usage accounting. Forwarding the caller's
	// Accept-Encoding leaves the body compressed and makes SSE metadata opaque.
	request.Header.Del("Accept-Encoding")
	request.Header.Del("x-api-key")
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if request.Header.Get("anthropic-version") == "" {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	if request.Header.Get("content-type") == "" {
		request.Header.Set("content-type", "application/json")
	}
	request.Host = s.upstream.Host
	return s.client.Do(request)
}

func (s *Server) warnMissingUsage(requestID, model string, response *http.Response) {
	now := time.Now().Unix()
	interval := int64(missingUsageWarningInterval / time.Second)
	for {
		previous := s.missingUsageWarningAt.Load()
		if previous != 0 && now-previous < interval {
			return
		}
		if s.missingUsageWarningAt.CompareAndSwap(previous, now) {
			break
		}
	}
	slog.Warn("successful Messages response contained no observable usage",
		"request_id", requestID,
		"model", model,
		"content_type", response.Header.Get("Content-Type"),
		"content_encoding", response.Header.Get("Content-Encoding"),
		"transport_uncompressed", response.Uncompressed)
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == 529 || status >= 500
}

func retryAfterHeader(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	resetAt, err := http.ParseTime(raw)
	if err != nil || !resetAt.After(now) {
		return 0, false
	}
	return resetAt.Sub(now), true
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, errorType, message)
}
