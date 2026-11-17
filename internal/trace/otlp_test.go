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
