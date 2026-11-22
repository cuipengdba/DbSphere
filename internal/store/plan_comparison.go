package store

import (
	"math"
	"sort"
	"strings"
)

func compareSQLPlans(current, prior map[string]any) map[string]any {
	after, before := object(current["summary"]), object(prior["summary"])
	comparison := map[string]any{"priorId": prior["id"], "shapeChanged": prior["shapeHash"] != current["shapeHash"],
		"executionRatio": nil, "database": current["database"]}
	if afterMs, beforeMs := number(after["executionMs"]), number(before["executionMs"]); afterMs > 0 && beforeMs > 0 {
		if ratio := afterMs / beforeMs; !math.IsInf(ratio, 0) {
			comparison["executionRatio"] = ratio
		}
	}
	afterNodes, beforeNodes := list(after["nodes"]), list(before["nodes"])
	comparison["operations"] = comparePlanOperations(afterNodes, beforeNodes)
	comparison["measurements"] = []map[string]any{
		{"name": "executionMs", "before": before["executionMs"], "after": after["executionMs"]},
		{"name": "estimateFactor", "before": planEstimateFactor(beforeNodes), "after": planEstimateFactor(afterNodes)},
		{"name": "tempReadBlocks", "before": planRootMeasure(beforeNodes, "tempReadBlocks"), "after": planRootMeasure(afterNodes, "tempReadBlocks")},
		{"name": "tempWrittenBlocks", "before": planRootMeasure(beforeNodes, "tempWrittenBlocks"), "after": planRootMeasure(afterNodes, "tempWrittenBlocks")},
	}
	return comparison
}

func comparePlanOperations(after, before []any) []map[string]any {
	oldCounts, newCounts := planOperationCounts(before), planOperationCounts(after)
	keys := make([]string, 0, len(oldCounts)+len(newCounts))
	for name := range oldCounts {
		keys = append(keys, name)
	}
	for name := range newCounts {
		if oldCounts[name] == 0 {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	result := make([]map[string]any, 0, len(keys))
	for _, name := range keys {
		result = append(result, map[string]any{"name": name, "before": oldCounts[name], "after": newCounts[name]})
	}
	return result
}

func planOperationCounts(nodes []any) map[string]int {
	counts := map[string]int{}
	for _, raw := range nodes {
		node := object(raw)
		kind := stringValue(node["type"])
		if strings.HasSuffix(kind, "Scan") || strings.HasSuffix(kind, "Join") || kind == "Nested Loop" {
			if join := stringValue(node["joinType"]); join != "" {
				kind += " · " + join
			}
			counts[kind]++
		}
	}
	return counts
}

func planEstimateFactor(nodes []any) any {
	worst := 0.0
	for _, raw := range nodes {
		node := object(raw)
		planned, actual := number(node["planRows"]), number(node["actualRows"])
		if planned > 0 && actual > 0 {
			factor := max(planned/actual, actual/planned)
			if !math.IsInf(factor, 0) {
				worst = max(worst, factor)
			}
		}
	}
	if worst == 0 {
		return nil
	}
	return worst
}

func planRootMeasure(nodes []any, key string) any {
	if len(nodes) == 0 {
		return nil
	}
	return object(nodes[0])[key]
}
