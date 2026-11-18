package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadRejectsMissingStorageAndWeakSecrets(t *testing.T) {
	t.Setenv("DBSPHERE_COLLECT_WORKERS", "")
	t.Setenv("DBSPHERE_DATABASE_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PostgreSQL 18") {
		t.Fatalf("missing PG18 storage should fail clearly: %v", err)
	}
	t.Setenv("DBSPHERE_DATABASE_URL", "postgres://example.invalid/dbsphere")
	t.Setenv("DBSPHERE_ADMIN_PASSWORD", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ADMIN_PASSWORD") {
		t.Fatalf("weak admin password should fail: %v", err)
	}
	t.Setenv("DBSPHERE_ADMIN_PASSWORD", "long-test-password")
	t.Setenv("DBSPHERE_VIEWER_PASSWORD", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_PASSWORD") {
		t.Fatalf("weak viewer password was accepted: %v", err)
	}
	t.Setenv("DBSPHERE_VIEWER_PASSWORD", "long-test-password")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_PASSWORD") {
		t.Fatalf("shared admin/viewer password was accepted: %v", err)
	}
	t.Setenv("DBSPHERE_VIEWER_PASSWORD", "viewer-test-password")
	t.Setenv("DBSPHERE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("short master key should fail: %v", err)
	}
	t.Setenv("DBSPHERE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("repeated-byte master key should fail: %v", err)
	}
	t.Setenv("DBSPHERE_MASTER_KEY", testMasterKey())
	t.Setenv("DBSPHERE_HOST", "127.0.0.1")
	t.Setenv("DBSPHERE_PORT", "8081")
	cfg, err := Load()
	if err != nil || cfg.ListenAddr != "127.0.0.1:8081" {
		t.Fatalf("valid config should load: %+v, %v", cfg, err)
	}
	t.Setenv("DBSPHERE_COLLECT_INTERVAL_SECONDS", "10")
	if _, err := Load(); err == nil {
		t.Fatal("collection interval below 15 seconds was accepted")
	}
	t.Setenv("DBSPHERE_COLLECT_INTERVAL_SECONDS", "60")
	t.Setenv("DBSPHERE_ENABLE_GO_COLLECTOR", "true")
	if cfg, err := Load(); err != nil || !cfg.AutoCollect || cfg.CollectEvery != 60 || cfg.CollectWorkers != 4 {
		t.Fatalf("automatic Go collection settings were not loaded: %+v %v", cfg, err)
	}
	t.Setenv("DBSPHERE_COLLECT_WORKERS", "17")
	if _, err := Load(); err == nil {
		t.Fatal("unbounded collector worker count was accepted")
	}
	t.Setenv("DBSPHERE_COLLECT_WORKERS", "3")
	if cfg, err := Load(); err != nil || cfg.CollectWorkers != 3 {
		t.Fatalf("bounded collector worker count was not loaded: %+v %v", cfg, err)
	}
	t.Setenv("DBSPHERE_OTLP_TOKEN", "short")
	if _, err := Load(); err == nil {
		t.Fatal("short OTLP write token was accepted")
	}
	t.Setenv("DBSPHERE_OTLP_TOKEN", strings.Repeat("t", 32))
	t.Setenv("DBSPHERE_TRACE_RETENTION_DAYS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("invalid trace retention was accepted")
	}
	t.Setenv("DBSPHERE_TRACE_RETENTION_DAYS", "14")
	if cfg, err := Load(); err != nil || cfg.TraceDays != 14 || len(cfg.OTLPToken) != 32 {
		t.Fatalf("OTLP configuration was not loaded: %+v %v", cfg, err)
	}
}

func TestViewerAssetScopeRequiresValidIDsAndPassword(t *testing.T) {
	t.Setenv("DBSPHERE_DATABASE_URL", "postgres://example.invalid/dbsphere")
	t.Setenv("DBSPHERE_ADMIN_PASSWORD", "admin-test-password")
	t.Setenv("DBSPHERE_MASTER_KEY", testMasterKey())
	t.Setenv("DBSPHERE_VIEWER_ASSET_IDS", "11111111-1111-1111-1111-111111111111")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_ASSET_IDS") {
		t.Fatalf("scope without viewer password was accepted: %v", err)
	}
	t.Setenv("DBSPHERE_VIEWER_PASSWORD", "viewer-test-password")
	t.Setenv("DBSPHERE_VIEWER_ASSET_IDS", "not-a-uuid")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "VIEWER_ASSET_IDS") {
		t.Fatalf("invalid scope was accepted: %v", err)
	}
	t.Setenv("DBSPHERE_VIEWER_ASSET_IDS", "11111111-1111-1111-1111-111111111111, 11111111-1111-1111-1111-111111111111")
	cfg, err := Load()
	if err != nil || len(cfg.ViewerAssetIDs) != 1 {
		t.Fatalf("asset scope was not normalized: %+v %v", cfg.ViewerAssetIDs, err)
	}
}

func testMasterKey() string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(key)
}
