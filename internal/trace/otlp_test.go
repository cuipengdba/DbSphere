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

func TestDecodeMapsOnlyUniquePgBouncerEndpointWithDatabaseIdentity(t *testing.T) {
	now := time.Now().UTC()
	attr := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	build := func(address, database string) []byte {
		port := "6432"
		if address == "pg.internal" {
			port = "5432"
		}
		attributes := []any{attr("db.system.name", "postgresql"),
			attr("server.address", address), attr("server.port", port)}
		if database != "" {
			attributes = append(attributes, attr("db.namespace", database))
		}
		body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attr("service.name", "checkout")}},
			"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
				"traceId": "abababababababababababababababab", "spanId": "1111111111111111",
				"kind": 3, "startTimeUnixNano": now.Add(-time.Second).UnixNano(),
				"endTimeUnixNano": now.UnixNano(), "attributes": attributes,
			}}}}}}})
		return body
	}
	asset := Asset{ID: "orders", Host: "pg.internal", Port: 5432, Database: "orders",
		PoolerHost: "pool.internal", PoolerPort: 6432}
	for _, tc := range []struct {
		name, address, database string
		assets                  []Asset
		wantRoute               string
	}{
		{"unique pooler", "pool.internal", "orders", []Asset{asset}, "pgbouncer"},
		{"missing database", "pool.internal", "", []Asset{asset}, ""},
		{"wrong database", "pool.internal", "other", []Asset{asset}, ""},
		{"ambiguous pooler", "pool.internal", "orders", []Asset{asset,
			{ID: "other", Host: "pg-other.internal", Port: 5432, Database: "orders",
				PoolerHost: "pool.internal", PoolerPort: 6432}}, ""},
		{"direct endpoint", "pg.internal", "orders", []Asset{asset}, "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Decode(build(tc.address, tc.database), tc.assets, now)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRoute == "" {
				if len(result.Spans) != 0 || result.Rejected != 1 {
					t.Fatalf("ambiguous or unqualified pooler span accepted: %+v", result)
				}
				return
			}
			if len(result.Spans) != 1 || result.Spans[0].AssetID != asset.ID ||
				result.Spans[0].Route != tc.wantRoute {
				t.Fatalf("incorrect PostgreSQL route: %+v", result)
			}
		})
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

func TestDecodeKeepsOnlyValidatedBackendPIDOnDatabaseSpan(t *testing.T) {
	now := time.Now().UTC()
	attribute := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	build := func(pid string) []byte {
		span := map[string]any{
			"traceId": "dddddddddddddddddddddddddddddddd", "spanId": "1111111111111111",
			"kind": 3, "startTimeUnixNano": now.Add(-time.Second).UnixNano(),
			"endTimeUnixNano": now.UnixNano(), "attributes": []any{
				attribute("db.system.name", "postgresql"), attribute("server.address", "pg-a"),
				attribute("server.port", "5432"), attribute("db.namespace", "orders"),
				attribute("dbsphere.pg.backend_pid", pid), attribute("db.statement", "SELECT secret")}}
		resource := map[string]any{"resource": map[string]any{"attributes": []any{attribute("service.name", "checkout")}},
			"scopeSpans": []any{map[string]any{"spans": []any{span}}}}
		body, _ := json.Marshal(map[string]any{"resourceSpans": []any{resource}})
		return body
	}
	assets := []Asset{{ID: "asset-a", Host: "pg-a", Port: 5432, Database: "orders"}}
	for _, test := range []struct {
		raw      string
		expected int
	}{{"101", 101}, {"0", 0}, {"-1", 0}, {"2147483648", 0}, {"101.5", 0}} {
		result, err := Decode(build(test.raw), assets, now)
		if err != nil || len(result.Spans) != 1 || result.Spans[0].BackendPID != test.expected {
			t.Fatalf("backend PID %q was not validated: %+v %v", test.raw, result, err)
		}
		encoded, _ := json.Marshal(result.Spans[0])
		if strings.Contains(string(encoded), "secret") {
			t.Fatal("SQL text entered decoded span")
		}
	}
}

func TestDecodeKeepsOnlyValidatedPostgresQueryID(t *testing.T) {
	now := time.Now().UTC()
	attribute := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	for _, tc := range []struct{ raw, want string }{
		{"-9223372036854775808", "-9223372036854775808"},
		{"+42", "42"}, {"0", ""}, {"9223372036854775808", ""},
		{"42;SELECT secret", ""},
	} {
		span := map[string]any{"traceId": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			"spanId": "1111111111111111", "kind": 3,
			"startTimeUnixNano": now.Add(-time.Second).UnixNano(), "endTimeUnixNano": now.UnixNano(),
			"attributes": []any{attribute("db.system.name", "postgresql"),
				attribute("server.address", "pg-a"), attribute("db.namespace", "orders"),
				attribute("dbsphere.pg.query_id", tc.raw), attribute("db.statement", "SELECT secret")}}
		body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource":   map[string]any{"attributes": []any{attribute("service.name", "checkout")}},
			"scopeSpans": []any{map[string]any{"spans": []any{span}}}}}})
		result, err := Decode(body, []Asset{{ID: "asset-a", Host: "pg-a", Port: 5432, Database: "orders"}}, now)
		if err != nil || len(result.Spans) != 1 || result.Spans[0].QueryID != tc.want {
			t.Fatalf("Query ID %q was not normalized: %+v %v", tc.raw, result, err)
		}
		encoded, _ := json.Marshal(result.Spans[0])
		if strings.Contains(string(encoded), "secret") {
			t.Fatal("SQL text entered decoded span")
		}
	}
}
