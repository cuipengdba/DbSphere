package config

import (
	"encoding/base64"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var assetIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	MasterKey      []byte
	AdminPass      string
	ViewerPass     string
	ViewerAssetIDs []string
	AutoCollect    bool
	CollectEvery   int
	CollectWorkers int
	OTLPToken      string
	TraceDays      int
}

func Load() (Config, error) {
	url := os.Getenv("DBSPHERE_DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("DBSPHERE_DATABASE_URL must point to an independent PostgreSQL 18 database")
	}
	password := os.Getenv("DBSPHERE_ADMIN_PASSWORD")
	if len(password) < 12 {
		return Config{}, errors.New("DBSPHERE_ADMIN_PASSWORD must contain at least 12 characters")
	}
	viewer := os.Getenv("DBSPHERE_VIEWER_PASSWORD")
	if viewer != "" && (len(viewer) < 12 || viewer == password) {
		return Config{}, errors.New("DBSPHERE_VIEWER_PASSWORD must be distinct and contain at least 12 characters")
	}
	var viewerAssetIDs []string
	if raw := os.Getenv("DBSPHERE_VIEWER_ASSET_IDS"); raw != "" {
		if viewer == "" {
			return Config{}, errors.New("DBSPHERE_VIEWER_ASSET_IDS requires DBSPHERE_VIEWER_PASSWORD")
		}
		seen := map[string]bool{}
		for _, part := range strings.Split(raw, ",") {
			id := strings.ToLower(strings.TrimSpace(part))
			if !assetIDPattern.MatchString(id) {
				return Config{}, errors.New("DBSPHERE_VIEWER_ASSET_IDS must contain comma-separated UUIDs")
			}
			if !seen[id] {
				viewerAssetIDs = append(viewerAssetIDs, id)
				seen[id] = true
			}
		}
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("DBSPHERE_MASTER_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("DBSPHERE_MASTER_KEY must be a base64-encoded 32-byte key")
	}
	repeated := true
	for _, value := range key[1:] {
		if value != key[0] {
			repeated = false
			break
		}
	}
	if repeated {
		return Config{}, errors.New("DBSPHERE_MASTER_KEY must not use a repeated-byte placeholder")
	}
	host := os.Getenv("DBSPHERE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 8080
	if value := os.Getenv("DBSPHERE_PORT"); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, errors.New("DBSPHERE_PORT must be between 1 and 65535")
		}
	}
	interval := 60
	if value := os.Getenv("DBSPHERE_COLLECT_INTERVAL_SECONDS"); value != "" {
		interval, err = strconv.Atoi(value)
		if err != nil || interval < 15 || interval > 3600 {
			return Config{}, errors.New("DBSPHERE_COLLECT_INTERVAL_SECONDS must be between 15 and 3600")
		}
	}
	workers := 4
	if value := os.Getenv("DBSPHERE_COLLECT_WORKERS"); value != "" {
		workers, err = strconv.Atoi(value)
		if err != nil || workers < 1 || workers > 16 {
			return Config{}, errors.New("DBSPHERE_COLLECT_WORKERS must be between 1 and 16")
		}
	}
	collectorFlag := os.Getenv("DBSPHERE_ENABLE_GO_COLLECTOR")
	if collectorFlag != "" && collectorFlag != "true" && collectorFlag != "false" {
		return Config{}, errors.New("DBSPHERE_ENABLE_GO_COLLECTOR must be true or false")
	}
	autoCollect := collectorFlag == "true"
	otlpToken := os.Getenv("DBSPHERE_OTLP_TOKEN")
	if otlpToken != "" && len(otlpToken) < 32 {
		return Config{}, errors.New("DBSPHERE_OTLP_TOKEN must contain at least 32 characters")
	}
	traceDays := 7
	if value := os.Getenv("DBSPHERE_TRACE_RETENTION_DAYS"); value != "" {
		traceDays, err = strconv.Atoi(value)
		if err != nil || traceDays < 1 || traceDays > 3650 {
			return Config{}, errors.New("DBSPHERE_TRACE_RETENTION_DAYS must be between 1 and 3650")
		}
	}
	return Config{ListenAddr: net.JoinHostPort(host, strconv.Itoa(port)),
		DatabaseURL: url, MasterKey: key, AdminPass: password, ViewerPass: viewer,
		ViewerAssetIDs: viewerAssetIDs,
		AutoCollect:    autoCollect, CollectEvery: interval, CollectWorkers: workers,
		OTLPToken: otlpToken, TraceDays: traceDays}, nil
}
