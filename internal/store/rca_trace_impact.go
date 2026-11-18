package store

import (
	"math"
	"sort"
	"time"
)

type observedTraceSpan struct {
	TraceID, SpanID, ParentSpanID, Service, Kind, Status string
	Start, End                                           time.Time
	BackendPID                                           int
}

const (
	traceErrorSignal uint8 = 1 << iota
	traceLockWaitSignal
)

// classifyTraceImpact separates nearby traffic from requests with a concrete
// database error or an exact PG backend PID match. A match is still evidence,
// not proof that this event caused the request failure.
func classifyTraceImpact(spans []observedTraceSpan, samples map[string]sampleObservation,
	impacts map[string]*serviceTraceImpact) {
	byTrace := make(map[string]map[string]observedTraceSpan)
	observed := make(map[string]map[string]bool)
	signals := make(map[string]map[string]uint8)
	for _, span := range spans {
		if byTrace[span.TraceID] == nil {
			byTrace[span.TraceID] = make(map[string]observedTraceSpan)
		}
		byTrace[span.TraceID][span.SpanID] = span
		if observed[span.Service] == nil {
			observed[span.Service] = make(map[string]bool)
		}
		observed[span.Service][span.TraceID] = true
	}
	for _, span := range spans {
		if span.Kind != "postgresql_client" {
			continue
		}
		var flags uint8
		if span.Status == "error" {
			flags |= traceErrorSignal
		}
		if span.BackendPID > 0 && span.End.Sub(span.Start) >= 500*time.Millisecond {
			for _, sample := range samples {
				if matchedLockWaitBlocker(sample, span.Start, span.End, span.BackendPID) > 0 {
					flags |= traceLockWaitSignal
					break
				}
			}
		}
		if flags == 0 {
			continue
		}
		current := span
		visited := map[string]bool{}
		for depth := 0; depth < 32; depth++ {
			if visited[current.SpanID] {
				break
			}
			visited[current.SpanID] = true
			if signals[current.Service] == nil {
				signals[current.Service] = make(map[string]uint8)
			}
			signals[current.Service][span.TraceID] |= flags
			parent, ok := byTrace[span.TraceID][current.ParentSpanID]
			if !ok {
				break
			}
			current = parent
		}
	}
	for service, impact := range impacts {
		impact.ObservedTraces = len(observed[service])
		ids := make([]string, 0, len(signals[service]))
		for traceID, flags := range signals[service] {
			ids = append(ids, traceID)
			if flags&traceErrorSignal != 0 {
				impact.ErrorTraces++
			}
			if flags&traceLockWaitSignal != 0 {
				impact.LockWaitTraces++
			}
		}
		sort.Strings(ids)
		impact.CorrelatedTraces = len(ids)
		if len(ids) > 10 {
			ids = ids[:10]
		}
		impact.TraceIDs = ids
	}
}

func matchedLockWaitBlocker(sample sampleObservation, start, end time.Time, backendPID int) int {
	if backendPID <= 0 || sample.Status != "ok" ||
		sample.At.Before(start.Add(-5*time.Second)) || sample.At.After(end.Add(5*time.Second)) {
		return 0
	}
	chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
	if stringValue(chain["status"]) != "ok" {
		return 0
	}
	for _, pair := range list(chain["data"]) {
		row := object(pair)
		waiting, waitingOK := numeric(row["waitingPid"])
		blocker, blockerOK := numeric(row["blockerPid"])
		if waitingOK && blockerOK && waiting == float64(backendPID) &&
			blocker > 0 && blocker == math.Trunc(blocker) {
			return int(blocker)
		}
	}
	return 0
}
