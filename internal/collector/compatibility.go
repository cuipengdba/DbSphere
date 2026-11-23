package collector

import (
	"strings"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
)

type postgresSignal = collectoradapter.Signal

func sampledSignal(key, source string, result map[string]any) postgresSignal {
	status, _ := result["status"].(string)
	reason, _ := result["reason"].(string)
	if status == "unavailable" && strings.HasSuffix(reason, "SQLSTATE 42501") {
		status = "limited_permissions"
	}
	return postgresSignal{Key: key, Source: source, Status: status, Reason: reason}
}

func postgresCompatibility(major int, recovery, readStats bool, diagnostics, performance map[string]any) map[string]any {
	lifecycle := "supported"
	if major == 14 {
		lifecycle = "upstream_eol"
	}
	checkpointSource := "pg_stat_bgwriter"
	if major >= 17 {
		checkpointSource = "pg_stat_checkpointer"
	}
	fromDiagnostic := func(key, source string) postgresSignal {
		result, _ := diagnostics[key].(map[string]any)
		return sampledSignal(key, source, result)
	}
	waits := fromDiagnostic("activeWaits", "pg_stat_activity")
	if !readStats {
		waits.Status, waits.Reason = "limited_permissions", "pg_read_all_stats privileges are not active"
	}
	io := fromDiagnostic("instanceIO", "pg_stat_io")
	if major < 16 {
		io.Status, io.Reason = "not_supported", "pg_stat_io is available from PostgreSQL 16"
	}
	replication := fromDiagnostic("replicationLinks", "pg_stat_replication")
	if recovery {
		replication = fromDiagnostic("walReceiver", "pg_stat_wal_receiver")
		if replication.Reason == "query returned no data" {
			replication.Status, replication.Reason = "inactive", "no active WAL receiver"
		}
	}
	if !readStats && (replication.Status == "ok" || replication.Status == "inactive") {
		replication.Status, replication.Reason = "limited_permissions", "pg_read_all_stats privileges are not active"
	}
	topSQL := sampledSignal("sqlPerformance", "pg_stat_statements", performance)
	if !readStats && topSQL.Status == "ok" {
		topSQL.Status, topSQL.Reason = "limited_permissions", "query IDs are limited to the current role"
	}
	logicalErrors := fromDiagnostic("logicalSubscriptionErrors", "pg_stat_subscription_stats")
	if major < 15 {
		logicalErrors.Status, logicalErrors.Reason = "not_supported", "pg_stat_subscription_stats is available from PostgreSQL 15"
	}
	signals := []postgresSignal{waits, io, fromDiagnostic("checkpoints", checkpointSource),
		fromDiagnostic("wal", "pg_stat_wal"), replication, topSQL,
		fromDiagnostic("logicalPublications", "pg_publication"),
		fromDiagnostic("logicalSubscriptions", "pg_subscription / pg_stat_subscription"),
		fromDiagnostic("logicalApplyProgress", "pg_replication_origin_status"), logicalErrors}
	if major < 14 {
		signals = append(signals, postgresSignal{Key: "activityQueryID", Source: "pg_stat_activity", Status: "not_supported",
			Reason: "this kernel does not expose a query ID in pg_stat_activity"})
	}
	return map[string]any{"major": major, "lifecycle": lifecycle, "signals": signals}
}
