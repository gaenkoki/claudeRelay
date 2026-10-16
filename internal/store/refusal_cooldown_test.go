package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRefusalCooldownLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().Truncate(time.Second)
	lookup := []string{"session:one", "chain:shared", "chain:refused"}
	record := []string{"session:one", "chain:refused"}
	first, err := db.RecordRefusalCooldown(t.Context(), lookup, record, "bio", now)
	if err != nil || first.Strikes != 1 || first.UntilAt != now.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	shared, err := db.RefusalCooldown(t.Context(), []string{"chain:shared"}, now)
	if err != nil || shared.Strikes != 0 {
		t.Fatalf("harmless ancestor blocked: %+v err=%v", shared, err)
	}
	// Refusals from already-running requests share the original deadline.
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			state, err := db.RecordRefusalCooldown(t.Context(), lookup, record, "bio", now.Add(time.Minute))
			if err != nil || state.UntilAt != first.UntilAt || state.Strikes != 1 || state.RetainUntil != first.RetainUntil {
				t.Errorf("concurrent refusal changed cooldown: %+v err=%v", state, err)
			}
		})
	}
	group.Wait()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := db.RefusalCooldown(t.Context(), lookup, now.Add(time.Minute))
	if err != nil || restored.UntilAt != first.UntilAt || restored.Strikes != 1 {
		t.Fatalf("restart lost cooldown: %+v err=%v", restored, err)
	}
	// A later refused continuation escalates both itself and the original chain.
	later := now.Add(11 * time.Minute)
	next, err := db.RecordRefusalCooldown(t.Context(), append(lookup, "chain:continuation"), []string{"chain:continuation"}, "bio", later)
	if err != nil || next.Strikes != 2 || next.UntilAt != later.Add(time.Hour).UnixMilli() {
		t.Fatalf("escalation=%+v err=%v", next, err)
	}
	original, err := db.RefusalCooldown(t.Context(), []string{"chain:refused"}, later)
	if err != nil || original.UntilAt != next.UntilAt {
		t.Fatalf("ancestor escaped escalation: %+v err=%v", original, err)
	}
	resetAt := later.Add(24 * time.Hour)
	reset, err := db.RecordRefusalCooldown(t.Context(), lookup, record, "unknown", resetAt)
	if err != nil || reset.Strikes != 1 || reset.UntilAt != resetAt.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("retention did not reset: %+v err=%v", reset, err)
	}
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM refusal_cooldowns`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("expired fingerprints not pruned: rows=%d err=%v", rows, err)
	}
}

func TestSchemaV15MigratesRefusalCooldowns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP TABLE refusal_cooldowns; PRAGMA user_version=15`); err != nil {
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
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if _, err := db.RecordRefusalCooldown(t.Context(), []string{"chain:migrated"}, []string{"chain:migrated"}, "bio", time.Now()); err != nil {
		t.Fatal(err)
	}
}
