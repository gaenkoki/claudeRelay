package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

var resetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
var resetRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var resetUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

const resetRetryWindow = 10 * time.Minute

type usageResetGrant struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	ResetsTotal      *int     `json:"resets_total"`
	ResetsLeft       *int     `json:"resets_left"`
	StartsAt         string   `json:"starts_at"`
	EndsAt           string   `json:"ends_at"`
	Clears           []string `json:"clears"`
	Paused           bool     `json:"paused"`
	UsableNow        bool     `json:"usable_now"`
	UseRequiresLimit *bool    `json:"use_requires_limit"`
	BlockedReason    string   `json:"blocked_reason,omitempty"`
}

type usageResetStatus struct {
	Eligible         *bool             `json:"eligible"`
	IneligibleReason string            `json:"ineligible_reason"`
	AtLimit          bool              `json:"at_limit"`
	CooldownUntil    string            `json:"cooldown_until"`
	NextGrantID      string            `json:"next_grant_id"`
	Grants           []usageResetGrant `json:"grants"`
}

type usageResetIdentity struct {
	Organization string
	Subject      string
}

// Only fixed local errors are exposed. Provider error bodies can contain
// personal data; neither errors nor the durable spending record include them.
func (s *Server) resetHTTP(ctx context.Context, account store.Account, method, path string, payload any) ([]byte, int, error) {
	relative, err := url.Parse(path)
	if err != nil {
		return nil, 0, err
	}
	target := *s.upstream
	target.Path = strings.TrimRight(target.Path, "/") + relative.Path
	target.RawQuery = relative.RawQuery
	var body []byte
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
	}
	r, err := http.NewRequestWithContext(withAccountExit(ctx, account), method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	r.Header.Set("Authorization", "Bearer "+account.AccessToken)
	r.Header.Set("anthropic-beta", "oauth-2025-04-20")
	r.Header.Set("User-Agent", "claude-cli/"+defaultClaudeCodeVersion+" (external, cli)")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	client := *s.client
	// A redirect must never replay a spending POST or forward account credentials.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(r)
	if err != nil {
		slog.Warn("usage reset transport failed", "account", account.Alias, "method", method, "error", err)
		return nil, 0, fmt.Errorf("重置卡接口连接失败或超时")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, accountUsageMaxBody+1))
	if err != nil || len(data) > accountUsageMaxBody {
		slog.Warn("usage reset response read failed", "account", account.Alias, "method", method, "bytes", len(data), "error", err)
		return nil, response.StatusCode, fmt.Errorf("重置卡接口响应不完整或过大")
	}
	return data, response.StatusCode, nil
}

func (s *Server) resetAccount(ctx context.Context, alias string) (store.Account, error) {
	account, found, err := s.store.AccountByAlias(ctx, alias)
	if err != nil {
		return account, err
	}
	if !found {
		return account, fmt.Errorf("账号不存在")
	}
	if account.Enabled {
		return s.tokens.ensureFresh(ctx, account)
	}
	if account.Credential.IsExpired(time.Now()) {
		return account, fmt.Errorf("停用账号的令牌已过期，请先启用或重新授权")
	}
	return account, nil
}

func (s *Server) resetIdentity(ctx context.Context, account store.Account) (usageResetIdentity, error) {
	body, status, err := s.resetHTTP(ctx, account, http.MethodGet, "/api/oauth/profile", nil)
	if err != nil {
		return usageResetIdentity{}, err
	}
	if status != http.StatusOK {
		return usageResetIdentity{}, fmt.Errorf("读取重置卡账号身份失败（HTTP %d）", status)
	}
	var profile struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &profile); err != nil || !resetUUIDPattern.MatchString(profile.Account.UUID) || !resetUUIDPattern.MatchString(profile.Organization.UUID) {
		return usageResetIdentity{}, fmt.Errorf("账号资料缺少有效的真实账号或组织 ID，未提交重置")
	}
	return usageResetIdentity{Organization: strings.ToLower(profile.Organization.UUID), Subject: strings.ToLower(profile.Account.UUID)}, nil
}

func (s *Server) resetStatus(ctx context.Context, account store.Account) (usageResetStatus, error) {
	body, status, err := s.resetHTTP(ctx, account, http.MethodGet, "/api/oauth/usage?cedar_ember=1&skip_spend=1", nil)
	if err != nil {
		return usageResetStatus{}, err
	}
	if status != http.StatusOK {
		return usageResetStatus{}, fmt.Errorf("读取重置卡失败（HTTP %d）", status)
	}
	var payload struct {
		Status *usageResetStatus `json:"cedar_ember"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Status == nil || payload.Status.Eligible == nil {
		return usageResetStatus{}, fmt.Errorf("上游未返回有效的重置卡信息，未提交重置")
	}
	value := *payload.Status
	seen := make(map[string]bool)
	if value.CooldownUntil != "" {
		if _, err := time.Parse(time.RFC3339Nano, value.CooldownUntil); err != nil {
			return value, fmt.Errorf("上游重置卡冷却时间无效")
		}
	}
	for i := range value.Grants {
		grant := &value.Grants[i]
		if !resetGrantIDPattern.MatchString(grant.ID) || seen[grant.ID] || grant.ResetsTotal == nil || grant.ResetsLeft == nil || *grant.ResetsLeft < 0 || *grant.ResetsTotal < *grant.ResetsLeft {
			return value, fmt.Errorf("上游重置卡条目无效")
		}
		seen[grant.ID] = true
		for _, stamp := range []string{grant.StartsAt, grant.EndsAt} {
			if stamp != "" {
				if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
					return value, fmt.Errorf("上游重置卡有效期无效")
				}
			}
		}
		grant.BlockedReason = resetGrantBlocker(value, *grant, time.Now())
	}
	if value.NextGrantID != "" && !seen[value.NextGrantID] {
		return value, fmt.Errorf("上游指定了未知的优先重置卡")
	}
	return value, nil
}

func resetGrantBlocker(status usageResetStatus, grant usageResetGrant, now time.Time) string {
	if status.Eligible == nil || !*status.Eligible {
		return "该账号不符合重置条件"
	}
	if grant.Paused || !grant.UsableNow {
		return "此卡暂不可用"
	}
	if grant.ResetsLeft == nil || *grant.ResetsLeft == 0 {
		return "此卡已用完"
	}
	if len(grant.Clears) == 0 {
		return "上游未明确提供重置范围"
	}
	if status.NextGrantID != "" && status.NextGrantID != grant.ID {
		return "官方要求先使用另一张卡片"
	}
	// The private provider response may omit this flag. Never infer permission
	// to spend from its absence: this is surfaced as an explicit blocked reason.
	if grant.UseRequiresLimit == nil {
		return "上游未明确提供使用条件"
	}
	if *grant.UseRequiresLimit && !status.AtLimit {
		return "需要触发额度限制后才能使用"
	}
	if grant.StartsAt != "" {
		if starts, _ := time.Parse(time.RFC3339Nano, grant.StartsAt); starts.After(now) {
			return "尚未生效"
		}
	}
	if grant.EndsAt != "" {
		if ends, _ := time.Parse(time.RFC3339Nano, grant.EndsAt); !ends.After(now) {
			return "已过期"
		}
	}
	if status.CooldownUntil != "" {
		if until, _ := time.Parse(time.RFC3339Nano, status.CooldownUntil); until.After(now) {
			return "官方重置冷却中"
		}
	}
	return ""
}

func (s *Server) claimUsageReset(ctx context.Context, account store.Account, op store.UsageResetOperation) (string, error) {
	body, status, err := s.resetHTTP(ctx, account, http.MethodPost, "/api/organizations/"+op.OrganizationUUID+"/reset_rate_limits", map[string]string{
		"program": "cedar_ember", "grant_id": op.GrantID, "request_id": op.RequestID,
	})
	if err != nil {
		return "", err
	}
	if status == 401 || status == 403 {
		return "auth_error", nil
	}
	if status == 429 {
		return "rate_limited", nil
	}
	var response struct {
		Result string `json:"result"`
	}
	if status != 200 || json.Unmarshal(body, &response) != nil {
		return "", fmt.Errorf("重置响应无法确认（HTTP %d），请勿改用新的操作 ID", status)
	}
	switch response.Result {
	case "reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable":
		return response.Result, nil
	default:
		return "", fmt.Errorf("上游返回了未知的重置结果")
	}
}
