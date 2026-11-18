package store

import (
	"encoding/json"
	"sort"
	"time"
)

type traceDBSpan struct {
	Node, Parent, Service string
	Start, End            time.Time
}

// traceLockCandidate preserves the application → database span → PG lock sample
// path under one event. The trace has no backend PID, so it is only a lead.
func traceLockCandidate(event *eventRecord, samples map[string]sampleObservation,
	spans []traceDBSpan, spanSet map[string]bool, add graphAdder,
	edges *[]map[string]string) map[string]any {
	sampleIDs := make([]string, 0, len(samples))
	for id := range samples {
		sampleIDs = append(sampleIDs, id)
	}
	sort.Slice(sampleIDs, func(i, j int) bool {
		left, right := samples[sampleIDs[i]], samples[sampleIDs[j]]
		if left.At.Equal(right.At) {
			return sampleIDs[i] < sampleIDs[j]
		}
		return left.At.After(right.At)
	})
	for _, span := range spans {
		if !spanSet[span.Parent] || span.End.Sub(span.Start) < 500*time.Millisecond {
			continue
		}
		for _, id := range sampleIDs {
			sample := samples[id]
			if sample.Status != "ok" || sample.At.Before(span.Start.Add(-time.Minute)) ||
				sample.At.After(span.End.Add(time.Minute)) {
				continue
			}
			chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
			pairs := list(chain["data"])
			if stringValue(chain["status"]) != "ok" || len(pairs) == 0 {
				continue
			}
			code := "trace_lock_correlated"
			title := "应用数据库调用变慢与锁阻塞同期出现"
			candidateNode := "candidate:" + code
			add(candidateNode, "candidate", title, "/api/events/"+event.ID, sample.At)
			supports := []string{span.Parent, span.Node, "sample:" + sample.ID}
			*edges = append(*edges, map[string]string{"from": "event:" + event.ID,
				"to": candidateNode, "relation": "hypothesizes"})
			for _, support := range supports {
				*edges = append(*edges, map[string]string{"from": candidateNode,
					"to": support, "relation": "supported_by"})
			}
			var review any
			for _, evidence := range event.Evidence {
				if evidence.Kind != "rca_decision" {
					continue
				}
				var payload map[string]any
				if json.Unmarshal(evidence.Payload, &payload) == nil && payload["code"] == code {
					review = map[string]any{"decision": payload["decision"], "actor": payload["actor"],
						"reason": payload["reason"], "at": evidence.ObservedAt, "evidenceId": evidence.ID}
				}
			}
			if reviewed := object(review); reviewed != nil {
				*edges = append(*edges, map[string]string{"from": candidateNode,
					"to": "evidence:" + stringValue(reviewed["evidenceId"]), "relation": "reviewed_by"})
			}
			relation := "same_window"
			if sample.At.Before(event.StartedAt) {
				relation = "before_event"
			} else if sample.At.After(event.StartedAt.Add(5 * time.Minute)) {
				relation = "after_event"
			}
			return map[string]any{
				"code": code, "title": title, "confidence": "low", "supports": supports,
				"against": []string{}, "missing": []string{"Trace 未包含 PostgreSQL 后端 PID，未证实该请求等待于所见锁链"},
				"limitation": "慢调用与锁阻塞在同一资产和时间窗出现，只能提示排查方向，不能证明同一 SQL 或唯一根因。",
				"detail": map[string]any{"service": span.Service,
					"databaseSpanMs": span.End.Sub(span.Start).Milliseconds(), "lockPairs": len(pairs)},
				"state": "hypothesis", "matchedConditions": []any{}, "review": review,
				"timeOrder": map[string]any{"firstObservedAt": sample.At,
					"eventStartedAt": event.StartedAt, "relation": relation},
			}
		}
	}
	return nil
}
