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
	bad = input
	bad.PoolerURL = "http://127.0.0.1/admin"
	if bad.Validate() == nil {
		t.Fatal("unrelated pooler URL accepted")
	}
}
