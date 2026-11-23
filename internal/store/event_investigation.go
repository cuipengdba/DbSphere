package store

import (
	"math"
	"time"
)

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
