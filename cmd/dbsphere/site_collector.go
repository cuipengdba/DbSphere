package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
)

type siteTarget struct {
	AssetID      string `json:"assetId"`
	Engine       string `json:"engine"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Database     string `json:"database"`
	Username     string `json:"username"`
	SSLMode      string `json:"sslMode"`
	PasswordFile string `json:"passwordFile"`
}

type siteCollectorConfig struct {
	Endpoint        string       `json:"endpoint"`
	CertificateFile string       `json:"certificateFile"`
	KeyFile         string       `json:"keyFile"`
	CAFile          string       `json:"caFile"`
	SpoolDirectory  string       `json:"spoolDirectory"`
	MaxBytes        int64        `json:"maxBytes"`
	MaxFiles        int          `json:"maxFiles"`
	IntervalSeconds int          `json:"intervalSeconds"`
	Targets         []siteTarget `json:"targets"`
}

func readSiteCollectorConfig(path string) (siteCollectorConfig, error) {
	var cfg siteCollectorConfig
	data, err := remoteobs.ReadFile(path, 128<<10)
	if err != nil || remoteobs.Decode(bytes.NewReader(data), &cfg) != nil {
		return cfg, errors.New("invalid site collector configuration file")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.EscapedPath() != remoteobs.Path ||
		len(cfg.Targets) == 0 || len(cfg.Targets) > 128 {
		return cfg, remoteobs.ErrInvalid
	}
	if cfg.IntervalSeconds < int(remoteobs.MinInterval/time.Second) || cfg.IntervalSeconds > int(remoteobs.MaxInterval/time.Second) {
		return cfg, remoteobs.ErrInvalid
	}
	seen := map[string]bool{}
	for _, target := range cfg.Targets {
		if !remoteobs.ValidID(target.AssetID) || seen[target.AssetID] || !remoteobs.ValidEngine(target.Engine) ||
			target.Host == "" || len(target.Host) > 255 || target.Port < 1 || target.Port > 65535 ||
			target.Username == "" || len(target.Username) > 100 || target.Database == "" || len(target.Database) > 100 ||
			strings.ContainsAny(target.Host+target.Username+target.Database, "\x00\r\n\t") ||
			(target.SSLMode != "require" && target.SSLMode != "disable") ||
			!filepath.IsAbs(target.PasswordFile) {
			return cfg, remoteobs.ErrInvalid
		}
		seen[target.AssetID] = true
	}
	return cfg, nil
}

func siteObservationID() string {
	var raw [16]byte
	rand.Read(raw[:])
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func collectSiteTarget(ctx context.Context, target siteTarget, backlog int) remoteobs.Observation {
	item := remoteobs.Observation{Version: remoteobs.Version, ID: siteObservationID(),
		AssetID: target.AssetID, Engine: target.Engine, Backlog: backlog}
	password, err := remoteobs.ReadFile(target.PasswordFile, 8192)
	if err != nil {
		item.Failure = "credential_unavailable"
		item.ObservedAt = time.Now().UTC()
		return item
	}
	runtime := target.Engine
	var adapter collectoradapter.Adapter
	switch target.Engine {
	case "mysql":
		adapter = collector.MySQLAdapter{}
	case "redis":
		adapter = collector.RedisAdapter{}
	default:
		adapter = collector.PostgreSQLAdapter{}
		runtime = "managed"
	}
	ctx, cancel := context.WithTimeout(ctx, 26*time.Second)
	defer cancel()
	snapshot, err := adapter.Collect(ctx, collector.Target{Host: target.Host, Port: target.Port, Database: target.Database,
		Username: target.Username, SSLMode: target.SSLMode, Runtime: runtime}, strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	item.ObservedAt = time.Now().UTC()
	if err != nil || snapshot.Metrics == nil {
		item.Failure = "target_unavailable"
		return item
	}
	snapshot.Metrics["adapter"] = snapshot.Report
	wire, err := json.Marshal(snapshot.Metrics)
	if err != nil || len(wire) > remoteobs.MaxBody-2048 {
		item.Failure = "target_unavailable"
		return item
	}
	item.Metrics = wire
	return item
}

func deliverObservation(ctx context.Context, client *http.Client, endpoint string, pending remoteobs.Pending, count int) (bool, error) {
	var item remoteobs.Observation
	if remoteobs.Decode(bytes.NewReader(pending.Data), &item) != nil {
		return false, remoteobs.ErrInvalid
	}
	if item.ObservedAt.Before(time.Now().Add(-remoteobs.MaxReplayAge)) {
		return true, errors.New("expired observation discarded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(pending.Data))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-DbSphere-Backlog", fmt.Sprint(count-1))
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("site collector endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("site collector endpoint returned HTTP %d", response.StatusCode)
	}
	var receipt remoteobs.Receipt
	if remoteobs.Decode(io.LimitReader(response.Body, 1024), &receipt) != nil || receipt.ID != item.ID ||
		(receipt.Status != "stored" && receipt.Status != "duplicate") {
		return false, errors.New("invalid site collector receipt")
	}
	return true, nil
}

func runSiteCollector(path string, log *slog.Logger) error {
	cfg, err := readSiteCollectorConfig(path)
	if err != nil {
		return err
	}
	spool, err := remoteobs.OpenSpool(cfg.SpoolDirectory, cfg.MaxBytes, cfg.MaxFiles)
	if err != nil {
		return err
	}
	defer spool.Close()
	tlsConfig, err := remoteobs.LoadTLS(cfg.CertificateFile, cfg.KeyFile, cfg.CAFile, false)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig, transport.Proxy = tlsConfig, nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	nextCollection := time.Now()
	nextDelivery := time.Now()
	for ctx.Err() == nil {
		if !time.Now().Before(nextDelivery) {
			for range 16 {
				pending, count, err := spool.Next()
				if err != nil {
					return err
				}
				if count == 0 {
					break
				}
				done, err := deliverObservation(ctx, client, cfg.Endpoint, pending, count)
				if err != nil && ctx.Err() == nil {
					log.Warn("site collector delivery", "error", err, "queued", count)
				}
				if !done {
					break
				}
				if err := spool.Remove(pending); err != nil {
					return err
				}
			}
			nextDelivery = time.Now().Add(2 * time.Second)
		}
		if !time.Now().Before(nextCollection) && ctx.Err() == nil {
			nextCollection = time.Now().Add(interval)
			for _, target := range cfg.Targets {
				if ctx.Err() != nil {
					break
				}
				_, count, err := spool.Next()
				if err != nil {
					return err
				}
				if count >= cfg.MaxFiles {
					log.Warn("site collector spool quota reached", "queued", count)
					break
				}
				item := collectSiteTarget(ctx, target, count)
				wire, err := json.Marshal(item)
				if err != nil {
					return err
				}
				if err := spool.Add(item, wire); errors.Is(err, remoteobs.ErrSpoolFull) {
					log.Warn("site collector spool byte quota reached", "queued", count)
					break
				} else if err != nil {
					return err
				}
			}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}
