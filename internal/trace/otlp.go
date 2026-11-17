package trace

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid OTLP trace payload")
var ErrTooMany = errors.New("too many OTLP spans or scopes")

var traceID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
var spanID = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)

type Asset struct {
	ID       string
	Host     string
	Port     int
	Database string
}

type Span struct {
	AssetID      string
	TraceID      string
	SpanID       string
	ParentSpanID string
	Service      string
	Kind         string
	Start        time.Time
	End          time.Time
	Status       string
}

type Result struct {
	Spans    []Span
	Received int
	Rejected int
}

type attribute struct {
	Key   string                     `json:"key"`
	Value map[string]json.RawMessage `json:"value"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Kind              json.RawMessage `json:"kind"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Attributes        []attribute     `json:"attributes"`
	Status            struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

type otlpResource struct {
	Resource struct {
		Attributes []attribute `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []struct {
		Spans []otlpSpan `json:"spans"`
	} `json:"scopeSpans"`
}

func rawText(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return string(value)
}

func attributes(values []attribute) map[string]string {
	result := map[string]string{}
	for _, item := range values {
		if item.Key == "" {
			continue
		}
		if raw, ok := item.Value["stringValue"]; ok {
			result[item.Key] = rawText(raw)
		} else if raw, ok := item.Value["intValue"]; ok {
			result[item.Key] = rawText(raw)
		}
	}
	return result
}

func nanos(value json.RawMessage) (time.Time, bool) {
	number, err := strconv.ParseInt(rawText(value), 10, 64)
	if err != nil || number <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, number).UTC(), true
}

func Decode(body []byte, assets []Asset, now time.Time) (Result, error) {
	var envelope struct {
		ResourceSpans []otlpResource `json:"resourceSpans"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.ResourceSpans == nil ||
		len(envelope.ResourceSpans) > 32 {
		return Result{}, ErrInvalid
	}
	type parsedSpan struct {
		Span
		address   string
		port      int
		namespace string
	}
	groups := map[string][]parsedSpan{}
	result := Result{Spans: []Span{}}
	for _, resource := range envelope.ResourceSpans {
		service := attributes(resource.Resource.Attributes)["service.name"]
		if len(resource.ScopeSpans) > 32 {
			return Result{}, ErrTooMany
		}
		for _, scope := range resource.ScopeSpans {
			if len(scope.Spans) > 256 {
				return Result{}, ErrTooMany
			}
			for _, raw := range scope.Spans {
				result.Received++
				if result.Received > 256 {
					return Result{}, ErrTooMany
				}
				start, startOK := nanos(raw.StartTimeUnixNano)
				end, endOK := nanos(raw.EndTimeUnixNano)
				if service == "" || len(service) > 128 || !traceID.MatchString(raw.TraceID) ||
					!spanID.MatchString(raw.SpanID) ||
					(raw.ParentSpanID != "" && !spanID.MatchString(raw.ParentSpanID)) ||
					!startOK || !endOK || end.Before(start) ||
					start.Before(now.Add(-7*24*time.Hour)) || start.After(now.Add(7*24*time.Hour)) {
					continue
				}
				attrs := attributes(raw.Attributes)
				isDB := (rawText(raw.Kind) == "3" || rawText(raw.Kind) == "SPAN_KIND_CLIENT") &&
					attrs["db.system.name"] == "postgresql"
				item := parsedSpan{Span: Span{TraceID: strings.ToLower(raw.TraceID),
					SpanID: strings.ToLower(raw.SpanID), ParentSpanID: strings.ToLower(raw.ParentSpanID),
					Service: service, Kind: "service", Start: start, End: end, Status: "ok"}}
				if code := rawText(raw.Status.Code); code == "2" || code == "STATUS_CODE_ERROR" {
					item.Status = "error"
				}
				if isDB {
					item.Kind = "postgresql_client"
					item.address = attrs["server.address"]
					item.port = 5432
					if value := attrs["server.port"]; value != "" {
						item.port, _ = strconv.Atoi(value)
					}
					item.namespace = strings.Split(attrs["db.namespace"], "|")[0]
				}
				groups[item.TraceID] = append(groups[item.TraceID], item)
			}
		}
	}
	accepted := map[string]bool{}
	for _, group := range groups {
		matched := map[string]bool{}
		dbAsset := map[string]string{}
		for _, item := range group {
			if item.Kind != "postgresql_client" || item.address == "" || item.port < 1 || item.port > 65535 {
				continue
			}
			match := ""
			count := 0
			for _, asset := range assets {
				if strings.EqualFold(asset.Host, item.address) && asset.Port == item.port &&
					(item.namespace == "" || asset.Database == item.namespace) {
					match = asset.ID
					count++
				}
			}
			if count == 1 {
				matched[match] = true
				dbAsset[item.SpanID] = match
			}
		}
		for assetID := range matched {
			for _, item := range group {
				if item.Kind == "postgresql_client" && dbAsset[item.SpanID] != assetID {
					continue
				}
				item.AssetID = assetID
				result.Spans = append(result.Spans, item.Span)
				if len(result.Spans) > 4096 {
					return Result{}, ErrTooMany
				}
				accepted[item.TraceID+":"+item.SpanID] = true
			}
		}
	}
	result.Rejected = result.Received - len(accepted)
	return result, nil
}
