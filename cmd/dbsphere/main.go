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
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
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
	if err := run(log); err != nil {
		log.Error("DbSphere Go startup failed", "error", err)
		os.Exit(1)
	}
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
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpapi.New(pg,
		httpapi.AuthConfig{MasterKey: cfg.MasterKey, PasswordHash: passwordHash,
			ViewerPasswordHash: viewerHash,
			ViewerAssetIDs:     cfg.ViewerAssetIDs,
			SecureCookie:       os.Getenv("DBSPHERE_COOKIE_SECURE") == "true",
			OTLPToken:          cfg.OTLPToken, AI: aiClient, AIDailyLimit: aiDailyLimit}),
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
			runCollector(ctx, pg, cfg.MasterKey, time.Duration(cfg.CollectEvery)*time.Second, log)
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

func runCollector(ctx context.Context, pg *store.Postgres, key []byte, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		ids, err := pg.DueAssetIDs(ctx, interval, 100)
		if err != nil {
			log.Error("Go collector could not list due assets", "error", err)
		} else {
			for _, id := range ids {
				if err := pg.CheckStaleAsset(ctx, id, interval, time.Now().UTC()); err != nil {
					log.Error("Go collector could not evaluate stale asset", "assetId", id, "error", err)
				}
				checkCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				result, err := pg.CheckAsset(checkCtx, id, key)
				cancel()
				if err == nil {
					if staleErr := pg.CheckStaleAsset(ctx, id, interval, time.Now().UTC()); staleErr != nil {
						log.Error("Go collector could not clear stale asset", "assetId", id, "error", staleErr)
					}
				}
				if err != nil {
					log.Error("Go collector failed to persist asset sample", "assetId", id, "error", err)
				} else if result.Status == "error" {
					log.Warn("Go collector observed asset failure", "assetId", id, "sampleId", result.SampleID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
