package store

import "testing"

func TestTraceTopologyOnlyLinksObservedCrossServiceParents(t *testing.T) {
	parents := []map[string]string{
		{"from": "span:t:1", "to": "span:t:2", "relation": "parent_of"},
		{"from": "span:t:2", "to": "span:t:3", "relation": "parent_of"},
		{"from": "span:t:1", "to": "span:t:4", "relation": "parent_of"},
		{"from": "span:t:missing", "to": "span:t:5", "relation": "parent_of"},
	}
	services := map[string]string{
		"span:t:1": "gateway", "span:t:2": "checkout", "span:t:3": "checkout",
		"span:t:4": "checkout", "span:t:5": "worker",
	}
	edges := []map[string]string{}
	if missing := addTraceParentAndServiceEdges(parents, services, &edges); missing != 1 {
		t.Fatalf("missing parent count = %d", missing)
	}
	dependencies, parentLinks := 0, 0
	for _, edge := range edges {
		if edge["relation"] == "calls_service" {
			dependencies++
			if edge["from"] != "service:gateway" || edge["to"] != "service:checkout" {
				t.Fatalf("false service dependency: %v", edge)
			}
		}
		if edge["relation"] == "parent_of" {
			parentLinks++
		}
	}
	if dependencies != 1 || parentLinks != 3 {
		t.Fatalf("topology edges: dependencies=%d parentLinks=%d", dependencies, parentLinks)
	}
}
