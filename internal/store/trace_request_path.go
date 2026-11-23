package store

import (
	"slices"
	"sort"
	"time"
)

const traceRequestPathLimit = 12

type requestSpanObservation struct {
	observedTraceSpan
	Route         string
	Endpoint      string
	Sampling      *float64
	ReceivedAt    *time.Time
	TimingAnomaly bool
}

type requestPathStep struct {
	Service  string    `json:"service"`
	SpanID   string    `json:"spanId"`
	Kind     string    `json:"kind"`
	At       time.Time `json:"startedAt"`
	Duration int64     `json:"durationMs"`
	Sampling *float64  `json:"samplingProbability"`
	Clock    string    `json:"clock"`
}

type requestPathEvidence struct {
	TraceID        string            `json:"traceId"`
	DatabaseSpanID string            `json:"databaseSpanId"`
	Steps          []requestPathStep `json:"steps"`
	ParentState    string            `json:"parentState"`
	MissingParent  string            `json:"missingParentSpanId,omitempty"`
	ClockAnomaly   bool              `json:"clockAnomaly"`
	BackendState   string            `json:"backendState"`
	BackendPID     int               `json:"backendPid,omitempty"`
	BlockerPID     int               `json:"blockerPid,omitempty"`
	BackendSample  string            `json:"backendSampleId,omitempty"`
	Route          string            `json:"route"`
	PoolerState    string            `json:"poolerState,omitempty"`
	PoolerSample   string            `json:"poolerSampleId,omitempty"`
}

func traceRequestPaths(spans []requestSpanObservation, samples map[string]sampleObservation) ([]requestPathEvidence, bool) {
	indexed := make(map[[2]string]requestSpanObservation, len(spans))
	ordered := make([]sampleObservation, 0, len(samples))
	for _, span := range spans {
		indexed[[2]string{span.TraceID, span.SpanID}] = span
	}
	for _, sample := range samples {
		ordered = append(ordered, sample)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].At.After(ordered[j].At)
	})
	paths := []requestPathEvidence{}
	for _, span := range spans {
		if span.Kind != "postgresql_client" {
			continue
		}
		path := requestPathEvidence{TraceID: span.TraceID, DatabaseSpanID: span.SpanID, ParentState: "complete",
			BackendPID: span.BackendPID, Route: span.Route}
		path.followParents(span, indexed)
		path.attachBackend(span, ordered)
		paths = append(paths, path)
	}
	// Prefer links with database-side evidence while preserving order for equal states.
	sort.SliceStable(paths, func(i, j int) bool {
		return paths[i].BackendState == "matched" && paths[j].BackendState != "matched"
	})
	if len(paths) > traceRequestPathLimit {
		return paths[:traceRequestPathLimit], true
	}
	return paths, false
}

func (p *requestPathEvidence) followParents(span requestSpanObservation, indexed map[[2]string]requestSpanObservation) {
	seen := map[string]bool{}
	for {
		clock := "observed"
		if span.TimingAnomaly {
			clock, p.ClockAnomaly = "unreliable", true
		} else if span.ReceivedAt == nil {
			clock = "unavailable"
		}
		p.Steps = append(p.Steps, requestPathStep{Service: span.Service, SpanID: span.SpanID, Kind: span.Kind,
			At: span.Start, Duration: span.End.Sub(span.Start).Milliseconds(), Sampling: span.Sampling, Clock: clock})
		if span.ParentSpanID == "" {
			if len(p.Steps) == 1 {
				p.ParentState = "database_root"
			}
			break
		}
		seen[span.SpanID] = true
		if seen[span.ParentSpanID] {
			p.ParentState = "cycle"
			break
		}
		if len(p.Steps) >= 32 {
			p.ParentState = "depth_limit"
			break
		}
		parent, ok := indexed[[2]string{span.TraceID, span.ParentSpanID}]
		if !ok {
			p.ParentState, p.MissingParent = "missing_parent", span.ParentSpanID
			break
		}
		span = parent
	}
	slices.Reverse(p.Steps)
}

func (p *requestPathEvidence) attachBackend(span requestSpanObservation, samples []sampleObservation) {
	p.BackendState = "no_lock_evidence"
	if p.BackendPID == 0 {
		p.BackendState = "pid_unreported"
	}
	if span.TimingAnomaly {
		p.BackendState = "time_unreliable"
		if p.Route == "pgbouncer" {
			p.PoolerState = "time_unreliable"
		}
		return
	}
	if p.Route == "pgbouncer" {
		p.PoolerState = "mapped_endpoint"
	}
	for _, sample := range samples {
		if sample.At.Before(span.Start) || sample.At.After(span.End) {
			continue
		}
		pooler := object(sample.Metrics["pgbouncer"])
		if p.Route == "pgbouncer" && p.PoolerSample == "" && stringValue(pooler["status"]) == "ok" {
			p.PoolerState, p.PoolerSample = "pool_sample_observed", sample.ID
		}
		if p.BackendPID == 0 || p.BackendState == "matched" {
			continue
		}
		if blocker := matchedLockWaitBlocker(sample, span.Start, span.End, p.BackendPID); blocker > 0 {
			p.BackendState, p.BlockerPID, p.BackendSample = "matched", blocker, sample.ID
			continue
		}
		chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
		if p.BackendState == "no_lock_evidence" && sample.Status == "ok" &&
			stringValue(chain["status"]) == "ok" && len(list(chain["data"])) != 0 {
			p.BackendState, p.BackendSample = "mismatch", sample.ID
		}
	}
}
