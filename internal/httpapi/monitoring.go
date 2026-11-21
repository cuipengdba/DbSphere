package httpapi

import (
	"errors"
	"net/http"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func readMonitoringSeries(w http.ResponseWriter, r *http.Request, store HealthStore, scope map[string]bool) {
	params := r.URL.Query()
	ids := params["asset"]
	if len(ids) < 1 || len(ids) > 5 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "Select one to five assets"})
		return
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !assetPath.MatchString("/api/assets/"+id) || seen[id] {
			respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid monitoring assets"})
			return
		}
		seen[id] = true
		if scope != nil && !scope[id] {
			respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
			return
		}
	}
	reader, ok := store.(MonitoringSeriesReader)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Monitoring series unavailable"})
		return
	}
	value, err := reader.MonitoringSeries(r.Context(), ids, params)
	switch {
	case errors.Is(err, storepkg.ErrInvalidRange):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid monitoring time range"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Monitoring series unavailable"})
	default:
		respond(w, http.StatusOK, value)
	}
}
