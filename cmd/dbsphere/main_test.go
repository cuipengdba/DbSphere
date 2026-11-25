package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func TestContainerReadinessProbe(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBSPHERE_PORT", address.Port())
	if err := probeReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if err := probeReady(context.Background()); err == nil {
		t.Fatal("unready PostgreSQL store was reported healthy")
	}
}

func TestCollectorReusesFreeSlotWhileSlowJobIsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 4)
	fast := make(chan struct{})
	slow := make(chan struct{})
	finished := make(chan struct{})
	stub := &collectorLaneStub{jobs: []store.CollectionLease{
		{AssetID: "slow"}, {AssetID: "fast-1"}, {AssetID: "fast-2"}, {AssetID: "next"},
	}, started: started, fast: fast, slow: slow}
	go func() {
		defer close(finished)
		runCollectorLane(ctx, stub, nil, 30*time.Second, 3, store.CollectionRegular, nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	for range 3 {
		select {
		case id := <-started:
			if id == "next" {
				t.Fatal("worker limit exceeded before any slot was released")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("independent assets did not start in parallel")
		}
	}
	fast <- struct{}{}
	select {
	case id := <-started:
		if id != "next" {
			t.Fatalf("started %q, want next", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first free slot waited for the slow job")
	}
	close(slow)
	close(fast)
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("collector lane did not finish")
	}
}

type collectorLaneStub struct {
	jobs    []store.CollectionLease
	started chan<- string
	fast    <-chan struct{}
	slow    <-chan struct{}
}

func (stub *collectorLaneStub) ClaimCollectionJobs(_ context.Context, _ time.Duration, limit int, _ store.CollectionLane) ([]store.CollectionLease, error) {
	count := min(limit, len(stub.jobs))
	claimed := stub.jobs[:count]
	stub.jobs = stub.jobs[count:]
	return claimed, nil
}

func (*collectorLaneStub) RetryCollectionLease(context.Context, store.CollectionLease) error {
	return nil
}

func (stub *collectorLaneStub) CheckCollectionLease(ctx context.Context, lease store.CollectionLease, _ time.Duration, _ []byte) (store.CheckResult, error) {
	stub.started <- lease.AssetID
	release := stub.fast
	if lease.AssetID == "slow" {
		release = stub.slow
	}
	select {
	case <-release:
		return store.CheckResult{Status: "ok"}, nil
	case <-ctx.Done():
		return store.CheckResult{}, ctx.Err()
	}
}

type collectorCycleStub struct{ scanErr error }

func (stub collectorCycleStub) ClaimCollectionJobs(context.Context, time.Duration, int, store.CollectionLane) ([]store.CollectionLease, error) {
	if stub.scanErr != nil {
		return nil, stub.scanErr
	}
	return []store.CollectionLease{{AssetID: "unavailable-target"}, {AssetID: "store-failure"}}, nil
}
func (collectorCycleStub) RetryCollectionLease(context.Context, store.CollectionLease) error {
	return nil
}
func (collectorCycleStub) CheckCollectionLease(_ context.Context, lease store.CollectionLease, _ time.Duration, _ []byte) (store.CheckResult, error) {
	if lease.AssetID == "store-failure" {
		return store.CheckResult{}, errors.New("test write failure")
	}
	return store.CheckResult{Status: "error"}, nil
}

func TestCollectorCycleSeparatesTargetAndStoreFailures(t *testing.T) {
	for _, test := range []struct {
		name         string
		scanErr      error
		selected     int
		processed    int
		target       int
		collectorErr int
		scanFailed   bool
	}{
		{name: "target and store", selected: 2, processed: 2, target: 1, collectorErr: 1},
		{name: "due scan", scanErr: errors.New("test scan failure"), scanFailed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := collectorstate.New(true, 15*time.Second, 2, time.Now().UTC())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				runCollectorLane(ctx, collectorCycleStub{scanErr: test.scanErr}, nil, 15*time.Second, 3, store.CollectionRegular,
					tracker, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()
			deadline := time.After(3 * time.Second)
			for tracker.Snapshot(time.Now().UTC()).CompletedCycles == 0 {
				select {
				case <-deadline:
					t.Fatal("collector did not complete its first cycle")
				case <-time.After(5 * time.Millisecond):
				}
			}
			cancel()
			<-done
			snapshot := tracker.Snapshot(time.Now().UTC())
			cycle := snapshot.Cycle
			if snapshot.State != "degraded" || cycle.Selected != test.selected ||
				cycle.Processed != test.processed || cycle.TargetFailures != test.target ||
				cycle.CollectorErrors != test.collectorErr || cycle.DueScanFailed != test.scanFailed {
				t.Fatalf("collector cycle status = %+v", snapshot)
			}
		})
	}
}

type autoAITestStore struct {
	cancel    context.CancelFunc
	completed chan *ai.Result
}

func (s *autoAITestStore) AutoAIEventIDs(context.Context, int) ([]string, error) {
	return []string{"event-1"}, nil
}
func (s *autoAITestStore) EventAIEvidence(context.Context, string) (string, []string, error) {
	return "脱敏事件证据 evidence-1", []string{"evidence-1"}, nil
}
func (s *autoAITestStore) ReserveAutomaticAIInvocation(context.Context, string, string, int) (string, error) {
	return "invocation-1", nil
}
func (s *autoAITestStore) CompleteAutomaticAIInvocation(_ context.Context, eventID, invocationID string, result *ai.Result) error {
	if eventID != "event-1" || invocationID != "invocation-1" {
		return ai.ErrUnavailable
	}
	s.completed <- result
	s.cancel()
	return nil
}

func TestAutomaticAIWorkerKeepsValidatedAdvice(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Error("unexpected model request")
		}
		content, _ := json.Marshal(map[string]any{"summary": "数据库告警需要核查。",
			"evidenceIds": []string{"evidence-1"}, "uncertainty": "还有未知项。",
			"suggestedChecks": []string{"查看原始样本"}})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"content": string(content)}}}})
	}))
	defer model.Close()
	client, err := ai.New(ai.Config{URL: model.URL + "/v1/chat/completions", Model: "local-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &autoAITestStore{cancel: cancel, completed: make(chan *ai.Result, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAutoAI(ctx, stub, func(context.Context) (ai.ActiveSettings, error) {
			return ai.ActiveSettings{Client: client, Settings: ai.Settings{AutoSummary: true, DailyLimit: 20}}, nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case result := <-stub.completed:
		if result == nil || result.Model != "local-test" || len(result.EvidenceIDs) != 1 ||
			result.EvidenceIDs[0] != "evidence-1" {
			t.Fatalf("auto AI advice was not validated: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("automatic AI worker did not analyze critical event")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("automatic AI worker did not stop")
	}
}
