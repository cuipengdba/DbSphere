package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

var scopedAssetPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:/.*)?$`)

type scopedResourceLookup interface {
	AssetIDForResource(context.Context, string, string) (string, error)
}

// Scoped viewers can only read resources whose asset ownership is known.
// Unknown and aggregate endpoints fail closed rather than exposing cross-asset data.
func authorizeScopedViewerRead(w http.ResponseWriter, r *http.Request, db HealthStore, allowed map[string]bool) bool {
	if r.Method != http.MethodGet {
		respond(w, http.StatusForbidden, map[string]string{"error": "Read-only account"})
		return false
	}
	path := r.URL.Path
	if match := scopedAssetPath.FindStringSubmatch(path); match != nil {
		if !allowed[match[1]] {
			respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
			return false
		}
		return true
	}
	for _, item := range []struct {
		kind  string
		match []string
	}{
		{"event", eventPath.FindStringSubmatch(path)},
		{"event", eventReportPath.FindStringSubmatch(path)},
		{"event", eventReviewPath.FindStringSubmatch(path)},
		{"event", eventImpactPath.FindStringSubmatch(path)},
		{"event", eventDeliveriesPath.FindStringSubmatch(path)},
		{"inspection", inspectionReportPath.FindStringSubmatch(path)},
	} {
		if item.match == nil {
			continue
		}
		return authorizeScopedResource(w, r, db, allowed, item.kind, item.match[1])
	}
	if path == "/api/ai/status" || path == "/api/alert-rules" || path == "/api/inspection-templates" ||
		path == "/api/monitoring/series" || path == monitoringOverviewPath || path == liveOverviewPath || path == liveStreamPath || path == liveIntegrationsPath ||
		path == "/api/events/page" || path == "/api/events/overview" || path == "/api/assets/page" {
		return true
	}
	if scopedCollectionKeys[path] != "" {
		return true
	}
	respond(w, http.StatusForbidden, map[string]string{"error": "Endpoint unavailable to asset-scoped viewer"})
	return false
}

func authorizeScopedResource(w http.ResponseWriter, r *http.Request, db HealthStore, allowed map[string]bool, kind, id string) bool {
	lookup, ok := db.(scopedResourceLookup)
	if !ok {
		respond(w, http.StatusForbidden, map[string]string{"error": "Asset scope unavailable"})
		return false
	}
	assetID, err := lookup.AssetIDForResource(r.Context(), kind, id)
	if err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset scope unavailable"})
		return false
	}
	if assetID == "" || !allowed[assetID] {
		respond(w, http.StatusNotFound, map[string]string{"error": "Resource not found"})
		return false
	}
	return true
}

var scopedCollectionKeys = map[string]string{
	"/api/assets":               "assets",
	"/api/monitoring/instances": "instances",
	"/api/events":               "events",
	"/api/alerts":               "alerts",
	"/api/alert-workbench":      "incidents",
	"/api/silences":             "silences",
	"/api/notifications":        "notifications",
	"/api/inspection-schedules": "schedules",
	"/api/inspection-runs":      "runs",
}

func filterScopedCollection(path string, value any, allowed map[string]bool) (any, error) {
	key := scopedCollectionKeys[path]
	if key == "" {
		return nil, errors.New("collection has no asset scope")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var collection map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &collection); err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(collection[key], &rows); err != nil || rows == nil {
		return nil, errors.New("collection rows missing")
	}
	filtered := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(row, &item); err != nil {
			return nil, err
		}
		idKey := "assetId"
		if path == "/api/assets" {
			idKey = "id"
		}
		var id string
		if err := json.Unmarshal(item[idKey], &id); err != nil {
			return nil, errors.New("collection row has no asset identity")
		}
		if allowed[strings.ToLower(id)] {
			filtered = append(filtered, row)
		}
	}
	return map[string]any{key: filtered}, nil
}
