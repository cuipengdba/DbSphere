package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type readinessSample struct {
	ID      string         `json:"id"`
	At      time.Time      `json:"at"`
	Status  string         `json:"status"`
	Metrics map[string]any `json:"metrics"`
	Error   string         `json:"error"`
}

type readinessCheck struct {
	Key      string `json:"key"`
	Status   string `json:"status"`
	Action   string `json:"action,omitempty"`
	Previous string `json:"previousStatus,omitempty"`
}

type readinessReport struct {
	AssetID          string           `json:"assetId"`
	Runtime          string           `json:"runtime"`
	SSLMode          string           `json:"sslMode"`
	Status           string           `json:"status"`
	Product          string           `json:"product,omitempty"`
	SampleID         string           `json:"sampleId,omitempty"`
	SampledAt        *time.Time       `json:"sampledAt,omitempty"`
	PreviousSampleID string           `json:"previousSampleId,omitempty"`
	Checks           []readinessCheck `json:"checks"`
}

// Use the asset's current sampling boundary so editing a connection cannot
// present the old target's readiness as a result for the new configuration.
func (s *Postgres) assetReadiness(ctx context.Context, id string, at *time.Time) (readinessReport, error) {
	report := readinessReport{AssetID: id, Status: "waiting", Checks: []readinessCheck{}}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT a.runtime,a.ssl_mode,COALESCE((
		SELECT jsonb_agg(jsonb_build_object('id',i.id,'at',i.collected_at,'status',i.status,
			'metrics',i.metrics_json,'error',i.error) ORDER BY i.collected_at DESC,i.id DESC)
		FROM (SELECT id,collected_at,status,metrics_json,error FROM samples
			WHERE asset_id=a.id AND collected_at<=COALESCE($2::timestamptz,a.last_sample_at)
			ORDER BY collected_at DESC,id DESC LIMIT 2) i),'[]'::jsonb)
		FROM assets a WHERE a.id=$1`, id, at).Scan(&report.Runtime, &report.SSLMode, &raw)
	if err != nil {
		return report, err
	}
	var samples []readinessSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return report, err
	}
	if len(samples) == 0 {
		return report, nil
	}
	current := samples[0]
	adapter := object(current.Metrics["adapter"])
	report.SampleID, report.SampledAt = current.ID, &current.At
	if adapter["product"] == "highgo" {
		report.Product = "highgo"
	}
	report.Checks = describeReadiness(current, report.Runtime)
	report.Status = "ready"
	if report.Checks[0].Status != "ok" {
		report.Status = "failed"
	} else {
		for _, check := range report.Checks {
			if check.Status == "failed" || check.Status == "limited" || check.Status == "unknown" {
				report.Status = "limited"
				break
			}
		}
	}
	if len(samples) > 1 {
		report.PreviousSampleID = samples[1].ID
		states := map[string]string{}
		for _, check := range describeReadiness(samples[1], report.Runtime) {
			states[check.Key] = check.Status
		}
		for i, check := range report.Checks {
			if old := states[check.Key]; old != check.Status {
				report.Checks[i].Previous = old
			}
		}
	}
	return report, nil
}

func describeReadiness(sample readinessSample, runtime string) []readinessCheck {
	metrics := sample.Metrics
	pgStatus := stringValue(object(metrics["collector"])["pgStatus"])
	if pgStatus == "" && runtime == "mysql" {
		pgStatus = stringValue(object(metrics["collector"])["mysqlStatus"])
	}
	if pgStatus == "" && sample.Status == "ok" {
		pgStatus = "ok"
	}
	checks := []readinessCheck{}
	add := func(key, status, action string) {
		checks = append(checks, readinessCheck{Key: key, Status: status, Action: action})
	}
	switch {
	case pgStatus == "ok":
		add("connection", "ok", "")
	case strings.Contains(sample.Error, "authentication failed"):
		add("connection", "failed", "check_credentials")
	case strings.Contains(sample.Error, "TLS"):
		add("connection", "failed", "check_tls")
	case strings.Contains(sample.Error, "SQLSTATE 42501"):
		add("connection", "limited", "check_database_access")
	case pgStatus == "availability":
		add("connection", "failed", "check_endpoint")
	default:
		add("connection", "unknown", "check_sample")
	}
	adapter := object(metrics["adapter"])
	transport := object(adapter["transport"])
	switch {
	case transport["verified"] == true:
		add("tls", "ok", "")
	case transport["encrypted"] == true:
		add("tls", "encrypted", "check_tls_identity")
	case transport != nil:
		add("tls", "inactive", "")
	default:
		add("tls", "unknown", "")
	}
	if readOnly, observed := transport["readOnly"].(bool); observed && readOnly {
		add("readOnly", "ok", "")
	} else if observed {
		add("readOnly", "failed", "check_read_only")
	} else {
		add("readOnly", "unknown", "")
	}
	for _, item := range list(adapter["signals"]) {
		signal := object(item)
		switch signal["key"] {
		case "activeWaits", "activityQueryID", "instanceIO", "checkpoints", "wal", "replicationLinks", "walReceiver",
			"logicalPublications", "logicalSubscriptions", "logicalApplyProgress", "logicalSubscriptionErrors", "sqlPerformance",
			"connections", "lockWaits", "replication":
			switch signal["status"] {
			case "ok", "inactive":
				add(stringValue(signal["key"]), stringValue(signal["status"]), "")
			case "limited_permissions":
				add(stringValue(signal["key"]), "limited", "check_stats_permission")
			case "not_configured":
				add(stringValue(signal["key"]), "not_configured", "check_extension")
			case "not_supported":
				add(stringValue(signal["key"]), "not_applicable", "")
			default:
				add(stringValue(signal["key"]), "unknown", "check_sample")
			}
		}
	}
	for _, key := range []string{"linux", "k8s", "containerStorage", "pgbouncer", "patroni"} {
		layer := object(metrics[key])
		switch layer["status"] {
		case "ok":
			add(key, "ok", "")
		case nil, "not_configured":
			add(key, "not_configured", "")
		default:
			status := "unknown"
			if strings.Contains(stringValue(layer["reason"]), "HTTP 403") {
				status = "limited"
			}
			add(key, status, "check_resource_source")
		}
	}
	kube := object(metrics["k8s"])
	if kube["status"] == "ok" {
		node := object(kube["node"])
		if node["uid"] != nil {
			add("k8sNode", "ok", "")
		} else {
			status := "unknown"
			if kube["nodeError"] == "Kubernetes API HTTP 403" {
				status = "limited"
			}
			add("k8sNode", status, "check_k8s_node")
		}
		claims := list(kube["pvcs"])
		status := "ok"
		if len(claims) == 0 {
			status = "inactive"
		}
		if kube["pvcError"] != nil {
			status = "unknown"
		}
		for _, item := range claims {
			claim := object(item)
			if claim["error"] == "Kubernetes API HTTP 403" {
				status = "limited"
				break
			}
			if claim["status"] != "ok" {
				status = "unknown"
			}
		}
		action := ""
		if status == "unknown" || status == "limited" {
			action = "check_k8s_pvc"
		}
		add("k8sPVC", status, action)
	}
	return checks
}
