package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RefusalCooldown contains no caller identity or conversation content.
type RefusalCooldown struct {
	Strikes     int
	UntilAt     int64
	RetainUntil int64
	Category    string
	Match       string
}

// RefusalCooldown checks explicit sessions and every complete message prefix.
// json_each avoids SQLite's bind-variable limit for long conversations.
func (s *Store) RefusalCooldown(ctx context.Context, fingerprints []string, now time.Time) (RefusalCooldown, error) {
	keys, err := json.Marshal(fingerprints)
	if err != nil {
		return RefusalCooldown{}, fmt.Errorf("encode refusal fingerprints: %w", err)
	}
	return readRefusalCooldown(ctx, s.db, string(keys), now.UnixMilli())
}

type refusalCooldownReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRefusalCooldown(ctx context.Context, db refusalCooldownReader, keys string, now int64) (RefusalCooldown, error) {
	var state RefusalCooldown
	err := db.QueryRowContext(ctx, `SELECT strikes,until_at,retain_until,category,
		CASE WHEN fingerprint LIKE 'session:%' THEN 'session' ELSE 'conversation' END FROM refusal_cooldowns
		WHERE fingerprint IN (SELECT value FROM json_each(?)) AND retain_until>?
		ORDER BY until_at DESC, strikes DESC LIMIT 1`, keys, now).
		Scan(&state.Strikes, &state.UntilAt, &state.RetainUntil, &state.Category, &state.Match)
	if errors.Is(err, sql.ErrNoRows) {
		return RefusalCooldown{}, nil
	}
	if err != nil {
		return RefusalCooldown{}, fmt.Errorf("read refusal cooldown: %w", err)
	}
	return state, nil
}

// RecordRefusalCooldown updates existing matched ancestors but inserts only the
// explicit session and full refused chain. Inserting every prefix would block
// unrelated conversations sharing an earlier, harmless exchange.
func (s *Store) RecordRefusalCooldown(ctx context.Context, lookup, record []string, category string, now time.Time) (RefusalCooldown, error) {
	if len(record) == 0 {
		return RefusalCooldown{}, fmt.Errorf("refusal cooldown requires a fingerprint")
	}
	keys, err := json.Marshal(lookup)
	if err != nil {
		return RefusalCooldown{}, fmt.Errorf("encode refusal fingerprints: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RefusalCooldown{}, fmt.Errorf("begin refusal cooldown: %w", err)
	}
	defer tx.Rollback()
	state, err := readRefusalCooldown(ctx, tx, string(keys), now.UnixMilli())
	if err != nil {
		return RefusalCooldown{}, err
	}
	// Concurrent requests may already be upstream when the first refusal arrives.
	// Their refusals join that cooldown without extending it or escalating strikes.
	if state.UntilAt <= now.UnixMilli() {
		state.Strikes++
		duration := 10 * time.Minute
		if state.Strikes >= 2 {
			state.Strikes = 2
			duration = time.Hour
		}
		state.UntilAt = now.Add(duration).UnixMilli()
		state.RetainUntil = now.Add(24 * time.Hour).UnixMilli()
		state.Category = category
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM refusal_cooldowns WHERE retain_until<=?`, now.UnixMilli()); err != nil {
		return RefusalCooldown{}, fmt.Errorf("prune refusal cooldowns: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE refusal_cooldowns SET strikes=?,until_at=?,retain_until=?,category=?
		WHERE fingerprint IN (SELECT value FROM json_each(?))`, state.Strikes, state.UntilAt, state.RetainUntil, state.Category, string(keys)); err != nil {
		return RefusalCooldown{}, fmt.Errorf("update refusal cooldown ancestors: %w", err)
	}
	for _, key := range record {
		if _, err := tx.ExecContext(ctx, `INSERT INTO refusal_cooldowns(fingerprint,strikes,until_at,retain_until,category)
			VALUES(?,?,?,?,?) ON CONFLICT(fingerprint) DO UPDATE SET
			strikes=excluded.strikes,until_at=excluded.until_at,retain_until=excluded.retain_until,category=excluded.category`,
			key, state.Strikes, state.UntilAt, state.RetainUntil, state.Category); err != nil {
			return RefusalCooldown{}, fmt.Errorf("record refusal cooldown: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RefusalCooldown{}, fmt.Errorf("commit refusal cooldown: %w", err)
	}
	return state, nil
}
