// Package metrics keeps a bounded, in-memory view of relayed request activity.
//
// It deliberately stores request metadata only: no prompts, no response bodies,
// no headers, and no credentials. Nothing is persisted, so restarting the
// process clears the whole history.
package metrics

import (
	"sync"
	"time"
)

// Event describes one completed relay attempt.
type Event struct {
	RequestID string
	Time      time.Time
	Path      string
	// Ingress is the API key that admitted the request, not the pool of the
	// account that served it.
	Ingress               string
	Model                 string
	Account               string
	Selection             string
	Status                int
	Duration              time.Duration
	Error                 string
	Failover              *Failover
	Client                *ClientEvidence
	ClientConsistency     *ClientConsistency
	ClientClass           string
	ClientKind            string
	ClassificationVersion int
	RelayAction           string
	Refusal               bool
	RefusalCategory       string
	RefusalBilling        *RefusalBilling
}

// RefusalBilling records only token counts, never prompts or refusal explanations.
type RefusalBilling struct {
	Category      string           `json:"category"`
	Multiplier    float64          `json:"multiplier"`
	OriginalUsage map[string]int64 `json:"original_usage"`
	BilledUsage   map[string]int64 `json:"billed_usage"`
}

// ClientEvidence contains presence checks only. It never includes raw request
// headers, billing values, or metadata identities.
type ClientEvidence struct {
	BillingBlock       bool `json:"billing_block"`
	CCVersion          bool `json:"cc_version"`
	KnownEntrypoint    bool `json:"known_entrypoint"`
	OfficialPrompt     bool `json:"official_prompt"`
	StructuredMetadata bool `json:"structured_metadata"`
	ClaudeUserAgent    bool `json:"claude_user_agent"`
	ClaudeCodeSession  bool `json:"claude_code_session"`
	XAppCLI            bool `json:"x_app_cli"`
	AnthropicBeta      bool `json:"anthropic_beta"`
	AnthropicVersion   bool `json:"anthropic_version"`
}

// ClientConsistency records only comparison outcomes, never original claims.
type ClientConsistency struct {
	Session    string   `json:"session"`
	Version    string   `json:"version"`
	Entrypoint string   `json:"entrypoint"`
	Checksum   string   `json:"checksum"`
	CCH        bool     `json:"cch_present"`
	Violations []string `json:"violations,omitempty"`
}

// Failover names the account a request was moved away from. The relay permits at
// most one alternate account, so a request has at most one of these. Without it
// the rate-limited account would never appear in any per-account total.
type Failover struct {
	Account string `json:"account"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Record is the JSON view of an Event.
type Record struct {
	RequestID             string             `json:"request_id"`
	At                    int64              `json:"at"`
	Path                  string             `json:"path"`
	Ingress               string             `json:"ingress,omitempty"`
	Model                 string             `json:"model,omitempty"`
	Account               string             `json:"account,omitempty"`
	Selection             string             `json:"selection,omitempty"`
	Status                int                `json:"status"`
	DurationMS            int64              `json:"duration_ms"`
	Error                 string             `json:"error,omitempty"`
	Failover              *Failover          `json:"failover,omitempty"`
	ClientClass           string             `json:"client_class,omitempty"`
	ClientKind            string             `json:"client_kind,omitempty"`
	ClassificationVersion int                `json:"classification_version,omitempty"`
	ClientEvidence        *ClientEvidence    `json:"client_evidence,omitempty"`
	ClientConsistency     *ClientConsistency `json:"client_consistency,omitempty"`
	RelayAction           string             `json:"relay_action,omitempty"`
	Refusal               bool               `json:"refusal,omitempty"`
	RefusalCategory       string             `json:"refusal_category,omitempty"`
	RefusalBilling        *RefusalBilling    `json:"refusal_billing,omitempty"`
}

// AccountStat aggregates activity for one account alias.
type AccountStat struct {
	Requests   int64 `json:"requests"`
	Failures   int64 `json:"failures"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
	LastStatus int   `json:"last_status,omitempty"`
}

// Summary aggregates activity across all accounts.
type Summary struct {
	StartedAt      int64 `json:"started_at"`
	Requests       int64 `json:"requests"`
	Failures       int64 `json:"failures"`
	RecentRequests int64 `json:"recent_requests"`
	RecentFailures int64 `json:"recent_failures"`
	Capacity       int   `json:"capacity"`
}

// RecentWindow is the trailing period reported as "recent" activity.
const RecentWindow = 5 * time.Minute

// Recorder is a fixed-size ring of request records plus running counters.
// A nil or zero-capacity Recorder accepts events and discards them, which keeps
// the relay path free of conditional logic when logging is turned off.
type Recorder struct {
	mu       sync.Mutex
	ring     []Record
	next     int
	filled   bool
	requests int64
	failures int64
	accounts map[string]*AccountStat
	started  time.Time
}

// New returns a Recorder holding at most capacity records.
func New(capacity int) *Recorder {
	if capacity < 0 {
		capacity = 0
	}
	return &Recorder{
		ring:     make([]Record, capacity),
		accounts: make(map[string]*AccountStat),
		started:  time.Now(),
	}
}

// Failed reports whether a status code and transport error pair counts as a failure.
func Failed(status int, transportError string) bool {
	return transportError != "" || status == 0 || status >= 400
}

// Record stores one event. It is safe for concurrent use.
func (r *Recorder) Record(event Event) {
	if r == nil {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	record := Record{
		RequestID:             event.RequestID,
		At:                    event.Time.UnixMilli(),
		Path:                  event.Path,
		Ingress:               event.Ingress,
		Model:                 event.Model,
		Account:               event.Account,
		Selection:             event.Selection,
		Status:                event.Status,
		DurationMS:            event.Duration.Milliseconds(),
		Error:                 event.Error,
		Failover:              event.Failover,
		ClientClass:           event.ClientClass,
		ClientKind:            event.ClientKind,
		ClassificationVersion: event.ClassificationVersion,
		ClientEvidence:        event.Client,
		RelayAction:           event.RelayAction,
		ClientConsistency:     event.ClientConsistency,
		Refusal:               event.Refusal,
		RefusalCategory:       event.RefusalCategory,
		RefusalBilling:        event.RefusalBilling,
	}
	failed := Failed(record.Status, record.Error)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	if failed {
		r.failures++
	}
	if record.Account != "" {
		r.touchAccountLocked(record.Account, failed, record.At, record.Status)
	}
	if record.Failover != nil && record.Failover.Account != "" {
		r.touchAccountLocked(record.Failover.Account, true, record.At, record.Failover.Status)
	}
	if len(r.ring) == 0 {
		return
	}
	r.ring[r.next] = record
	r.next = (r.next + 1) % len(r.ring)
	if r.next == 0 {
		r.filled = true
	}
}

// Recent returns up to limit records, newest first.
func (r *Recorder) Recent(limit int) []Record {
	if r == nil || limit <= 0 {
		return []Record{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.storedLocked()
	if limit > stored {
		limit = stored
	}
	records := make([]Record, 0, limit)
	for offset := 1; offset <= limit; offset++ {
		records = append(records, r.ring[r.indexLocked(offset)])
	}
	return records
}

// Summary returns global counters plus activity within RecentWindow.
func (r *Recorder) Summary(now time.Time) Summary {
	if r == nil {
		return Summary{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	summary := Summary{
		StartedAt: r.started.UnixMilli(),
		Requests:  r.requests,
		Failures:  r.failures,
		Capacity:  len(r.ring),
	}
	cutoff := now.Add(-RecentWindow).UnixMilli()
	stored := r.storedLocked()
	for offset := 1; offset <= stored; offset++ {
		record := r.ring[r.indexLocked(offset)]
		if record.At < cutoff {
			break
		}
		summary.RecentRequests++
		if Failed(record.Status, record.Error) {
			summary.RecentFailures++
		}
	}
	return summary
}

// AccountStats returns a copy of the per-account aggregates keyed by alias.
func (r *Recorder) AccountStats() map[string]AccountStat {
	stats := make(map[string]AccountStat)
	if r == nil {
		return stats
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for alias, stat := range r.accounts {
		stats[alias] = *stat
	}
	return stats
}

// Forget drops the aggregates of a deleted account so the console does not keep
// reporting activity for an alias that no longer exists.
func (r *Recorder) Forget(alias string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accounts, alias)
}

// touchAccountLocked folds one attempt into an account's running totals. A failed
// attempt that was retried elsewhere still counts against the account that failed.
func (r *Recorder) touchAccountLocked(alias string, failed bool, at int64, status int) {
	stat := r.accounts[alias]
	if stat == nil {
		stat = &AccountStat{}
		r.accounts[alias] = stat
	}
	stat.Requests++
	if failed {
		stat.Failures++
	}
	stat.LastUsedAt = at
	stat.LastStatus = status
}

func (r *Recorder) storedLocked() int {
	if r.filled {
		return len(r.ring)
	}
	return r.next
}

// indexLocked maps a newest-first offset (1 is the newest record) onto the ring.
func (r *Recorder) indexLocked(offset int) int {
	index := (r.next - offset) % len(r.ring)
	if index < 0 {
		index += len(r.ring)
	}
	return index
}
