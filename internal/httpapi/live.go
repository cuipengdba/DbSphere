package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type liveObservationReader interface {
	LiveObservation(context.Context, []string, int) (json.RawMessage, error)
}

const liveOverviewPath, liveStreamPath = "/api/live/overview", "/api/live/stream"

func readLiveObservation(w http.ResponseWriter, r *http.Request, db HealthStore, auth *AuthConfig, ids []string, connections chan struct{}) {
	reader, ok := db.(liveObservationReader)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Live observation unavailable"})
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
	observed, err := reader.LiveObservation(r.Context(), ids, staleAfter)
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
			value, err := reader.LiveObservation(r.Context(), ids, staleAfter)
			if err != nil || send(value) != nil {
				return
			}
		}
	}
}
