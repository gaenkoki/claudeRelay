package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageResetLedgerMigrationAndPendingExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP TABLE usage_reset_operations; PRAGMA user_version=16`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	var admitted atomic.Int64
	var group sync.WaitGroup
	for i := range 8 {
		group.Go(func() {
			op := UsageResetOperation{RequestID: fmt.Sprintf("request-%d", i), OrganizationUUID: "org", SubjectUUID: "subject", AccountID: 1, AccountUUID: "credential", GrantID: "launch", Clears: []string{"five_hour"}, CreatedAt: time.Now().UnixMilli()}
			if err := db.BeginUsageReset(t.Context(), op); err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrUsageResetPending) {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("concurrent operations admitted=%d", admitted.Load())
	}
	op, found, err := db.LatestUsageReset(t.Context(), "org", "subject")
	if err != nil || !found {
		t.Fatalf("missing pending operation: %v", err)
	}
	if err := db.FinishUsageReset(t.Context(), op, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restored, found, err := db.LatestUsageReset(t.Context(), "org", "subject")
	if err != nil || !found || restored.State != "unknown" || restored.RequestID != op.RequestID {
		t.Fatalf("restart lost uncertain operation: %+v err=%v", restored, err)
	}
	next := op
	next.RequestID = "next-request"
	if err := db.BeginUsageReset(t.Context(), next); !errors.Is(err, ErrUsageResetPending) {
		t.Fatalf("pending exclusion lost: %v", err)
	}
	if err := db.FinishUsageReset(t.Context(), restored, "settled", "manually_resolved"); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginUsageReset(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	// Settled IDs remain in the ledger even after a later operation begins.
	if old, found, err := db.UsageResetOperation(t.Context(), op.RequestID); err != nil || !found || old.Result != "manually_resolved" {
		t.Fatalf("deduplication history lost: %+v %v", old, err)
	}
}
