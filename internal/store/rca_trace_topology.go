package store

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
