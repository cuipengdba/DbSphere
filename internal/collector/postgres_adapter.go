package collector

import (
	"context"
	"crypto/tls"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

func postgresAdapterProfile(target Target) string {
	if target.Runtime != "managed" {
		return "postgresql"
	}
	host := strings.TrimSuffix(strings.ToLower(target.Host), ".")
	if strings.HasSuffix(host, ".rds.amazonaws.com") || strings.HasSuffix(host, ".rds.amazonaws.com.cn") {
		return "aws-rds-postgresql"
	}
	return "managed-postgresql"
}

func postgresAdapterReport(ctx context.Context, target Target, conn *pgx.Conn, version int, role string, readOnly bool,
	compatibility map[string]any) collectoradapter.Report {
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "postgresql",
		Profile: postgresAdapterProfile(target), Version: version, Role: role, Signals: []collectoradapter.Signal{}}
	if channel, ok := conn.PgConn().Conn().(*tls.Conn); ok {
		state := channel.ConnectionState()
		report.Transport.Encrypted = state.HandshakeComplete
		report.Transport.Verified = len(state.VerifiedChains) > 0
		report.Transport.Protocol = tls.VersionName(state.Version)
	}
	report.Transport.ReadOnly = readOnly
	if signals, ok := compatibility["signals"].([]postgresSignal); ok {
		for _, signal := range signals {
			report.Signals = append(report.Signals, collectoradapter.Signal(signal))
		}
	}
	if target.Runtime != "managed" {
		return report
	}
	if report.Profile == "aws-rds-postgresql" {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var rdsSetting bool
		err := conn.QueryRow(probe, `SELECT EXISTS(SELECT 1 FROM pg_settings WHERE name='rds.force_ssl')`).Scan(&rdsSetting)
		report.ProviderStatus = "unverified"
		if err == nil && rdsSetting && report.Transport.Verified && readOnly {
			report.ProviderStatus = "provider_signals_observed"
		}
	}
	report.Signals = append(report.Signals, collectoradapter.Signal{Key: "hostMetrics", Source: "deployment", Status: "not_supported",
		Reason: "managed instance host metrics require a separate provider source"})
	return report
}
