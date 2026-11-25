package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/internal/store"
)

type liveObservationReader interface {
	LiveObservation(context.Context, []string, int, store.LiveScope) (json.RawMessage, error)
}

const liveOverviewPath, liveStreamPath = "/api/live/overview", "/api/live/stream"

func readLiveObservation(w http.ResponseWriter, r *http.Request, db HealthStore, auth *AuthConfig, ids []string, connections chan struct{}, platformAdmin bool) {
	reader, ok := db.(liveObservationReader)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Live observation unavailable"})
		return
	}
	params := r.URL.Query()
	scope := store.LiveScope{Organization: params.Get("organization"), Environment: params.Get("environment"),
		Cluster: params.Get("cluster"), Engine: params.Get("engine")}
	if params.Get("board") == "wallboard" {
		scope.Wallboard, scope.UnassignedHosts, scope.Focus = true, platformAdmin, params.Get("asset")
		scope.BoardState = params.Get("state")
		switch scope.BoardState {
		case "", "affected", "missing", "maintenance", "recovered", "healthy":
		default:
			respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid wallboard state"})
			return
		}
		if params.Has("page") {
			page, err := strconv.Atoi(params.Get("page"))
			if err != nil || page < 0 || page > 10000 {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid wallboard page"})
				return
			}
			scope.Page = page
		}
	}
	if (scope.Organization != "" && !opsUUID.MatchString(scope.Organization)) || len(scope.Environment) > 80 || len(scope.Cluster) > 256 ||
		(scope.Engine != "" && scope.Engine != "postgres" && scope.Engine != "mysql" && scope.Engine != "dm8" && scope.Engine != "kingbase" && scope.Engine != "redis") ||
		(scope.Focus != "" && !opsUUID.MatchString(scope.Focus)) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid live observation scope"})
		return
	}
	stream := r.URL.Path == liveStreamPath
	if stream {
		select {
		case connections <- struct{}{}:
			defer func() { <-connections }()
		default:
			w.Header().Set("Retry-After", "10")
			respond(w, http.StatusTooManyRequests, map[string]string{"error": "Live stream capacity reached"})
			return
		}
	}
	staleAfter := 0
	if auth.CollectorState != nil {
		state := auth.CollectorState.Snapshot(time.Now())
		if state.Enabled {
			staleAfter = max(10, state.IntervalSeconds*2)
		}
	}
	observed, err := reader.LiveObservation(r.Context(), ids, staleAfter, scope)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Live observation unavailable"})
		return
	}
	if !stream {
		respond(w, http.StatusOK, observed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(value json.RawMessage) error {
		if _, err := fmt.Fprintf(w, "event: observation\ndata: %s\n\n", value); err != nil {
			return err
		}
		return controller.Flush()
	}
	if _, err := fmt.Fprint(w, "retry: 1250\n\n"); err != nil || send(observed) != nil {
		return
	}
	// Renew authorization and scope periodically, including after password changes.
	timer := time.NewTimer(22 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(2500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			_, _ = fmt.Fprint(w, "event: reconnect\ndata: {}\n\n")
			_ = controller.Flush()
			return
		case <-ticker.C:
			value, err := reader.LiveObservation(r.Context(), ids, staleAfter, scope)
			if err != nil || send(value) != nil {
				return
			}
		}
	}
}
