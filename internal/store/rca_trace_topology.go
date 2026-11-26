package store

func requestParentEdges(spans []requestSpanObservation) ([]map[string]string, map[string]bool, int) {
	indexed := indexRequestSpans(spans)
	edges := make([]map[string]string, 0, len(spans))
	matched := make(map[string]bool, len(spans))
	conflicted := 0
	for _, span := range spans {
		if span.ParentSpanID == "" {
			continue
		}
		node := "span:" + span.TraceID + ":" + span.SpanID
		parent, found := indexed[[2]string{span.TraceID, span.ParentSpanID}]
		if found {
			if !traceParentTimeCompatible(parent, span) || parentCycle(span, indexed) {
				conflicted++
				continue
			}
			matched[node] = true
		}
		edges = append(edges, map[string]string{"from": "span:" + span.TraceID + ":" + span.ParentSpanID,
			"to": node, "relation": "parent_of"})
	}
	return edges, matched, conflicted
}

func parentCycle(span requestSpanObservation, indexed map[[2]string]requestSpanObservation) bool {
	current := span
	for i := 0; i < len(indexed) && current.ParentSpanID != ""; i++ {
		if current.ParentSpanID == span.SpanID {
			return true
		}
		parent, found := indexed[[2]string{span.TraceID, current.ParentSpanID}]
		if !found || !traceParentTimeCompatible(parent, current) {
			return false
		}
		current = parent
	}
	return false
}

// Resolve only parent links visible in this event's bounded span set. A
// cross-service parent link is an observed dependency, not a causal claim.
func addTraceParentAndServiceEdges(parentEdges []map[string]string, spanServices map[string]string,
	edges *[]map[string]string) int {
	unresolved := 0
	dependencies := map[string]bool{}
	for _, edge := range parentEdges {
		parentService, parentOK := spanServices[edge["from"]]
		childService, childOK := spanServices[edge["to"]]
		if !parentOK || !childOK {
			unresolved++
			continue
		}
		*edges = append(*edges, edge)
		if parentService == childService {
			continue
		}
		key := parentService + "\x00" + childService
		if dependencies[key] {
			continue
		}
		dependencies[key] = true
		*edges = append(*edges, map[string]string{
			"from": "service:" + parentService, "to": "service:" + childService,
			"relation": "calls_service"})
	}
	return unresolved
}
