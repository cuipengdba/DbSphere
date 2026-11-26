package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const historyPolicyPath = "/api/admin/history-retention"

var assetHistoryTierPath = regexp.MustCompile("^/api/assets/(" + accessUUID + ")/history-tier$")

type historyTierStore interface {
	HistoryTierPolicies(context.Context) ([]storepkg.HistoryTierPolicy, error)
	SetHistoryTierPolicy(context.Context, storepkg.HistoryTierPolicy) (storepkg.HistoryTierPolicy, error)
	ReadHistoryTier(context.Context, string, string, url.Values) (any, error)
}

func handleHistoryTier(w http.ResponseWriter, r *http.Request, store HealthStore, role string) bool {
	isPolicy := r.URL.Path == historyPolicyPath
	series := assetHistoryTierPath.FindStringSubmatch(r.URL.Path)
	if !isPolicy && series == nil {
		return false
	}
	if isPolicy && role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
		return true
	}
	db, ok := store.(historyTierStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "History tier unavailable"})
		return true
	}
	var value any
	var err error
	switch {
	case series != nil && r.Method == http.MethodGet:
		value, err = db.ReadHistoryTier(r.Context(), series[1], r.URL.Query().Get("metric"), r.URL.Query())
	case isPolicy && r.Method == http.MethodGet:
		policies, readErr := db.HistoryTierPolicies(r.Context())
		value, err = map[string]any{"policies": policies}, readErr
	case isPolicy && r.Method == http.MethodPut:
		var input storepkg.HistoryTierPolicy
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrHistoryTierInput
			break
		}
		policy, writeErr := db.SetHistoryTierPolicy(r.Context(), input)
		value, err = map[string]any{"policy": policy}, writeErr
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return true
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, value)
	case errors.Is(err, storepkg.ErrHistoryTierInput):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid history metric or retention"})
	case errors.Is(err, storepkg.ErrAssetNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "History storage unavailable"})
	}
	return true
}
