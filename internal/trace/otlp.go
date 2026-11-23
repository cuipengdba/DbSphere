package trace

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid OTLP trace payload")
var ErrTooMany = errors.New("too many OTLP spans or scopes")

var traceID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
var spanID = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
var k8sUID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
var endpointName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

type Asset struct {
	ID         string
	Host       string
	Port       int
	Database   string
	PoolerHost string
	PoolerPort int
}

type Span struct {
	AssetID         string
	TraceID         string
	SpanID          string
	ParentSpanID    string
	BackendPID      int
	QueryID         string
	Route           string
	AppPodNamespace string
	AppPodName      string
	AppPodUID       string
	Service         string
	Endpoint        string
	Kind            string
	Start           time.Time
	End             time.Time
	Status          string
	// SamplingProbability is an application declaration, not measured coverage.
	SamplingProbability *float64
	ReceivedAt          time.Time
}

// Pre-session stages are separate from SQL client spans. They must never
// carry a backend PID or be considered a database query.
func ConnectionStage(kind string) (phase string, timeout bool) {
	switch kind {
	case "pg_pool_acquire", "pg_pool_acquire_timeout":
		return "pool_acquire", kind == "pg_pool_acquire_timeout"
	case "pg_connect", "pg_connect_timeout":
		return "connect", kind == "pg_connect_timeout"
	}
	return "", false
}

func IsDatabaseTargetSpan(kind string) bool {
	phase, _ := ConnectionStage(kind)
	return kind == "postgresql_client" || phase != ""
}

type Result struct {
	Spans      []Span
	Candidates []Span
	Received   int
	Rejected   int
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
		} else if raw, ok := item.Value["doubleValue"]; ok {
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

func declaredSamplingProbability(value string) *float64 {
	if len(value) == 0 || len(value) > 32 {
		return nil
	}
	probability, err := strconv.ParseFloat(value, 64)
	if err != nil || probability <= 0 || probability > 1 || math.IsNaN(probability) || math.IsInf(probability, 0) {
		return nil
	}
	return &probability
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
		resourceAttrs := attributes(resource.Resource.Attributes)
		service := resourceAttrs["service.name"]
		probability := declaredSamplingProbability(resourceAttrs["dbsphere.trace.sampling_probability"])
		appNamespace, appPod, appUID := resourceAttrs["k8s.namespace.name"],
			resourceAttrs["k8s.pod.name"], resourceAttrs["k8s.pod.uid"]
		if !k8sName.MatchString(appNamespace) || len(appNamespace) > 63 ||
			!k8sName.MatchString(appPod) || len(appPod) > 253 || !k8sUID.MatchString(appUID) {
			appNamespace, appPod, appUID = "", "", ""
		}
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
					Service: service, Kind: "service", Start: start, End: end, Status: "ok",
					SamplingProbability: probability, ReceivedAt: now.UTC()}}
				if code := rawText(raw.Status.Code); code == "2" || code == "STATUS_CODE_ERROR" {
					item.Status = "error"
				}
				if isDB {
					item.Kind = "postgresql_client"
					switch attrs["dbsphere.connection.phase"] {
					case "", "execute":
					case "pool_acquire":
						item.Kind = "pg_pool_acquire"
					case "connect":
						item.Kind = "pg_connect"
					default:
						continue
					}
					if endpoint := attrs["dbsphere.endpoint"]; endpointName.MatchString(endpoint) {
						item.Endpoint = endpoint
					}
					if item.Kind != "postgresql_client" && item.Status == "error" && attrs["dbsphere.connection.outcome"] == "timeout" {
						item.Kind += "_timeout"
					}
					item.AppPodNamespace, item.AppPodName, item.AppPodUID = appNamespace, appPod, appUID
					if item.Kind == "postgresql_client" {
						if value, err := strconv.ParseInt(attrs["dbsphere.pg.backend_pid"], 10, 32); err == nil && value > 0 {
							item.BackendPID = int(value)
						}
						if value, err := strconv.ParseInt(attrs["dbsphere.pg.query_id"], 10, 64); err == nil && value != 0 {
							item.QueryID = strconv.FormatInt(value, 10)
						}
					}
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
		byID := map[string]parsedSpan{}
		ambiguous := map[string]bool{}
		for _, item := range group {
			if _, exists := byID[item.SpanID]; exists {
				ambiguous[item.SpanID] = true
			}
			byID[item.SpanID] = item
		}
		matched := map[string]bool{}
		dbAsset := map[string]string{}
		dbRoute := map[string]string{}
		for _, item := range group {
			if !IsDatabaseTargetSpan(item.Kind) || ambiguous[item.SpanID] ||
				item.address == "" || item.port < 1 || item.port > 65535 {
				continue
			}
			if phase, _ := ConnectionStage(item.Kind); phase != "" && item.namespace == "" {
				continue
			}
			match, route := "", ""
			count := 0
			for _, asset := range assets {
				direct := strings.EqualFold(asset.Host, item.address) && asset.Port == item.port &&
					(item.namespace == "" || asset.Database == item.namespace)
				viaPooler := item.namespace != "" && asset.Database == item.namespace &&
					asset.PoolerHost != "" && strings.EqualFold(asset.PoolerHost, item.address) &&
					asset.PoolerPort == item.port
				if direct || viaPooler {
					match = asset.ID
					route = "direct"
					if !direct {
						route = "pgbouncer"
					}
					count++
				}
			}
			if count == 1 {
				matched[match] = true
				dbAsset[item.SpanID] = match
				dbRoute[item.SpanID] = route
			}
		}
		for assetID := range matched {
			include := map[string]bool{}
			for spanID, mappedAsset := range dbAsset {
				if mappedAsset != assetID {
					continue
				}
				visited := map[string]bool{}
				for current := spanID; current != "" && !visited[current]; {
					visited[current] = true
					item, exists := byID[current]
					if !exists || ambiguous[current] ||
						(IsDatabaseTargetSpan(item.Kind) && dbAsset[current] != assetID) {
						break
					}
					include[current] = true
					current = item.ParentSpanID
				}
			}
			for _, item := range group {
				if !include[item.SpanID] {
					continue
				}
				item.AssetID = assetID
				if IsDatabaseTargetSpan(item.Kind) {
					item.Route = dbRoute[item.SpanID]
				}
				result.Spans = append(result.Spans, item.Span)
				if len(result.Spans) > 4096 {
					return Result{}, ErrTooMany
				}
				accepted[item.TraceID+":"+item.SpanID] = true
			}
		}
		// Service spans may be exported after their database child in a separate
		// OTLP request. Keep only sanitized candidates for a store-side lookup;
		// they must never be persisted without a previously mapped child.
		for _, item := range group {
			if item.Kind != "service" || ambiguous[item.SpanID] || accepted[item.TraceID+":"+item.SpanID] {
				continue
			}
			result.Candidates = append(result.Candidates, item.Span)
		}
	}
	result.Rejected = result.Received - len(accepted)
	return result, nil
}
