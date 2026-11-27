package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/config"
	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/cuipengdba/dbsphere/internal/store"
)

type siteSummaryConfig struct {
	Endpoint        string `json:"endpoint"`
	CertificateFile string `json:"certificateFile"`
	KeyFile         string `json:"keyFile"`
	CAFile          string `json:"caFile"`
	Organization    string `json:"organization"`
	IntervalSeconds int    `json:"intervalSeconds"`
	LocalVersionURL string `json:"localVersionUrl"`
}

func readSiteSummaryConfig(path string) (siteSummaryConfig, error) {
	var cfg siteSummaryConfig
	wire, err := remoteobs.ReadFile(path, 32<<10)
	if err != nil || remoteobs.Decode(bytes.NewReader(wire), &cfg) != nil {
		return cfg, errors.New("invalid site summary configuration file")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.EscapedPath() != remoteobs.SiteSummaryPath ||
		!remoteobs.ValidID(cfg.Organization) || cfg.IntervalSeconds < 10 || cfg.IntervalSeconds > 3600 {
		return cfg, remoteobs.ErrInvalid
	}
	local, err := url.Parse(cfg.LocalVersionURL)
	if err != nil || local.Scheme != "http" || (local.Hostname() != "127.0.0.1" && local.Hostname() != "::1") ||
		local.User != nil || local.RawQuery != "" || local.Fragment != "" || local.EscapedPath() != "/api/version" {
		return cfg, remoteobs.ErrInvalid
	}
	return cfg, nil
}

func readLocalSiteVersion(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var result struct {
		Version string `json:"version"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 2048)).Decode(&result) != nil ||
		!remoteobs.ValidSiteRelease(result.Version) {
		return "", errors.New("invalid site console version")
	}
	return result.Version, nil
}

func deliverSiteSummary(ctx context.Context, client *http.Client, endpoint string, point remoteobs.SiteSummary) error {
	data, err := json.Marshal(point)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid site summary request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return errors.New("site summary receiver unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("site summary receiver HTTP %d", res.StatusCode)
	}
	var receipt remoteobs.Receipt
	if remoteobs.Decode(io.LimitReader(res.Body, 1024), &receipt) != nil || receipt.ID != point.ID ||
		(receipt.Status != "stored" && receipt.Status != "duplicate" && receipt.Status != "superseded") {
		return errors.New("invalid site summary receipt")
	}
	return nil
}

func runSiteSummary(path string, log *slog.Logger) error {
	cfg, err := readSiteSummaryConfig(path)
	if err != nil {
		return err
	}
	local, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	startup, stop := context.WithTimeout(ctx, 20*time.Second)
	pg, err := store.Open(startup, local.DatabaseURL)
	stop()
	if err != nil {
		return err
	}
	defer pg.Close()
	tlsConfig, err := remoteobs.LoadTLS(cfg.CertificateFile, cfg.KeyFile, cfg.CAFile, false)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy, transport.TLSClientConfig = nil, tlsConfig
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	tick := time.NewTicker(min(3*time.Second, interval))
	defer tick.Stop()
	var next time.Time
	var pending *remoteobs.SiteSummary
	for ctx.Err() == nil {
		if now := time.Now().UTC(); !now.Before(next) {
			point := remoteobs.SiteSummary{ID: siteObservationID(),
				ObservedAt: now, IntervalSeconds: cfg.IntervalSeconds}
			readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			point.Version, err = readLocalSiteVersion(readCtx, client, cfg.LocalVersionURL)
			if err == nil {
				err = pg.ReadSiteSummary(readCtx, cfg.Organization, local.CollectEvery, &point)
			}
			stop()
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("site summary local read unavailable")
				}
			} else {
				pending = &point
			}
			next = now.Add(interval)
		}
		if pending != nil {
			if err := deliverSiteSummary(ctx, client, cfg.Endpoint, *pending); err == nil {
				pending = nil
			} else if ctx.Err() == nil {
				log.Warn("site summary delivery", "error", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil
}
