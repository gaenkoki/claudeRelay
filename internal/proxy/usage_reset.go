package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func (s *Server) listUsageResets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), accountUsageTimeout)
	defer cancel()
	account, err := s.resetAccount(ctx, strings.TrimSpace(r.PathValue("alias")))
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	identity, err := s.resetIdentity(ctx, account)
	if err != nil {
		writeError(w, 502, "api_error", err.Error())
		return
	}
	op, found, err := s.store.LatestUsageReset(ctx, identity.Organization, identity.Subject)
	if err != nil {
		writeError(w, 500, "api_error", "读取重置操作记录失败")
		return
	}
	var latest *store.UsageResetOperation
	if found {
		latest = &op
	}
	status, err := s.resetStatus(ctx, account)
	if err != nil {
		if !found {
			writeError(w, 502, "api_error", err.Error())
			return
		}
		// Provider grant data may fail independently after a claim. Preserve
		// access to the durable receipt, but expose the failure and offer no cards.
		slog.Warn("usage reset status unavailable while reading receipt", "account", account.Alias, "error", err)
		writeJSON(w, 200, map[string]any{"status": nil, "operation": latest, "status_error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": status, "operation": latest, "retry_window_seconds": int(resetRetryWindow.Seconds())})
}

func (s *Server) useUsageReset(w http.ResponseWriter, r *http.Request) {
	var request struct {
		GrantID     string `json:"grant_id"`
		RequestID   string `json:"request_id"`
		Confirmed   bool   `json:"confirmed"`
		ResolveOnly bool   `json:"resolve_only"`
	}
	if err := decodeAdminJSON(w, r, &request); err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if !request.Confirmed || !resetRequestIDPattern.MatchString(request.RequestID) || !resetGrantIDPattern.MatchString(request.GrantID) {
		writeError(w, 400, "invalid_request_error", "需要确认操作并提供有效的卡片和操作 ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountUsageTimeout)
	defer cancel()
	account, err := s.resetAccount(ctx, strings.TrimSpace(r.PathValue("alias")))
	if err != nil {
		writeError(w, 400, "invalid_request_error", err.Error())
		return
	}
	identity, err := s.resetIdentity(ctx, account)
	if err != nil {
		writeError(w, 502, "api_error", err.Error())
		return
	}
	lockValue, _ := s.resetLocks.LoadOrStore(identity.Organization+":"+identity.Subject, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	op, found, err := s.store.UsageResetOperation(ctx, request.RequestID)
	if err != nil {
		writeError(w, 500, "api_error", "读取重置操作记录失败")
		return
	}
	if found && (op.OrganizationUUID != identity.Organization || op.SubjectUUID != identity.Subject || op.GrantID != request.GrantID) {
		writeError(w, 409, "invalid_request_error", "操作 ID 已用于其他账号或卡片")
		return
	}
	if found && op.State == "settled" {
		writeJSON(w, 200, map[string]any{"operation": op})
		return
	}
	if request.ResolveOnly {
		if !found {
			writeError(w, 404, "not_found_error", "没有可核实的重置操作")
			return
		}
		if err := s.store.FinishUsageReset(ctx, op, "settled", "manually_resolved"); err != nil {
			writeError(w, 500, "api_error", "保存人工核实结果失败")
			return
		}
		slog.Warn("usage reset outcome manually acknowledged", "account", account.Alias, "operation_id", op.RequestID)
		op.State, op.Result = "settled", "manually_resolved"
		writeJSON(w, 200, map[string]any{"operation": op})
		return
	}
	if found && time.Since(time.UnixMilli(op.CreatedAt)) >= resetRetryWindow {
		writeError(w, 409, "invalid_request_error", "该操作已超过安全重试时间，请到官方核实结果，再解除待确认状态")
		return
	}
	if !found {
		status, err := s.resetStatus(ctx, account)
		if err != nil {
			writeError(w, 502, "api_error", err.Error())
			return
		}
		var grant *usageResetGrant
		for i := range status.Grants {
			if status.Grants[i].ID == request.GrantID {
				grant = &status.Grants[i]
				break
			}
		}
		if grant == nil {
			writeError(w, 409, "invalid_request_error", "此卡已不存在，请重新查询")
			return
		}
		if reason := resetGrantBlocker(status, *grant, time.Now()); reason != "" {
			writeError(w, 409, "invalid_request_error", reason)
			return
		}
		op = store.UsageResetOperation{RequestID: request.RequestID, OrganizationUUID: identity.Organization,
			SubjectUUID: identity.Subject, AccountID: account.ID, AccountUUID: account.AccountUUID,
			GrantID: grant.ID, Clears: grant.Clears, State: "pending", CreatedAt: time.Now().UnixMilli()}
		if err := s.store.BeginUsageReset(ctx, op); err != nil {
			if errors.Is(err, store.ErrUsageResetPending) {
				writeError(w, 409, "invalid_request_error", "该账号还有结果待确认的操作，请先查询并核实")
				return
			}
			slog.Error("persist usage reset before dispatch", "account", account.Alias, "error", err)
			writeError(w, 500, "api_error", "保存重置操作失败，未提交兑换")
			return
		}
	}
	// Once durably recorded, finish even if the browser disconnects. Spending
	// POSTs are never automatically retried; manual retries retain the same ID.
	claimCtx, stopClaim := context.WithTimeout(context.WithoutCancel(r.Context()), 25*time.Second)
	result, claimErr := s.claimUsageReset(claimCtx, account, op)
	stopClaim()
	state := "settled"
	if claimErr != nil || (found && result != "reset" && result != "already_used") {
		// A refusal to a retry does not disprove spending by the earlier POST.
		state = "unknown"
	}
	saveCtx, stopSave := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	err = s.store.FinishUsageReset(saveCtx, op, state, result)
	stopSave()
	if err != nil {
		slog.Error("persist usage reset outcome", "account", account.Alias, "operation_id", op.RequestID, "error", err)
		writeError(w, 500, "api_error", "兑换可能已完成，但本地记录保存失败，请查询原操作，不要另起兑换")
		return
	}
	op.State, op.Result, op.UpdatedAt = state, result, time.Now().UnixMilli()
	level := slog.LevelInfo
	if state == "unknown" {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "usage reset outcome", "account", account.Alias, "operation_id", op.RequestID, "state", state, "result", result, "error", claimErr)
	warning := ""
	if state == "unknown" {
		warning = "结果尚未确认，可能已消耗卡片。请仅重试原操作，或到官方核实后解除待确认状态。"
	}
	if state == "settled" && (result == "reset" || result == "already_used") {
		warning = s.refreshAfterUsageReset(r.Context(), account, op.RequestID)
	}
	writeJSON(w, 200, map[string]any{"operation": op, "warning": warning})
}

func (s *Server) refreshAfterUsageReset(ctx context.Context, account store.Account, operationID string) string {
	s.sampler.forget(account)
	s.usage.cacheMu.Lock()
	delete(s.usage.cache, accountUsageCacheKey(account))
	s.usage.cacheMu.Unlock()
	view, err := s.usage.get(ctx, account, true)
	if err == nil {
		err = s.recordAccountUsageObservation(ctx, account, view, operationID)
	}
	if err != nil {
		slog.Warn("refresh quota after usage reset", "account", account.Alias, "error", err)
		return "重置结果已确认，但额度刷新失败，请手动刷新订阅额度。"
	}
	return ""
}
