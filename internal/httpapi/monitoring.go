package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type monitoringFleetReader interface {
	MonitoringFleet(context.Context, storepkg.FleetQuery, []string) (any, error)
}

func readMonitoringFleet(w http.ResponseWriter, r *http.Request, store HealthStore, ids []string, staleSeconds int) {
	reader, ok := store.(monitoringFleetReader)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Monitoring fleet unavailable"})
		return
	}
	params := r.URL.Query()
	q := storepkg.FleetQuery{Search: params.Get("q"), Runtime: params.Get("runtime"), Environment: params.Get("environment"),
		Tag: params.Get("tag"), Status: params.Get("status"), Page: 1, Limit: 12, StaleSeconds: staleSeconds}
	for key, target := range map[string]*int{"page": &q.Page, "limit": &q.Limit, "major": &q.Major} {
		if params.Has(key) {
			value, err := strconv.Atoi(params.Get(key))
			if err != nil {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid monitoring fleet query"})
				return
			}
			*target = value
		}
	}
	value, err := reader.MonitoringFleet(r.Context(), q, ids)
	switch {
	case errors.Is(err, storepkg.ErrInvalidFleetQuery):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid monitoring fleet query"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Monitoring fleet unavailable"})
	default:
		respond(w, http.StatusOK, value)
	}
}

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
