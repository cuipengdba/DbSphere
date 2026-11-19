package assets

import (
	"encoding/json"
	"testing"
)

func TestAssetInputValidationAndBrowserPort(t *testing.T) {
	var input Input
	if err := json.Unmarshal([]byte(`{"name":"pg","host":"pg.internal","port":5432,
		"database":"postgres","username":"monitor","password":"secret",
		"sslMode":"require","runtime":"linux","linuxHost":"pg-node",
		"linuxExporterPort":"9100"}`), &input); err != nil || input.Validate() != nil {
		t.Fatalf("valid browser asset rejected: %v", err)
	}
	if input.LinuxExporterPort != 9100 {
		t.Fatal("browser exporter port was not parsed")
	}
	bad := input
	bad.Host = "pg.internal;DROP TABLE assets"
	if bad.Validate() == nil {
		t.Fatal("unsafe host accepted")
	}
	bad = input
	bad.Runtime = "k8s"
	if bad.Validate() == nil {
		t.Fatal("Kubernetes asset without scope accepted")
	}
	managed := input
	managed.Runtime = "managed"
	managed.LinuxHost = ""
	managed.LinuxExporterPort = 0
	if err := managed.Validate(); err != nil {
		t.Fatalf("managed PostgreSQL rejected: %v", err)
	}
	managed.LinuxHost = "stale-host"
	if managed.Validate() == nil {
		t.Fatal("managed PostgreSQL accepted a misleading Linux mapping")
	}
	bad = input
	bad.PoolerURL = "http://127.0.0.1/admin"
	if bad.Validate() == nil {
		t.Fatal("unrelated pooler URL accepted")
	}
	withPooler := input
	withPooler.PoolerEndpoint = "pooler-app.internal:6543"
	if err := withPooler.Validate(); err != nil {
		t.Fatalf("separate PgBouncer application endpoint rejected: %v", err)
	}
	if host, port, err := ParsePoolerEndpoint("[2001:db8::1]:6543"); err != nil || host != "2001:db8::1" || port != 6543 {
		t.Fatalf("IPv6 PgBouncer application endpoint was misread: %q %d %v", host, port, err)
	}
	for _, endpoint := range []string{"postgres://user:secret@pooler:6432/pgbouncer", "pooler.internal",
		"pooler.internal:0", "pooler.internal:65536", "pooler.internal:6432/path",
		"user@pooler.internal:6432", "pooler.internal:6432?token=secret", "[invalid:ip]:6432"} {
		bad = input
		bad.PoolerEndpoint = endpoint
		if bad.Validate() == nil {
			t.Fatalf("unsafe PgBouncer application endpoint accepted: %s", endpoint)
		}
	}
	withPatroni := input
	withPatroni.PatroniURL = "https://reader:secret@patroni.internal:8008"
	if err := withPatroni.Validate(); err != nil {
		t.Fatalf("valid read-only Patroni endpoint rejected: %v", err)
	}
	for _, endpoint := range []string{"file:///etc/passwd", "http://patroni:8008/failover",
		"http://patroni:8008?token=secret", "https://:secret@patroni:8008"} {
		bad = input
		bad.PatroniURL = endpoint
		if bad.Validate() == nil {
			t.Fatalf("unsafe Patroni endpoint accepted: %s", endpoint)
		}
	}
}
