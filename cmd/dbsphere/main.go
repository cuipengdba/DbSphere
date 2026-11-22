package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/httpapi"
	"github.com/cuipengdba/dbsphere/internal/notify"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := probeReady(ctx); err != nil {
			log.Error("DbSphere readiness probe failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "rotate-admin-password" {
		if err := rotateAdminPassword(); err != nil {
			log.Error("DbSphere administrator password rotation failed", "error", err)
			os.Exit(1)
		}
		log.Info("DbSphere administrator password rotated; restart the app to apply it")
		return
	}
	if err := run(log); err != nil {
		log.Error("DbSphere Go startup failed", "error", err)
		os.Exit(1)
	}
}

func rotateAdminPassword() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pg, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	return pg.RotateAdminPassword(ctx, cfg.AdminPass)
}

func probeReady(ctx context.Context) error {
	port := os.Getenv("DBSPHERE_PORT")
	if port == "" {
		port = "8080"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/readyz", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness HTTP %d", response.StatusCode)
	}
	return nil
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pg, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	pg.ConfigureRetention(cfg.SampleDays, cfg.EventDays)
	notifyConfig, err := notify.Load()
	if err != nil {
		return err
	}
	passwordHash, err := pg.AdminPasswordHash(ctx, cfg.AdminPass)
	if err != nil {
		return err
	}
	viewerHash, err := pg.ViewerPasswordHash(ctx, cfg.ViewerPass)
	if err != nil {
		return err
	}
	approverHash, err := pg.ApproverPasswordHash(ctx, cfg.ApproverPass)
	if err != nil {
		return err
	}
	remoteAI := os.Getenv("DBSPHERE_AI_ALLOW_REMOTE")
	if remoteAI != "" && remoteAI != "true" && remoteAI != "false" {
		return fmt.Errorf("DBSPHERE_AI_ALLOW_REMOTE must be true or false")
	}
	aiClient, err := ai.New(ai.Config{URL: os.Getenv("DBSPHERE_AI_CHAT_URL"),
		Model: os.Getenv("DBSPHERE_AI_MODEL"), APIKey: os.Getenv("DBSPHERE_AI_API_KEY"),
		AllowRemote: remoteAI == "true"})
	if err != nil {
		return err
	}
	aiDailyLimit := 20
	if raw := os.Getenv("DBSPHERE_AI_DAILY_LIMIT"); raw != "" {
		aiDailyLimit, err = strconv.Atoi(raw)
		if err != nil || aiDailyLimit < 1 || aiDailyLimit > 1000 {
			return fmt.Errorf("DBSPHERE_AI_DAILY_LIMIT must be between 1 and 1000")
		}
	}
	autoAI := os.Getenv("DBSPHERE_AI_AUTO_SUMMARY")
	if autoAI != "" && autoAI != "true" && autoAI != "false" {
		return fmt.Errorf("DBSPHERE_AI_AUTO_SUMMARY must be true or false")
	}
	if autoAI == "true" && aiClient == nil {
		return fmt.Errorf("automatic AI analysis requires a configured model")
	}
	collectorRuntime := collectorstate.New(cfg.AutoCollect, time.Duration(cfg.CollectEvery)*time.Second,
		cfg.CollectWorkers, time.Now())
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpapi.New(pg,
		httpapi.AuthConfig{MasterKey: cfg.MasterKey, PasswordHash: passwordHash,
			ViewerPasswordHash:   viewerHash,
			ApproverPasswordHash: approverHash,
			ViewerAssetIDs:       cfg.ViewerAssetIDs,
			AutoCollect:          cfg.AutoCollect,
			CollectEvery:         cfg.CollectEvery,
			CollectorState:       collectorRuntime,
			SecureCookie:         os.Getenv("DBSPHERE_COOKIE_SECURE") == "true",
			OTLPToken:            cfg.OTLPToken, AI: aiClient, AIDailyLimit: aiDailyLimit}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	var collectorDone <-chan struct{}
	notifierDone := make(chan struct{})
	go func() {
		defer close(notifierDone)
		runNotifier(ctx, pg, notifyConfig, log)
	}()
	tracePrunerDone := make(chan struct{})
	go func() {
		defer close(tracePrunerDone)
		runTracePruner(ctx, pg, cfg.TraceDays, log)
	}()
	partitionDone := make(chan struct{})
	go func() {
		defer close(partitionDone)
		runPartitionMaintainer(ctx, pg, log)
	}()
	trendDone := make(chan struct{})
	go func() {
		defer close(trendDone)
		runTrendMaintainer(ctx, pg, log)
	}()
	var retentionDone <-chan struct{}
	if cfg.SampleDays > 0 || cfg.EventDays > 0 {
		done := make(chan struct{})
		retentionDone = done
		go func() {
			defer close(done)
			runDataRetention(ctx, pg, cfg.SampleDays, cfg.EventDays, log)
		}()
	}
	inspectionDone := make(chan struct{})
	go func() {
		defer close(inspectionDone)
		runInspectionScheduler(ctx, pg, cfg.MasterKey, log)
	}()
	var autoAIDone <-chan struct{}
	if autoAI == "true" {
		done := make(chan struct{})
		autoAIDone = done
		go func() {
			defer close(done)
			runAutoAI(ctx, pg, aiClient, aiDailyLimit, log)
		}()
		log.Info("automatic critical event AI summaries enabled", "dailyLimit", aiDailyLimit)
	}
	if cfg.AutoCollect {
		done := make(chan struct{})
		collectorDone = done
		go func() {
			defer close(done)
			runCollector(ctx, pg, cfg.MasterKey, time.Duration(cfg.CollectEvery)*time.Second,
				cfg.CollectWorkers, collectorRuntime, log)
		}()
		log.Info("Go PostgreSQL collector enabled", "intervalSeconds", cfg.CollectEvery)
	}
	log.Info("DbSphere Go migration server listening", "address", cfg.ListenAddr)
	err = server.ListenAndServe()
	cancel()
	if collectorDone != nil {
		<-collectorDone
	}
	<-notifierDone
	<-tracePrunerDone
	<-partitionDone
	<-trendDone
	if retentionDone != nil {
		<-retentionDone
	}
	<-inspectionDone
	if autoAIDone != nil {
		<-autoAIDone
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type autoAIStore interface {
	AutoAIEventIDs(context.Context, int) ([]string, error)
	EventAIEvidence(context.Context, string) (string, []string, error)
	ReserveAutomaticAIInvocation(context.Context, string, string, int) (string, error)
	CompleteAutomaticAIInvocation(context.Context, string, string, *ai.Result) error
}

func runAutoAI(ctx context.Context, pg autoAIStore, model *ai.Client, dailyLimit int, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		ids, err := pg.AutoAIEventIDs(ctx, 5)
		if err != nil && ctx.Err() == nil {
			log.Error("automatic AI event scan failed", "error", err)
		}
		for _, eventID := range ids {
			analyzeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			prompt, evidenceIDs, err := pg.EventAIEvidence(analyzeCtx, eventID)
			if err != nil || len(evidenceIDs) == 0 {
				cancel()
				continue
			}
			invocationID, err := pg.ReserveAutomaticAIInvocation(analyzeCtx, eventID, model.Model(), dailyLimit)
			if errors.Is(err, store.ErrAIBudgetExceeded) {
				cancel()
				break
			}
			if errors.Is(err, store.ErrAIAlreadyAnalyzed) {
				cancel()
				continue
			}
			if errors.Is(err, store.ErrAIEventNotEligible) {
				cancel()
				continue
			}
			if err != nil {
				log.Error("automatic AI budget reservation failed", "eventId", eventID, "error", err)
				cancel()
				continue
			}
			result, modelErr := model.Analyze(analyzeCtx, prompt, evidenceIDs)
			cancel()
			auditCtx, auditCancel := context.WithTimeout(context.Background(), 5*time.Second)
			var advisory *ai.Result
			if modelErr == nil {
				advisory = &result
			}
			if err := pg.CompleteAutomaticAIInvocation(auditCtx, eventID, invocationID, advisory); err != nil {
				log.Error("automatic AI audit failed", "eventId", eventID, "error", err)
			}
			auditCancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runInspectionScheduler(ctx context.Context, pg *store.Postgres, key []byte, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		due, err := pg.ClaimDueInspections(ctx, time.Now().UTC(), 20)
		if err != nil && ctx.Err() == nil {
			log.Error("Go inspection schedule scan failed", "error", err)
		}
		for _, item := range due {
			checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			_, err := pg.RunInspection(checkCtx, item.AssetID, item.TemplateID, key, "")
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("Go scheduled inspection failed", "assetId", item.AssetID,
					"templateId", item.TemplateID, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runTracePruner(ctx context.Context, pg *store.Postgres, days int, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	iteration := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := pg.PrunePendingTraceSpans(ctx); err != nil && ctx.Err() == nil {
			log.Error("Go pending trace cleanup failed", "error", err)
		}
		if iteration%12 == 0 {
			if _, err := pg.PruneTraceSpans(ctx, days); err != nil && ctx.Err() == nil {
				log.Error("Go trace retention scan failed", "error", err)
			}
		}
		iteration++
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runPartitionMaintainer(ctx context.Context, pg *store.Postgres, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maintenanceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := pg.EnsureTimePartitions(maintenanceCtx, time.Now().UTC())
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("Go storage partition maintenance failed", "error", err)
			}
		}
	}
}

func runTrendMaintainer(ctx context.Context, pg *store.Postgres, log *slog.Logger) {
	var pruneAfter time.Time
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
		projected, err := pg.AdvanceTrendProjection(workCtx)
		complete := false
		if projected && err == nil {
			complete, err = pg.AdvanceHourlyTrend(workCtx)
		}
		if complete && err == nil && time.Now().After(pruneAfter) {
			err = pg.PruneTrendProjection(workCtx)
			if err == nil {
				pruneAfter = time.Now().Add(time.Hour)
			}
		}
		cancel()
		wait := 150 * time.Millisecond
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("Go trend projection maintenance paused", "error", err)
			wait = 20 * time.Second
		case complete:
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func runDataRetention(ctx context.Context, pg *store.Postgres, sampleDays, eventDays int, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		var sampleCutoff, eventCutoff time.Time
		if eventDays > 0 {
			eventCutoff = now.AddDate(0, 0, -eventDays)
			sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			removed, more, err := drainRetention(sweepCtx, 1000, func(ctx context.Context, limit int) (int64, error) {
				return pg.PruneResolvedEvents(ctx, eventCutoff, limit)
			})
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("Go event retention sweep failed", "removed", removed, "error", err)
			} else if removed > 0 || more {
				log.Info("Go event retention sweep", "removed", removed, "backlogPossible", more)
			}
		}
		if sampleDays > 0 {
			sampleCutoff = now.AddDate(0, 0, -sampleDays)
			sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			removed, more, err := drainRetention(sweepCtx, 10000, func(ctx context.Context, limit int) (int64, error) {
				return pg.PruneSamples(ctx, sampleCutoff, limit)
			})
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("Go sample retention sweep failed", "removed", removed, "error", err)
			} else if removed > 0 || more {
				log.Info("Go sample retention sweep", "removed", removed, "backlogPossible", more)
			}
		}
		sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		removed, err := pg.DropEmptyExpiredPartitions(sweepCtx, sampleCutoff, eventCutoff, 4)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Error("Go empty partition retention sweep failed", "error", err)
		} else if removed > 0 {
			log.Info("Go empty partition retention sweep", "removed", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drainRetention limits each hourly sweep by time, batch size and batch count.
// A full last batch may leave a backlog for the next hour.
func drainRetention(ctx context.Context, batchSize int, prune func(context.Context, int) (int64, error)) (int64, bool, error) {
	const maxBatches = 100
	var removed int64
	for batch := range maxBatches {
		count, err := prune(ctx, batchSize)
		removed += count
		if err != nil {
			return removed, false, err
		}
		if count < int64(batchSize) {
			return removed, false, nil
		}
		if batch == maxBatches-1 {
			return removed, true, nil
		}
		select {
		case <-ctx.Done():
			return removed, false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return removed, true, nil
}

func runNotifier(ctx context.Context, pg *store.Postgres, cfg notify.Config, log *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := pg.QueueCriticalEscalations(ctx, time.Now().UTC(), 15); err != nil && ctx.Err() == nil {
			log.Error("Go alert escalation scan failed", "error", err)
		}
		if _, err := pg.DispatchDueNotifications(ctx, 20, cfg.Send); err != nil && ctx.Err() == nil {
			log.Error("Go notification dispatch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type collectorStore interface {
	ClaimCollectionJobs(context.Context, time.Duration, int) ([]store.CollectionLease, error)
	CheckCollectionLease(context.Context, store.CollectionLease, time.Duration, []byte) (store.CheckResult, error)
	RetryCollectionLease(context.Context, store.CollectionLease) error
}

func runCollector(ctx context.Context, pg collectorStore, key []byte, interval time.Duration, workers int,
	tracker *collectorstate.Tracker, log *slog.Logger) {
	ticker := time.NewTicker(min(interval, 3*time.Second))
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		tracker.Begin(time.Now())
		cycle := collectorstate.Cycle{}
		claimCtx, stopClaim := context.WithTimeout(ctx, 5*time.Second)
		jobs, err := pg.ClaimCollectionJobs(claimCtx, interval, workers)
		stopClaim()
		if err != nil {
			cycle.DueScanFailed = true
			log.Error("Go collector could not list due assets", "error", err)
		} else {
			cycle.Selected = len(jobs)
			cycle.AtBatchLimit = len(jobs) == workers
			var processed, targetFailures, collectorErrors atomic.Int64
			runBoundedBatch(ctx, jobs, workers, func(job store.CollectionLease) {
				checkCtx, cancel := context.WithTimeout(ctx, store.CollectionBudget)
				result, err := pg.CheckCollectionLease(checkCtx, job, interval, key)
				cancel()
				processed.Add(1)
				if err != nil {
					collectorErrors.Add(1)
					log.Error("Go collector failed to persist asset sample", "assetId", job.AssetID, "error", err)
					releaseCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					defer stop()
					if retryErr := pg.RetryCollectionLease(releaseCtx, job); retryErr != nil {
						log.Error("Go collector could not reschedule lease", "assetId", job.AssetID, "error", retryErr)
					}
				} else if result.Status == "error" {
					targetFailures.Add(1)
					log.Warn("Go collector observed asset failure", "assetId", job.AssetID, "sampleId", result.SampleID)
				}
			})
			cycle.Processed = int(processed.Load())
			cycle.TargetFailures = int(targetFailures.Load())
			cycle.CollectorErrors = int(collectorErrors.Load())
		}
		tracker.Finish(time.Now(), cycle)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runBoundedBatch[T any](ctx context.Context, ids []T, workers int, check func(T)) {
	if workers < 1 {
		workers = 1
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	jobs := make(chan T)
	var active sync.WaitGroup
	for range workers {
		active.Add(1)
		go func() {
			defer active.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				check(id)
			}
		}()
	}
feed:
	for _, id := range ids {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- id:
		}
	}
	close(jobs)
	active.Wait()
}
