package collector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresAdapterProfilesAndTLS(t *testing.T) {
	for _, name := range []string{"DBSPHERE_POSTGRES_CA_FILE", "DBSPHERE_POSTGRES_CA_PEM_BASE64", "DBSPHERE_RDS_CA_FILE", "DBSPHERE_RDS_CA_PEM_BASE64"} {
		t.Setenv(name, "")
	}
	tests := []struct{ host, runtime, profile string }{
		{"pg.internal", "linux", "postgresql"},
		{"pg.example.com", "managed", "managed-postgresql"},
		{"db.Fixture.rds.amazonaws.com.", "managed", "aws-rds-postgresql"},
		{"db.fixture.rds.amazonaws.com.cn", "managed", "aws-rds-postgresql"},
		{"db.fixture.rds.amazonaws.com.example.com", "managed", "managed-postgresql"},
	}
	for _, test := range tests {
		target := Target{Host: test.host, Port: 5432, Database: "postgres", Username: "monitor", Runtime: test.runtime, SSLMode: "disable"}
		if profile := postgresAdapterProfile(target); profile != test.profile {
			t.Fatalf("profile for %s: %s", test.host, profile)
		}
		if _, err := readOnlyPostgresConfig(target, ""); (err != nil) != (test.profile == "aws-rds-postgresql") {
			t.Fatalf("unexpected TLS requirement for %s", test.host)
		}
	}
	target := Target{Host: "fixture.internal", Port: 5432, SSLMode: "require"}
	config, err := readOnlyPostgresConfig(target, "")
	if err != nil || config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != target.Host || len(config.Fallbacks) != 0 {
		t.Fatal("PostgreSQL TLS must verify the hostname without a plaintext fallback")
	}
	t.Setenv("DBSPHERE_POSTGRES_CA_PEM_BASE64", "corrupt-cert-bundle")
	if _, err := readOnlyPostgresConfig(target, ""); err == nil {
		t.Fatal("invalid PostgreSQL CA was accepted")
	}
}

// TestLiveManagedPostgresAdapter exercises the same read-only collector as an
// enrolled asset. Set the four URLs described in docs/MANAGED_POSTGRESQL.md to
// run the acceptance test against a dedicated test service.
func TestLiveManagedPostgresAdapter(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_MANAGED_PRIMARY_URL") == "" {
		t.Skip("managed PostgreSQL acceptance fixture is not configured")
	}
	provider := os.Getenv("DBSPHERE_TEST_MANAGED_PROFILE")
	if provider != "managed-postgresql" && provider != "aws-rds-postgresql" {
		t.Fatal("set the expected managed PostgreSQL profile")
	}
	for _, test := range []struct{ name, role, signal, status string }{
		{"PRIMARY", "primary", "replicationLinks", "ok"},
		{"REPLICA", "standby", "walReceiver", "ok"},
		{"LIMITED", "primary", "activeWaits", "limited_permissions"},
		{"UNAVAILABLE", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := os.Getenv("DBSPHERE_TEST_MANAGED_" + test.name + "_URL")
			if dsn == "" {
				t.Fatal("missing managed PostgreSQL fixture URL")
			}
			config, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatal("invalid managed PostgreSQL fixture URL")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			target := Target{Host: config.Host, Port: int(config.Port), Database: config.Database,
				Username: config.User, SSLMode: "require", Runtime: "managed"}
			sample, err := (PostgreSQLAdapter{}).Collect(ctx, target, config.Password)
			if test.role == "" {
				if err == nil {
					t.Fatal("collector accepted the unavailable or invalid-host fixture")
				}
				return
			}
			if err != nil {
				t.Fatal("managed PostgreSQL collection failed")
			}
			report := sample.Report
			if report.Contract != 1 || report.Engine != "postgresql" || report.Profile != provider || report.Role != test.role ||
				!report.Transport.Verified || !report.Transport.Encrypted || !report.Transport.ReadOnly || sample.Metrics["privileged_role"] != false {
				t.Fatalf("unexpected role, privileges, or transport: %+v", report)
			}
			if provider == "aws-rds-postgresql" && report.ProviderStatus != "provider_signals_observed" {
				t.Fatal("AWS RDS provider feature was not observed")
			}
			for _, signal := range report.Signals {
				if signal.Key == test.signal && signal.Status == test.status {
					return
				}
			}
			t.Fatalf("missing capability %s/%s", test.signal, test.status)
		})
	}
}
