package trace

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecodeMapsDatabaseSpansWithoutCrossAssetLeak(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-time.Second).UnixNano()
	end := now.UnixNano()
	attr := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	trace := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	parent := "1111111111111111"
	makeDB := func(id, address string) map[string]any {
		return map[string]any{"traceId": trace, "spanId": id, "parentSpanId": parent,
			"kind": 3, "startTimeUnixNano": start, "endTimeUnixNano": end,
			"attributes": []any{attr("db.system.name", "postgresql"),
				attr("server.address", address), attr("server.port", "5432"),
				attr("db.namespace", "orders"), attr("db.statement", "SELECT secret")}}
	}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attr("service.name", "checkout")}},
		"scopeSpans": []any{map[string]any{"spans": []any{
			map[string]any{"traceId": trace, "spanId": parent, "startTimeUnixNano": start,
				"endTimeUnixNano": end, "name": "GET /secret", "attributes": []any{attr("http.url", "https://secret")}},
			makeDB("2222222222222222", "pg-a"), makeDB("3333333333333333", "pg-b")}}}}}})
	result, err := Decode(body, []Asset{{ID: "asset-a", Host: "pg-a", Port: 5432, Database: "orders"},
		{ID: "asset-b", Host: "pg-b", Port: 5432, Database: "orders"}}, now)
	if err != nil || result.Received != 3 || result.Rejected != 0 || len(result.Spans) != 4 {
		t.Fatalf("OTLP mapping failed: %+v %v", result, err)
	}
	for _, span := range result.Spans {
		if span.Kind == "postgresql_client" &&
			((span.SpanID == "2222222222222222" && span.AssetID != "asset-a") ||
				(span.SpanID == "3333333333333333" && span.AssetID != "asset-b")) {
			t.Fatalf("DB span crossed asset boundary: %+v", span)
		}
		encoded, _ := json.Marshal(span)
		if string(encoded) == "" || strings.Contains(string(encoded), "secret") {
			t.Fatalf("unallowlisted attribute persisted: %s", encoded)
		}
	}
}

func TestDecodeRejectsInvalidAndExcessiveOTLP(t *testing.T) {
	if _, err := Decode([]byte(`{"other":[]}`), nil, time.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing resourceSpans accepted")
	}
	scopes := make([]any, 33)
	for index := range scopes {
		scopes[index] = map[string]any{"spans": []any{}}
	}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{"scopeSpans": scopes}}})
	if _, err := Decode(body, nil, time.Now()); !errors.Is(err, ErrTooMany) {
		t.Fatal("unbounded scopes accepted")
	}
}

func TestDecodeOnlyKeepsDatabaseAncestorChain(t *testing.T) {
	now := time.Now().UTC()
	attr := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	traceID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	makeSpan := func(id, parent string, db bool) map[string]any {
		span := map[string]any{"traceId": traceID, "spanId": id, "parentSpanId": parent,
			"startTimeUnixNano": now.Add(-time.Second).UnixNano(), "endTimeUnixNano": now.UnixNano()}
		if db {
			span["kind"] = 3
			span["attributes"] = []any{attr("db.system.name", "postgresql"),
				attr("server.address", "pg-a"), attr("server.port", "5432"), attr("db.namespace", "orders")}
		}
		return span
	}
	root := "1111111111111111"
	parent := "2222222222222222"
	db := "3333333333333333"
	sibling := "4444444444444444"
	unrelated := "5555555555555555"
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attr("service.name", "checkout")}},
		"scopeSpans": []any{map[string]any{"spans": []any{makeSpan(root, "", false),
			makeSpan(parent, root, false), makeSpan(db, parent, true),
			makeSpan(sibling, root, false), makeSpan(unrelated, "", false)}}}}}})
	result, err := Decode(body, []Asset{{ID: "asset-a", Host: "pg-a", Port: 5432, Database: "orders"}}, now)
	if err != nil || result.Received != 5 || result.Rejected != 2 || len(result.Spans) != 3 {
		t.Fatalf("unrelated branch was retained: %+v %v", result, err)
	}
	for _, span := range result.Spans {
		if span.SpanID == sibling || span.SpanID == unrelated || span.AssetID != "asset-a" {
			t.Fatalf("trace crossed the database parent chain: %+v", span)
		}
	}
}

func TestDecodeRejectsAmbiguousSpanIdentity(t *testing.T) {
	now := time.Now().UTC()
	attribute := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	span := map[string]any{"traceId": "cccccccccccccccccccccccccccccccc",
		"spanId": "1111111111111111", "kind": 3,
		"startTimeUnixNano": now.Add(-time.Second).UnixNano(), "endTimeUnixNano": now.UnixNano(),
		"attributes": []any{attribute("db.system.name", "postgresql"),
			attribute("server.address", "pg-a"), attribute("server.port", "5432"),
			attribute("db.namespace", "orders")}}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{attribute("service.name", "checkout")}},
		"scopeSpans": []any{map[string]any{"spans": []any{span, span}}}}}})
	result, err := Decode(body, []Asset{{ID: "asset-a", Host: "pg-a", Port: 5432, Database: "orders"}}, now)
	if err != nil || result.Received != 2 || result.Rejected != 2 || len(result.Spans) != 0 {
		t.Fatalf("ambiguous database span entered an asset: %+v %v", result, err)
	}
}
