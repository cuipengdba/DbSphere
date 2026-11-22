package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/notify"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func newPlatformReplica(collectorEnabled bool) store.PlatformReplica {
	var identity [16]byte
	_, _ = rand.Read(identity[:])
	id := hex.EncodeToString(identity[:])
	name := strings.TrimSpace(os.Getenv("DBSPHERE_REPLICA_NAME"))
	if name == "" || len(name) > 100 {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = id[:8]
	}
	return store.PlatformReplica{ID: id, Name: name, Collector: collectorEnabled}
}

func runPlatform(ctx context.Context, pg *store.Postgres, replica store.PlatformReplica, tracker *collectorstate.Tracker,
	start func(context.Context), log *slog.Logger) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var cancelLeader context.CancelFunc
	var leaderDone <-chan struct{}
	heartbeatHealthy := true
	stopWorkers := func() {
		if cancelLeader != nil {
			cancelLeader()
			<-leaderDone
			cancelLeader = nil
			log.Info("platform maintenance paused", "replicaId", replica.ID)
		}
	}
	defer func() {
		stopWorkers()
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pg.ReleasePlatformReplica(cleanup, replica.ID)
	}()
	for ctx.Err() == nil {
		replica.CompletedCycles = int64(tracker.Snapshot(time.Now()).CompletedCycles)
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		leader, err := pg.RegisterPlatformReplica(probe, replica)
		cancel()
		if heartbeatHealthy != (err == nil) && ctx.Err() == nil {
			heartbeatHealthy = err == nil
			log.Info("platform heartbeat state changed", "reachable", heartbeatHealthy, "replicaId", replica.ID)
		}
		if !leader || err != nil {
			stopWorkers()
		} else if cancelLeader == nil && ctx.Err() == nil {
			cancelLeader, leaderDone = startMaintenance(ctx, start)
			log.Info("platform maintenance acquired", "replicaId", replica.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func startMaintenance(ctx context.Context, run func(context.Context)) (context.CancelFunc, <-chan struct{}) {
	leaderCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		run(leaderCtx)
	}()
	return cancel, done
}

func runMaintenance(ctx context.Context, pg *store.Postgres, cfg config.Config, notifications notify.Config,
	autoModel *ai.Client, aiDailyLimit int, log *slog.Logger) {
	var tasks sync.WaitGroup
	tasks.Go(func() { runNotifier(ctx, pg, notifications, log) })
	tasks.Go(func() { runTracePruner(ctx, pg, cfg.TraceDays, log) })
	tasks.Go(func() { runPartitionMaintainer(ctx, pg, log) })
	tasks.Go(func() { runTrendMaintainer(ctx, pg, log) })
	tasks.Go(func() { runInspectionScheduler(ctx, pg, cfg.MasterKey, log) })
	if cfg.SampleDays > 0 || cfg.EventDays > 0 {
		tasks.Go(func() { runDataRetention(ctx, pg, cfg.SampleDays, cfg.EventDays, log) })
	}
	if autoModel != nil {
		tasks.Go(func() { runAutoAI(ctx, pg, autoModel, aiDailyLimit, log) })
	}
	tasks.Wait()
}
