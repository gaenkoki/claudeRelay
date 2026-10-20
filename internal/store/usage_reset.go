package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Retained independently of account rows: deletion/reimport must not forget a
// possibly spent reset. No credentials or request content are recorded here.
type UsageResetOperation struct {
	RequestID        string   `json:"request_id"`
	OrganizationUUID string   `json:"-"`
	SubjectUUID      string   `json:"-"`
	AccountID        int64    `json:"-"`
	AccountUUID      string   `json:"-"`
	GrantID          string   `json:"grant_id"`
	Clears           []string `json:"clears"`
	State            string   `json:"state"`
	Result           string   `json:"result"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
}

var ErrUsageResetPending = errors.New("another usage reset has an unresolved outcome")

const usageResetColumns = `request_id,organization_uuid,subject_uuid,account_id,account_uuid,grant_id,clears_json,state,result,created_at,updated_at`

func scanUsageReset(row *sql.Row) (UsageResetOperation, bool, error) {
	var op UsageResetOperation
	var clears string
	err := row.Scan(&op.RequestID, &op.OrganizationUUID, &op.SubjectUUID, &op.AccountID, &op.AccountUUID,
		&op.GrantID, &clears, &op.State, &op.Result, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return op, false, nil
	}
	if err != nil {
		return op, false, fmt.Errorf("read usage reset operation: %w", err)
	}
	if err := json.Unmarshal([]byte(clears), &op.Clears); err != nil {
		return op, false, fmt.Errorf("decode usage reset windows: %w", err)
	}
	return op, true, nil
}

func (s *Store) UsageResetOperation(ctx context.Context, requestID string) (UsageResetOperation, bool, error) {
	return scanUsageReset(s.db.QueryRowContext(ctx, `SELECT `+usageResetColumns+` FROM usage_reset_operations WHERE request_id=?`, requestID))
}

func (s *Store) LatestUsageReset(ctx context.Context, org, subject string) (UsageResetOperation, bool, error) {
	return scanUsageReset(s.db.QueryRowContext(ctx, `SELECT `+usageResetColumns+` FROM usage_reset_operations
		WHERE organization_uuid=? AND subject_uuid=? ORDER BY CASE WHEN state='settled' THEN 1 ELSE 0 END,created_at DESC LIMIT 1`, org, subject))
}

// The unique partial index also prevents concurrent processes from starting
// different operations for one authenticated upstream identity.
func (s *Store) BeginUsageReset(ctx context.Context, op UsageResetOperation) error {
	clears, err := json.Marshal(op.Clears)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage reset transaction: %w", err)
	}
	defer tx.Rollback()
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_reset_operations
		WHERE organization_uuid=? AND subject_uuid=? AND state IN ('pending','unknown'))`, op.OrganizationUUID, op.SubjectUUID).Scan(&pending); err != nil {
		return fmt.Errorf("check pending usage reset: %w", err)
	}
	if pending {
		return ErrUsageResetPending
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_reset_operations (`+usageResetColumns+`) VALUES(?,?,?,?,?,?,?,'pending','',?,?)`,
		op.RequestID, op.OrganizationUUID, op.SubjectUUID, op.AccountID, op.AccountUUID, op.GrantID, string(clears), op.CreatedAt, op.CreatedAt)
	if err != nil {
		return fmt.Errorf("begin usage reset operation: %w", err)
	}
	return tx.Commit()
}

// Settling the operation and removing the corresponding quota cooldowns commit
// together. Unrelated overload/model cooldowns and refusal cooldowns survive.
func (s *Store) FinishUsageReset(ctx context.Context, op UsageResetOperation, state, result string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage reset completion: %w", err)
	}
	defer tx.Rollback()
	changed, err := tx.ExecContext(ctx, `UPDATE usage_reset_operations SET state=?,result=?,updated_at=?
		WHERE request_id=? AND organization_uuid=? AND subject_uuid=? AND state IN ('pending','unknown')`,
		state, result, time.Now().UnixMilli(), op.RequestID, op.OrganizationUUID, op.SubjectUUID)
	if err != nil {
		return fmt.Errorf("finish usage reset: %w", err)
	}
	rows, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("usage reset operation is no longer pending")
	}
	if state == "settled" && (result == "reset" || result == "already_used") {
		for _, window := range op.Clears {
			reason := map[string]string{
				"five_hour": "anthropic_5h_window_exhausted", "seven_day": "anthropic_7d_window_exhausted",
				"seven_day_overage_included": "anthropic_7d_oi_window_exhausted",
			}[window]
			if reason == "" {
				continue // New provider windows have no corresponding local exclusion.
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM account_cooldowns WHERE account_id=? AND reason=?
				AND EXISTS(SELECT 1 FROM accounts WHERE id=? AND account_uuid=?)`, op.AccountID, reason, op.AccountID, op.AccountUUID); err != nil {
				return fmt.Errorf("clear reset quota cooldown: %w", err)
			}
		}
	}
	return tx.Commit()
}
