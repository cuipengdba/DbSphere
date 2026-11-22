package collectorstate

import (
	"sync"
	"testing"
	"time"
)

func TestCollectorCycleHealthAndRestartBoundary(t *testing.T) {
	base := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	tracker := New(true, time.Minute, 4, base)
	if got := tracker.Snapshot(base.Add(time.Second)).State; got != "starting" {
		t.Fatalf("new collector state = %q", got)
	}
	if got := tracker.Snapshot(base.Add(3 * time.Minute)).State; got != "stalled" {
		t.Fatalf("collector without a first cycle = %q", got)
	}
	tracker.Begin(base.Add(3 * time.Minute))
	if active := tracker.Snapshot(base.Add(3*time.Minute + time.Second)); active.State != "running" ||
		active.ActiveStarted == nil || active.CycleStarted != nil {
		t.Fatalf("first active cycle mixed with a completed cycle: %+v", active)
	}
	if got := tracker.Snapshot(base.Add(6 * time.Minute)).State; got != "overdue" {
		t.Fatalf("slow but active collector was hidden: %q", got)
	}
	tracker.Finish(base.Add(6*time.Minute+time.Second), Cycle{Selected: 4, Processed: 3,
		TargetFailures: 1, CollectorErrors: 1, AtBatchLimit: true})
	snapshot := tracker.Snapshot(base.Add(6*time.Minute + 2*time.Second))
	if snapshot.State != "degraded" || snapshot.CompletedCycles != 1 ||
		snapshot.CycleDurationMs != 181000 || snapshot.Cycle.Processed != 3 ||
		!snapshot.Cycle.AtBatchLimit || snapshot.BatchLimit != snapshot.WorkerLimit {
		t.Fatalf("failed cycle status was lost: %+v", snapshot)
	}
	tracker.Begin(base.Add(7 * time.Minute))
	active := tracker.Snapshot(base.Add(7*time.Minute + time.Second))
	if active.State != "running" || active.ActiveStarted == nil ||
		active.CycleStarted == nil || !active.CycleStarted.Equal(base.Add(3*time.Minute)) ||
		active.CycleFinished == nil || active.CycleDurationMs != 181000 {
		t.Fatalf("active and last completed cycle were mixed: %+v", active)
	}
	tracker.Finish(base.Add(7*time.Minute+10*time.Second), Cycle{Selected: 1, Processed: 1})
	if got := tracker.Snapshot(base.Add(7*time.Minute + 11*time.Second)).State; got != "healthy" {
		t.Fatalf("successful cycle did not clear degraded state: %q", got)
	}
	if got := tracker.Snapshot(base.Add(10 * time.Minute)).State; got != "stalled" {
		t.Fatalf("stopped collector was hidden: %q", got)
	}
	newProcess := New(true, time.Minute, 4, base.Add(11*time.Minute))
	if got := newProcess.Snapshot(base.Add(11 * time.Minute)).CompletedCycles; got != 0 {
		t.Fatalf("process-local count survived restart: %d", got)
	}
	if got := New(false, time.Minute, 4, base).Snapshot(base.Add(24 * time.Hour)).State; got != "disabled" {
		t.Fatalf("disabled collector appeared stalled: %q", got)
	}
}

func TestCollectorSnapshotConcurrentWithWorkers(t *testing.T) {
	base := time.Now().UTC()
	tracker := New(true, time.Minute, 4, base)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				_ = tracker.Snapshot(time.Now().UTC())
			}
		}()
	}
	for i := range 100 {
		tracker.Begin(base.Add(time.Duration(i) * time.Minute))
		tracker.Finish(base.Add(time.Duration(i)*time.Minute+time.Second), Cycle{Selected: 1, Processed: 1})
	}
	readers.Wait()
	if got := tracker.Snapshot(base.Add(100 * time.Minute)).CompletedCycles; got != 100 {
		t.Fatalf("completed cycles = %d", got)
	}
}
