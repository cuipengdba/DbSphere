package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadRejectsMissingStorageAndWeakSecrets(t *testing.T) {
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
	t.Setenv("DBSPHERE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("short master key should fail: %v", err)
	}
	t.Setenv("DBSPHERE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("DBSPHERE_HOST", "127.0.0.1")
	t.Setenv("DBSPHERE_PORT", "8081")
	cfg, err := Load()
	if err != nil || cfg.ListenAddr != "127.0.0.1:8081" {
		t.Fatalf("valid config should load: %+v, %v", cfg, err)
	}
}
