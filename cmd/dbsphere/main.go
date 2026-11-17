package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/httpapi"
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
	passwordHash, err := pg.AdminPasswordHash(ctx, cfg.AdminPass)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpapi.New(pg,
		httpapi.AuthConfig{MasterKey: cfg.MasterKey, PasswordHash: passwordHash,
			SecureCookie: os.Getenv("DBSPHERE_COOKIE_SECURE") == "true"}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	var collectorDone <-chan struct{}
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
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
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
				checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				result, err := pg.CheckAsset(checkCtx, id, key)
				cancel()
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
