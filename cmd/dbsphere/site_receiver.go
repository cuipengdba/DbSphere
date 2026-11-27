package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/cuipengdba/dbsphere/internal/store"
)

type siteReceiverConfig struct {
	Listen          string              `json:"listen"`
	CertificateFile string              `json:"certificateFile"`
	KeyFile         string              `json:"keyFile"`
	ClientCAFile    string              `json:"clientCAFile"`
	IntervalSeconds int                 `json:"intervalSeconds"`
	Sites           map[string][]string `json:"sites"`
}

func loadSiteReceiver(path string) (siteReceiverConfig, map[string]string, error) {
	var cfg siteReceiverConfig
	raw, err := remoteobs.ReadFile(path, 64<<10)
	if err != nil || remoteobs.Decode(bytes.NewReader(raw), &cfg) != nil {
		return cfg, nil, errors.New("invalid site receiver configuration file")
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	value, portErr := strconv.Atoi(port)
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	if err != nil || portErr != nil || value < 1 || value > 65535 ||
		interval < remoteobs.MinInterval || interval > remoteobs.MaxInterval || len(cfg.Sites) == 0 || len(cfg.Sites) > 128 {
		return cfg, nil, remoteobs.ErrInvalid
	}
	lookup := map[string]string{}
	for site, fingerprints := range cfg.Sites {
		if !remoteobs.ValidSite(site) || site == "local" || len(fingerprints) == 0 || len(fingerprints) > 4 {
			return cfg, nil, remoteobs.ErrInvalid
		}
		for _, fingerprint := range fingerprints {
			digest, err := hex.DecodeString(fingerprint)
			if err != nil || len(digest) != sha256.Size {
				return cfg, nil, remoteobs.ErrInvalid
			}
			fingerprint = hex.EncodeToString(digest)
			if lookup[fingerprint] != "" {
				return cfg, nil, remoteobs.ErrInvalid
			}
			lookup[fingerprint] = site
		}
	}
	return cfg, lookup, nil
}

func siteReceiver(pg *store.Postgres, sites map[string]string, interval time.Duration) http.Handler {
	concurrency := make(chan struct{}, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+remoteobs.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}
		digest := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		site := sites[hex.EncodeToString(digest[:])]
		if site == "" {
			http.Error(w, "site certificate is not registered", http.StatusForbidden)
			return
		}
		select {
		case concurrency <- struct{}{}:
			defer func() { <-concurrency }()
		default:
			http.Error(w, "receiver busy", http.StatusServiceUnavailable)
			return
		}
		var item remoteobs.Observation
		if remoteobs.Decode(http.MaxBytesReader(w, r.Body, remoteobs.MaxBody), &item) != nil {
			http.Error(w, "invalid observation", http.StatusBadRequest)
			return
		}
		backlog := item.Backlog
		if raw := r.Header.Get("X-DbSphere-Backlog"); raw != "" {
			var err error
			backlog, err = strconv.Atoi(raw)
			if err != nil || backlog < 0 || backlog > 10000 {
				http.Error(w, "invalid backlog", http.StatusBadRequest)
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		receipt, err := pg.StoreRemoteObservation(ctx, site, item, interval, backlog)
		if err != nil {
			status := http.StatusServiceUnavailable
			switch {
			case errors.Is(err, remoteobs.ErrInvalid):
				status = http.StatusBadRequest
			case errors.Is(err, store.ErrRemoteScope):
				status = http.StatusForbidden
			case errors.Is(err, store.ErrRemoteConflict):
				status = http.StatusConflict
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(receipt)
	})
	return mux
}

func startSiteReceiver(ctx context.Context, pg *store.Postgres, path string, log *slog.Logger) (func(), error) {
	cfg, scopes, err := loadSiteReceiver(path)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := remoteobs.LoadTLS(cfg.CertificateFile, cfg.KeyFile, cfg.ClientCAFile, true)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen for site observations: %w", err)
	}
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	srv := &http.Server{Handler: siteReceiver(pg, scopes, interval), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 8 * time.Second,
		WriteTimeout: 8 * time.Second, IdleTimeout: 20 * time.Second, MaxHeaderBytes: 8 << 10}
	ctx, cancel := context.WithCancel(ctx)
	served, scanned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("site receiver stopped", "error", err)
		}
	}()
	go func() {
		defer close(scanned)
		ticker := time.NewTicker(min(interval, 30*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				checkCtx, stop := context.WithTimeout(ctx, 8*time.Second)
				err := pg.RefreshRemoteCollection(checkCtx, interval, now.UTC())
				stop()
				if err != nil && ctx.Err() == nil {
					log.Warn("refresh site collector freshness", "error", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if srv.Shutdown(shutdown) != nil {
			_ = srv.Close()
		}
		<-served
		<-scanned
	}, nil
}

func assignSiteCollector(id, site string) error {
	if site == "local" {
		site = ""
	} else {
		cfg, _, err := loadSiteReceiver(os.Getenv("DBSPHERE_SITE_RECEIVER_CONFIG"))
		if err != nil {
			return err
		}
		if len(cfg.Sites[site]) == 0 {
			return errors.New("site is not registered in the receiver configuration")
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	pg, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	return pg.AssignRemoteCollector(ctx, id, site)
}
