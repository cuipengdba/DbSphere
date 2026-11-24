package store

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
)

type investigationPeak struct {
	replayComparison
	Signal    string    `json:"signal"`
	Value     float64   `json:"value"`
	Identity  string    `json:"identity,omitempty"`
	Observed  int       `json:"observed"`
	ScannedTo time.Time `json:"scannedTo"`
	Limited   bool      `json:"limited"`
}

// Compare a bounded interval of raw samples for the triggering signal. Claim
// and container names can be reused, so these comparisons require their IDs.
func investigationPeakSignal(metric string, anchor sampleObservation) (string, func(sampleObservation) (float64, bool)) {
	switch metric {
	case "pg.blocked_sessions", "pg.replication_lag_bytes", "pgbouncer.waiting_clients":
		return "", func(sample sampleObservation) (float64, bool) {
			signal := readGoSignal(metric, sample.Metrics, sample.At)
			return signal.Value, signal.Available
		}
	case "k8s.pvc_used_percent":
		signal := readGoSignal(metric, anchor.Metrics, anchor.At)
		name := stringValue(object(signal.Detail)["pvc"])
		var uid string
		for _, raw := range list(object(anchor.Metrics["k8s"])["pvcs"]) {
			claim := object(raw)
			if claim["name"] == name {
				uid = stringValue(claim["uid"])
				break
			}
		}
		if signal.Available && uid != "" {
			return "PVC " + name + " · " + uid, func(sample sampleObservation) (float64, bool) {
				kube := object(sample.Metrics["k8s"])
				if kube["status"] != "ok" || kube["pvcError"] != nil {
					return 0, false
				}
				for _, raw := range list(kube["pvcs"]) {
					claim := object(raw)
					if claim["uid"] != uid || claim["name"] != name {
						continue
					}
					usage := object(claim["usage"])
					used, usedOK := numeric(usage["usedBytes"])
					capacity, capacityOK := numeric(usage["capacityBytes"])
					if usage["status"] == "ok" && usedOK && capacityOK && used >= 0 && capacity > 0 {
						return 100 * used / capacity, true
					}
				}
				return 0, false
			}
		}
	}
	if code, ok := strings.CutPrefix(metric, "container."); ok && storageFindings[code] != "" {
		detail := object(containerStorageAlert(anchor.Metrics, code).Detail)
		if id := stringValue(detail["containerId"]); id != "" {
			return "容器 " + id, func(sample sampleObservation) (float64, bool) {
				finding := object(containerStorageAlert(sample.Metrics, code).Detail)
				value, ok := numeric(finding["ioPressure"])
				return value, ok && finding["containerId"] == id
			}
		}
	}
	return "", nil
}

func (s *Postgres) incidentPeak(ctx context.Context, event *eventRecord, detection *replayPoint, anchor sampleObservation) (*investigationPeak, error) {
	if detection.Strength != "direct" {
		return nil, nil
	}
	identity, measurement := investigationPeakSignal(detection.Signal, anchor)
	if measurement == nil {
		return nil, nil
	}
	const limit = 120
	rows, err := s.pool.Query(ctx, `SELECT id::text,collected_at,status,metrics_json FROM samples
		WHERE asset_id=$1 AND collected_at >= $2 AND collected_at < coalesce($3::timestamptz, now())
		ORDER BY collected_at,id LIMIT $4`, event.AssetID, anchor.At, event.EndedAt, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	peak := &investigationPeak{Identity: identity, Signal: detection.Signal}
	if strings.HasPrefix(detection.Signal, "container.") {
		peak.Signal = "container.io_pressure"
	}
	for i := 0; rows.Next(); i++ {
		if i == limit {
			peak.Limited = true
			break
		}
		var sample sampleObservation
		var raw []byte
		if err := rows.Scan(&sample.ID, &sample.At, &sample.Status, &raw); err != nil {
			return nil, err
		}
		peak.ScannedTo = sample.At
		if json.Unmarshal(raw, &sample.Metrics) != nil {
			continue
		}
		if value, ok := measurement(sample); ok {
			peak.Observed++
			if peak.SampleID == "" || value > peak.Value {
				peak.Value, peak.replayComparison = value, compareReplaySample("peak", sample)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if peak.SampleID == "" {
		return nil, nil
	}
	return peak, nil
}

type investigationBlocker struct {
	WaitingPID     int    `json:"waitingPid"`
	BlockerPID     int    `json:"blockerPid"`
	WaitingQueryID string `json:"waitingQueryId,omitempty"`
	BlockerQueryID string `json:"blockerQueryId,omitempty"`
}

type investigationLocks struct {
	At         time.Time              `json:"at"`
	SampleID   string                 `json:"sampleId"`
	EvidenceID string                 `json:"evidenceId"`
	Pairs      []investigationBlocker `json:"pairs"`
}

// The investigation shortcut only shows lock relationships observed by a
// firing lock-wait rule on this event. A coincident sample, even one with a
// valid lock graph, cannot stand in for that event's trigger.
func investigationLockEvidence(points []replayPoint, samples map[string]sampleObservation) *investigationLocks {
	for _, point := range points {
		if point.Kind != "alert_firing" || point.Signal != "pg.blocked_sessions" ||
			point.Strength != "direct" || point.EvidenceID == "" {
			continue
		}
		sample, exists := samples[point.SampleID]
		if !exists {
			continue
		}
		chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
		if chain["status"] != "ok" {
			continue
		}
		seen := map[[2]int]bool{}
		pairs := []investigationBlocker{}
		for _, raw := range list(chain["data"]) {
			row := object(raw)
			waiting, waitingOK := numeric(row["waitingPid"])
			blocker, blockerOK := numeric(row["blockerPid"])
			if !waitingOK || !blockerOK || waiting < 1 || blocker < 1 || waiting > math.MaxInt32 || blocker > math.MaxInt32 ||
				waiting == blocker || waiting != math.Trunc(waiting) || blocker != math.Trunc(blocker) {
				continue
			}
			key := [2]int{int(waiting), int(blocker)}
			if seen[key] {
				continue
			}
			seen[key] = true
			pairs = append(pairs, investigationBlocker{WaitingPID: key[0], BlockerPID: key[1],
				WaitingQueryID: stringValue(row["waitingQueryId"]), BlockerQueryID: stringValue(row["blockerQueryId"])})
			if len(pairs) == 12 {
				break
			}
		}
		if len(pairs) != 0 {
			return &investigationLocks{At: sample.At, SampleID: sample.ID, EvidenceID: point.EvidenceID, Pairs: pairs}
		}
	}
	return nil
}
