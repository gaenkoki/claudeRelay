package metrics

import (
	"testing"
	"time"
)

func TestRecentReturnsNewestFirstAndDropsOldest(t *testing.T) {
	t.Parallel()
	recorder := New(3)
	for i := 1; i <= 5; i++ {
		recorder.Record(Event{RequestID: string(rune('a' + i - 1)), Status: 200, Account: "primary"})
	}
	records := recorder.Recent(10)
	if len(records) != 3 {
		t.Fatalf("Recent length = %d, want 3", len(records))
	}
	want := []string{"e", "d", "c"}
	for i, record := range records {
		if record.RequestID != want[i] {
			t.Errorf("record %d = %q, want %q", i, record.RequestID, want[i])
		}
	}
	summary := recorder.Summary(time.Now())
	if summary.Requests != 5 {
		t.Errorf("Requests = %d, want 5", summary.Requests)
	}
	if summary.RecentRequests != 3 {
		t.Errorf("RecentRequests = %d, want 3 (bounded by ring capacity)", summary.RecentRequests)
	}
}

func TestZeroCapacityKeepsCountersWithoutRecords(t *testing.T) {
	t.Parallel()
	recorder := New(0)
	recorder.Record(Event{Status: 200, Account: "primary"})
	recorder.Record(Event{Status: 500, Account: "primary"})
	if records := recorder.Recent(10); len(records) != 0 {
		t.Fatalf("Recent length = %d, want 0", len(records))
	}
	summary := recorder.Summary(time.Now())
	if summary.Requests != 2 || summary.Failures != 1 {
		t.Fatalf("summary = %+v, want 2 requests and 1 failure", summary)
	}
	if stat := recorder.AccountStats()["primary"]; stat.Requests != 2 || stat.Failures != 1 {
		t.Fatalf("account stat = %+v, want 2 requests and 1 failure", stat)
	}
}

func TestExtraUsageCountersSurviveEvictionAndDisabledRequestLog(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, 2, 4} {
		now := time.Now()
		recorder := New(capacity)
		recorder.Record(Event{Time: now.Add(-2 * RecentWindow), Status: 400, UpstreamErrorCategory: UpstreamErrorThirdPartyExtraUsage})
		recorder.Record(Event{Time: now, Status: 400, UpstreamErrorCategory: UpstreamErrorThirdPartyExtraUsage})
		recorder.Record(Event{Time: now, Status: 400})
		summary := recorder.Summary(now)
		wantRecent := int64(1)
		if capacity == 0 {
			wantRecent = 0
		}
		if summary.ThirdPartyExtraUsageRequests != 2 || summary.RecentThirdPartyExtraUsageRequests != wantRecent || summary.Failures != 3 {
			t.Fatalf("capacity=%d summary=%+v", capacity, summary)
		}
	}
}

func TestFailoverCountsAgainstTheAbandonedAccount(t *testing.T) {
	t.Parallel()
	recorder := New(4)
	recorder.Record(Event{
		Status:   200,
		Account:  "secondary",
		Failover: &Failover{Account: "primary", Status: 429},
	})
	stats := recorder.AccountStats()
	if stats["secondary"].Failures != 0 || stats["secondary"].Requests != 1 {
		t.Errorf("secondary stat = %+v, want one successful request", stats["secondary"])
	}
	if stats["primary"].Failures != 1 || stats["primary"].LastStatus != 429 {
		t.Errorf("primary stat = %+v, want one failure recorded as 429", stats["primary"])
	}
	if summary := recorder.Summary(time.Now()); summary.Requests != 1 || summary.Failures != 0 {
		t.Errorf("summary = %+v, want one successful client request", summary)
	}
}

func TestSummaryWindowExcludesOldRecords(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recorder := New(4)
	recorder.Record(Event{Time: now.Add(-2 * RecentWindow), Status: 500})
	recorder.Record(Event{Time: now, Status: 200})
	summary := recorder.Summary(now)
	if summary.Requests != 2 {
		t.Errorf("Requests = %d, want 2", summary.Requests)
	}
	if summary.RecentRequests != 1 || summary.RecentFailures != 0 {
		t.Errorf("recent = %d/%d, want 1 request and 0 failures", summary.RecentRequests, summary.RecentFailures)
	}
}

func TestForgetDropsAccountAggregates(t *testing.T) {
	t.Parallel()
	recorder := New(2)
	recorder.Record(Event{Status: 200, Account: "primary"})
	recorder.Forget("primary")
	if _, present := recorder.AccountStats()["primary"]; present {
		t.Fatal("account stats survived Forget")
	}
}

func TestRefusalIsRecordedWithoutCountingSuccessfulHTTPResponseAsFailure(t *testing.T) {
	recorder := New(2)
	recorder.Record(Event{Status: 200, Account: "primary", Refusal: true, RefusalCategory: "cyber"})
	records := recorder.Recent(1)
	if len(records) != 1 || !records[0].Refusal || records[0].RefusalCategory != "cyber" {
		t.Fatalf("records=%+v", records)
	}
	if summary := recorder.Summary(time.Now()); summary.Failures != 0 {
		t.Fatalf("summary=%+v", summary)
	}
}
