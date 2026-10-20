package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

const (
	accountHeader         = "X-Claude-Relay-Account"
	sessionStickyTTL      = time.Hour
	defaultCacheStickyTTL = 5 * time.Minute
	sessionRoutePrefix    = "session:"
)

type requestRoute struct {
	// ConversationKey remains a stable attribution seed for every request.
	// Selectors consult or persist it only when StickyTTL is positive.
	ConversationKey string
	SelectionKey    string
	// StickyTTL is one hour for an explicit client session, the declared cache
	// lifetime for a cache prefix, and zero for a content-only fallback.
	StickyTTL   time.Duration
	AccountUUID string
	Model       string
	// Ingress scopes routing keys so policies that transform the same client
	// request cannot share sticky state. AccountAccess independently controls
	// which stored account pools may serve the request.
	Ingress       string
	AccountAccess store.AccountAccess
	Client        clientObservation
	// CountTokens selects the independent short-request admission pool and
	// never consumes an active client-session slot.
	CountTokens bool
}

type metadataIdentity struct {
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func deriveRequestRoute(body []byte, headers http.Header, ingress ingressPolicy, path string) (requestRoute, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return requestRoute{}, fmt.Errorf("decode request body: %w", err)
	}
	if root == nil {
		return requestRoute{}, fmt.Errorf("request body must be a JSON object")
	}
	route := requestRoute{Ingress: ingress.Name, AccountAccess: ingress.AccountAccess, CountTokens: path == "/v1/messages/count_tokens"}
	route.Model, _ = root["model"].(string)
	route.Client = classifyClient(root, headers, path)

	identity := parseMetadataIdentity(root["metadata"])
	route.AccountUUID = identity.AccountUUID
	session := firstSessionHeader(headers)
	if session == "" {
		session = identity.SessionID
	}
	// Routing keys stay scoped per ingress so different policies cannot collide
	// on a sticky binding.
	scope := shortHash(ingress.Name)
	if session != "" {
		route.ConversationKey = sessionRoutePrefix + shortHash(scope+"\x00"+session)
		route.StickyTTL = sessionStickyTTL
	}

	prefix, cacheTTL := cachePrefix(root)
	if prefix == nil {
		prefix = ordinaryAnchor(root)
	}
	// Request-level automatic caching has no explicit content breakpoint. Keep
	// the stable ordinary anchor, but still honor its declared cache lifetime.
	if topLevelTTL := declaredCacheStickyTTL(root); topLevelTTL > cacheTTL {
		cacheTTL = topLevelTTL
	}
	raw, err := json.Marshal(prefix)
	if err != nil {
		return requestRoute{}, fmt.Errorf("encode routing prefix: %w", err)
	}
	route.SelectionKey = "prefix:" + shortHash(scope+"\x00"+route.Model+"\x00"+string(raw))
	if route.ConversationKey == "" {
		route.ConversationKey = route.SelectionKey
		route.StickyTTL = cacheTTL
	}
	return route, nil
}

func isExplicitSessionRoute(routeKey string) bool {
	return strings.HasPrefix(routeKey, sessionRoutePrefix)
}

func parseMetadataIdentity(value any) metadataIdentity {
	metadata, ok := value.(map[string]any)
	if !ok {
		return metadataIdentity{}
	}
	userID, _ := metadata["user_id"].(string)
	var identity metadataIdentity
	_ = json.Unmarshal([]byte(userID), &identity)
	return identity
}

func firstSessionHeader(headers http.Header) string {
	for _, name := range sessionHeaderNames {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func cachePrefix(root map[string]any) (any, time.Duration) {
	tools, _ := root["tools"].([]any)
	system := normalizeRoutingBlocks(root["system"])
	messages, _ := root["messages"].([]any)

	if prefix, ok := messagePrefixThroughCacheControl(messages); ok {
		return map[string]any{"tools": tools, "system": system, "messages": prefix},
			maxCacheStickyTTL(tools, system, prefix)
	}
	if prefix, ok := blocksThroughCacheControl(system); ok {
		return map[string]any{"tools": tools, "system": prefix}, maxCacheStickyTTL(tools, prefix, nil)
	}
	if prefix, ok := blocksThroughCacheControl(tools); ok {
		return map[string]any{"tools": prefix}, maxCacheStickyTTL(prefix, nil, nil)
	}
	return nil, 0
}

// maxCacheStickyTTL returns the longest declared lifetime among the cache
// breakpoints whose content contributes to one routing prefix. Anthropic's
// omitted ttl is the five-minute default; only an explicit 1h declaration
// extends account affinity to an hour.
func maxCacheStickyTTL(tools, system, messages []any) time.Duration {
	longest := time.Duration(0)
	visit := func(value any) {
		if ttl := declaredCacheStickyTTL(value); ttl > longest {
			longest = ttl
		}
	}
	for _, block := range tools {
		visit(block)
	}
	for _, block := range system {
		visit(block)
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"].([]any); ok {
			for _, block := range content {
				visit(block)
			}
			continue
		}
		visit(message["content"])
	}
	return longest
}

func declaredCacheStickyTTL(value any) time.Duration {
	block, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	raw, exists := block["cache_control"]
	if !exists {
		return 0
	}
	cacheControl, _ := raw.(map[string]any)
	ttl, _ := cacheControl["ttl"].(string)
	if strings.EqualFold(strings.TrimSpace(ttl), "1h") {
		return time.Hour
	}
	return defaultCacheStickyTTL
}

func ordinaryAnchor(root map[string]any) any {
	anchor := map[string]any{
		"tools":  root["tools"],
		"system": root["system"],
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || message["role"] != "user" {
				continue
			}
			anchor["first_user"] = message["content"]
			break
		}
	}
	return anchor
}

func normalizeRoutingBlocks(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case string:
		return []any{typed}
	default:
		return nil
	}
}

func blocksThroughCacheControl(blocks []any) ([]any, bool) {
	last := -1
	for i, block := range blocks {
		if hasCacheControl(block) {
			last = i
		}
	}
	if last < 0 {
		return nil, false
	}
	return blocks[:last+1], true
}

func messagePrefixThroughCacheControl(messages []any) ([]any, bool) {
	lastMessage, lastBlock := -1, -1
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			if hasCacheControl(message["content"]) {
				lastMessage, lastBlock = i, -1
			}
			continue
		}
		for j, block := range content {
			if hasCacheControl(block) {
				lastMessage, lastBlock = i, j
			}
		}
	}
	if lastMessage < 0 {
		return nil, false
	}
	prefix := append([]any(nil), messages[:lastMessage+1]...)
	if lastBlock >= 0 {
		message := cloneMap(messages[lastMessage].(map[string]any))
		content := message["content"].([]any)
		message["content"] = content[:lastBlock+1]
		prefix[lastMessage] = message
	}
	return prefix, true
}

func cloneMap(source map[string]any) map[string]any {
	destination := make(map[string]any, len(source))
	for key, value := range source {
		destination[key] = value
	}
	return destination
}

func hasCacheControl(value any) bool {
	return declaredCacheStickyTTL(value) > 0
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

type selection struct {
	Account        store.Account
	Pinned         bool
	Source         string
	RoutingPolicy  string
	QuotaPriority  quotaPriority
	release        func()
	releaseSession func()
}

type accountSelector struct {
	store                         *store.Store
	load                          *accountLoadTracker
	countTokensLoad               *accountLoadTracker
	concurrency                   *accountConcurrencyPolicy
	maxCountTokensInflightPerAcct int
	sessions                      *sessionAdmissionTracker
	sampler                       *subscriptionSampler
	policy                        *routingPolicyState
}

type selectionClientError interface {
	ClientMessage() string
}

type safeSelectionError struct {
	diagnostic    string
	clientMessage string
}

func (e safeSelectionError) Error() string         { return e.diagnostic }
func (e safeSelectionError) ClientMessage() string { return e.clientMessage }

type localRateLimitError struct{ safeSelectionError }

const (
	relayCapacityClientMessage = "relay capacity limit reached; retry later"
	accountUnavailableMessage  = "no account is currently available; retry later"
)

func selectionFailed(clientMessage, format string, args ...any) error {
	return safeSelectionError{diagnostic: fmt.Sprintf(format, args...), clientMessage: clientMessage}
}

func (s accountSelector) loadPolicy(route requestRoute) (*accountLoadTracker, int) {
	if route.CountTokens {
		return s.countTokensLoad, s.maxCountTokensInflightPerAcct
	}
	return s.load, s.concurrency.current()
}

func (s accountSelector) makeSelection(route requestRoute, account store.Account, source string, pinned bool, policy string) selection {
	release := func() {}
	load, _ := s.loadPolicy(route)
	if load != nil {
		release = load.reserve(account.ID)
	}
	return selection{
		Account:       account,
		Pinned:        pinned,
		Source:        source,
		RoutingPolicy: policy,
		release:       release,
	}
}

func (s accountSelector) makeLimitedSelection(route requestRoute, account store.Account, source string, pinned bool, policy string) (selection, bool) {
	load, limit := s.loadPolicy(route)
	if load == nil {
		return s.makeSelection(route, account, source, pinned, policy), true
	}
	release, _, ok := load.reserveBelow(account.ID, limit)
	if !ok {
		return selection{}, false
	}
	return selection{Account: account, Pinned: pinned, Source: source, RoutingPolicy: policy, release: release}, true
}

func (s accountSelector) routingPolicy() string {
	if s.policy == nil {
		return routingPolicyLegacy
	}
	return s.policy.current()
}

func (s accountSelector) wasBoundTo(ctx context.Context, route requestRoute, accountID int64) (bool, error) {
	if route.StickyTTL <= 0 || route.ConversationKey == "" {
		return false, nil
	}
	bound, found, err := s.store.BoundAccount(ctx, route.ConversationKey, route.AccountAccess, time.Now())
	if err != nil {
		return false, err
	}
	return found && bound.ID == accountID, nil
}

func (s accountSelector) admitSession(ctx context.Context, route requestRoute, selected selection, existing bool) (selection, bool, error) {
	if route.CountTokens || s.sessions == nil {
		selected.releaseSession = func() {}
		return selected, true, nil
	}
	release, admitted, err := s.sessions.reserve(ctx, route.ConversationKey, selected.Account.ID, existing)
	if err != nil || !admitted {
		if selected.release != nil {
			selected.release()
		}
		return selection{}, false, err
	}
	selected.releaseSession = release
	return selected, true, nil
}

func locallyRateLimited(clientMessage, format string, args ...any) error {
	return localRateLimitError{safeSelectionError{
		diagnostic:    fmt.Sprintf(format, args...),
		clientMessage: clientMessage,
	}}
}

func (s accountSelector) selectAccount(ctx context.Context, route requestRoute, forcedAlias string, excluded map[int64]bool) (selection, error) {
	policy := s.routingPolicy()
	forcedAlias = strings.TrimSpace(forcedAlias)
	if forcedAlias != "" {
		account, found, err := s.store.AccountByAlias(ctx, forcedAlias)
		if err != nil {
			return selection{}, err
		}
		if !found || !account.Enabled {
			return selection{}, selectionFailed("requested account is unavailable", "requested account %q is unavailable", forcedAlias)
		}
		if !store.AccountAccessMayUse(route.AccountAccess, account.Pool) {
			return selection{}, selectionFailed("requested account cannot serve this traffic",
				"requested account %q is in the %s pool and cannot serve %s traffic",
				forcedAlias, account.Pool, route.Ingress)
		}
		cooling, err := s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
		if err != nil {
			return selection{}, err
		}
		if cooling {
			return selection{}, selectionFailed("requested account is temporarily cooling down",
				"requested account %q is temporarily cooling down", forcedAlias)
		}
		selected, ok := s.makeLimitedSelection(route, account, "header", true, policy)
		if !ok {
			return selection{}, locallyRateLimited("requested account reached its in-flight limit",
				"requested account %q reached its in-flight limit", forcedAlias)
		}
		if policy == routingPolicyLegacy {
			existing, err := s.wasBoundTo(ctx, route, account.ID)
			if err != nil {
				selected.release()
				return selection{}, err
			}
			var admitted bool
			selected, admitted, err = s.admitSession(ctx, route, selected, existing)
			if err != nil {
				return selection{}, err
			}
			if !admitted {
				return selection{}, locallyRateLimited("requested account reached its active-session limit",
					"requested account %q reached its active-session limit", forcedAlias)
			}
		} else {
			selected.releaseSession = func() {}
		}
		return selected, nil
	}

	if policy == routingPolicyQuotaAware {
		return s.selectQuotaAware(ctx, route, excluded)
	}

	if route.AccountUUID != "" {
		account, found, err := s.store.AccountByUUID(ctx, route.AccountUUID, route.AccountAccess)
		if err != nil {
			return selection{}, err
		}
		cooling := false
		if found {
			cooling, err = s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
			if err != nil {
				return selection{}, err
			}
		}
		if found && !cooling && !excluded[account.ID] {
			selected, ok := s.makeLimitedSelection(route, account, "account_uuid", false, routingPolicyLegacy)
			if !ok {
				found = false
			} else {
				existing, boundErr := s.wasBoundTo(ctx, route, account.ID)
				if boundErr != nil {
					selected.release()
					return selection{}, boundErr
				}
				selected, admitted, admissionErr := s.admitSession(ctx, route, selected, existing)
				if admissionErr != nil {
					return selection{}, admissionErr
				}
				if admitted {
					return selected, nil
				}
				found = false
			}
		}
	}

	if route.ConversationKey != "" && s.sessions != nil {
		if accountID, pending := s.sessions.pendingAccount(route.ConversationKey); pending && !excluded[accountID] {
			account, found, err := s.store.AccountByID(ctx, accountID)
			if err != nil {
				return selection{}, err
			}
			if found && account.Enabled && store.AccountAccessMayUse(route.AccountAccess, account.Pool) {
				cooling, coolingErr := s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
				if coolingErr != nil {
					return selection{}, coolingErr
				}
				if !cooling {
					selected, ok := s.makeLimitedSelection(route, account, "session_pending", false, routingPolicyLegacy)
					if !ok {
						return selection{}, locallyRateLimited(relayCapacityClientMessage,
							"account %q reached its in-flight limit", account.Alias)
					}
					selected, admitted, admissionErr := s.admitSession(ctx, route, selected, false)
					if admissionErr != nil {
						return selection{}, admissionErr
					}
					if admitted {
						return selected, nil
					}
				}
			}
		}
	}

	if route.StickyTTL > 0 && route.ConversationKey != "" {
		account, found, err := s.store.BoundAccount(ctx, route.ConversationKey, route.AccountAccess, time.Now())
		if err != nil {
			return selection{}, err
		}
		cooling := false
		if found {
			cooling, err = s.store.IsCooling(ctx, account.ID, route.Model, time.Now())
			if err != nil {
				return selection{}, err
			}
		}
		if found && !cooling && !excluded[account.ID] {
			selected, ok := s.makeLimitedSelection(route, account, "sticky", false, routingPolicyLegacy)
			if !ok {
				return selection{}, locallyRateLimited(relayCapacityClientMessage,
					"account %q reached its in-flight limit", account.Alias)
			}
			selected, admitted, admissionErr := s.admitSession(ctx, route, selected, true)
			if admissionErr != nil {
				return selection{}, admissionErr
			}
			if admitted {
				return selected, nil
			}
		}
	}

	accounts, err := s.store.Accounts(ctx, route.AccountAccess, route.Model, time.Now())
	if err != nil {
		return selection{}, err
	}
	localExcluded := make(map[int64]bool, len(excluded)+len(accounts))
	for id, skip := range excluded {
		localExcluded[id] = skip
	}
	load, limit := s.loadPolicy(route)
	if load != nil {
		for {
			account, release, found := load.reserveLeastLoaded(accounts, route.SelectionKey, localExcluded, limit)
			if !found {
				break
			}
			selected, admitted, admissionErr := s.admitSession(ctx, route, selection{
				Account:       account,
				Source:        "load_balance",
				RoutingPolicy: routingPolicyLegacy,
				release:       release,
			}, false)
			if admissionErr != nil {
				return selection{}, admissionErr
			}
			if admitted {
				return selected, nil
			}
			localExcluded[account.ID] = true
		}
	}

	if load == nil {
		for {
			var chosen *store.Account
			var chosenScore [32]byte
			for i := range accounts {
				account := accounts[i]
				if localExcluded[account.ID] {
					continue
				}
				score := accountSelectionScore(route.SelectionKey, account.Alias)
				if chosen == nil || bytes.Compare(score[:], chosenScore[:]) > 0 {
					chosen, chosenScore = &account, score
				}
			}
			if chosen == nil {
				break
			}
			selected, admitted, admissionErr := s.admitSession(ctx, route, s.makeSelection(route, *chosen, "cache_affinity", false, routingPolicyLegacy), false)
			if admissionErr != nil {
				return selection{}, admissionErr
			}
			if admitted {
				return selected, nil
			}
			localExcluded[chosen.ID] = true
		}
	}
	for _, account := range accounts {
		if !excluded[account.ID] {
			return selection{}, locallyRateLimited(relayCapacityClientMessage,
				"all eligible accounts reached a local session or in-flight limit")
		}
	}
	return selection{}, selectionFailed(accountUnavailableMessage,
		"no healthy Claude subscription account is available for the %s ingress", route.Ingress)
}

func (s accountSelector) selectQuotaAware(ctx context.Context, route requestRoute, excluded map[int64]bool) (selection, error) {
	accounts, err := s.store.Accounts(ctx, route.AccountAccess, route.Model, time.Now())
	if err != nil {
		return selection{}, err
	}
	localExcluded := make(map[int64]bool, len(excluded))
	for id, skip := range excluded {
		localExcluded[id] = skip
	}

	load, limit := s.loadPolicy(route)
	if load == nil {
		return selection{}, selectionFailed(accountUnavailableMessage,
			"quota-aware routing requires an account load tracker")
	}
	if route.CountTokens {
		account, release, found := load.reserveLeastLoaded(accounts, route.SelectionKey, localExcluded, limit)
		if found {
			return selection{Account: account, Source: "quota_count_tokens", RoutingPolicy: routingPolicyQuotaAware,
				release: release, releaseSession: func() {}}, nil
		}
		return s.quotaSelectionFailure(accounts, excluded, route)
	}
	if s.sampler == nil {
		return selection{}, selectionFailed(accountUnavailableMessage,
			"quota-aware routing requires a five-hour usage sampler")
	}

	now := time.Now()
	priorities := make(map[int64]quotaPriority, len(accounts))
	quotaCandidates := make([]store.Account, 0, len(accounts))
	for _, account := range accounts {
		reading, ok := s.sampler.current(account, now)
		if !ok {
			priorities[account.ID] = quotaPriority{}
			quotaCandidates = append(quotaCandidates, account)
			continue
		}
		reset, parseErr := strconv.ParseInt(reading.resetsAt, 10, 64)
		if parseErr != nil || reset <= now.Unix() {
			priorities[account.ID] = quotaPriority{}
			quotaCandidates = append(quotaCandidates, account)
			continue
		}
		remaining := math.Max(0, math.Min(100, 100-reading.usedPercent))
		priorities[account.ID] = quotaPriority{
			known: true, remaining: remaining, resetUnix: reset,
			urgency: remaining / float64(reset-now.Unix()),
		}
		if remaining > 0 {
			quotaCandidates = append(quotaCandidates, account)
		}
	}
	account, priority, release, found := load.reserveQuotaAware(quotaCandidates, priorities, route.SelectionKey, localExcluded, limit)
	if found {
		source := "quota_aware"
		if !priority.known {
			source = "quota_probe"
		}
		return selection{Account: account, Source: source, RoutingPolicy: routingPolicyQuotaAware,
			QuotaPriority: priority, release: release, releaseSession: func() {}}, nil
	}
	return s.quotaSelectionFailure(quotaCandidates, excluded, route)
}

func (s accountSelector) quotaSelectionFailure(accounts []store.Account, excluded map[int64]bool, route requestRoute) (selection, error) {
	for _, account := range accounts {
		if !excluded[account.ID] {
			return selection{}, locallyRateLimited(relayCapacityClientMessage,
				"all eligible accounts reached a local in-flight limit")
		}
	}
	return selection{}, selectionFailed(accountUnavailableMessage,
		"no healthy Claude subscription account is available for the %s ingress", route.Ingress)
}
