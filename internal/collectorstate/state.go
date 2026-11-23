package collectorstate

import (
	"sync"
	"time"
)

const MinimumIntervalSeconds = 10

// Cycle counts work observed by this Go process. It does not estimate work
// performed by other replicas or measure the number of due assets beyond the
// scheduler's batch limit.
type Cycle struct {
	Selected        int  `json:"selected"`
	Processed       int  `json:"processed"`
	TargetFailures  int  `json:"targetFailures"`
	CollectorErrors int  `json:"collectorErrors"`
	DueScanFailed   bool `json:"dueScanFailed"`
	AtBatchLimit    bool `json:"atBatchLimit"`
}

type Snapshot struct {
	State           string     `json:"state"`
	Enabled         bool       `json:"enabled"`
	IntervalSeconds int        `json:"intervalSeconds"`
	WorkerLimit     int        `json:"workerLimit"`
	BatchLimit      int        `json:"batchLimit"`
	ProcessStarted  time.Time  `json:"processStartedAt"`
	ActiveStarted   *time.Time `json:"activeStartedAt"`
	CycleStarted    *time.Time `json:"cycleStartedAt"`
	CycleFinished   *time.Time `json:"cycleFinishedAt"`
	CycleDurationMs int64      `json:"cycleDurationMs"`
	Cycle           Cycle      `json:"cycle"`
	CompletedCycles uint64     `json:"completedCycles"`
}

// Tracker is intentionally process-local. A restart clears its counters;
// durable target samples and alerts continue to live in PostgreSQL 18.
type Tracker struct {
	mu       sync.RWMutex
	enabled  bool
	interval time.Duration
	workers  int
	started  time.Time
	running  bool
	activeAt time.Time
	cycleAt  time.Time
	endedAt  time.Time
	last     Cycle
	cycles   uint64
}

func New(enabled bool, interval time.Duration, workers int, started time.Time) *Tracker {
	return &Tracker{enabled: enabled, interval: interval, workers: workers, started: started}
}

func (t *Tracker) Begin(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = true
	t.activeAt = at
}

func (t *Tracker) Finish(at time.Time, cycle Cycle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false
	t.cycleAt = t.activeAt
	t.activeAt = time.Time{}
	t.endedAt = at
	t.last = cycle
	t.cycles++
}

func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := Snapshot{Enabled: t.enabled, IntervalSeconds: int(t.interval.Seconds()),
		WorkerLimit: t.workers, BatchLimit: t.workers, ProcessStarted: t.started.UTC(),
		Cycle: t.last, CompletedCycles: t.cycles}
	if t.running {
		at := t.activeAt.UTC()
		result.ActiveStarted = &at
	}
	if !t.cycleAt.IsZero() {
		at := t.cycleAt.UTC()
		result.CycleStarted = &at
	}
	if !t.endedAt.IsZero() {
		at := t.endedAt.UTC()
		result.CycleFinished = &at
		result.CycleDurationMs = t.endedAt.Sub(t.cycleAt).Milliseconds()
	}
	anchor := t.started
	if !t.endedAt.IsZero() {
		anchor = t.endedAt
	}
	switch {
	case !t.enabled:
		result.State = "disabled"
	case t.interval <= 0:
		result.State = "degraded"
	case t.running && now.Sub(t.activeAt) > 2*t.interval:
		result.State = "overdue"
	case t.running:
		result.State = "running"
	case now.Sub(anchor) > 2*t.interval:
		result.State = "stalled"
	case t.cycles == 0:
		result.State = "starting"
	case t.last.DueScanFailed || t.last.CollectorErrors > 0:
		result.State = "degraded"
	default:
		result.State = "healthy"
	}
	return result
}
