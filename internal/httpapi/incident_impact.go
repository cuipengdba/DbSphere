package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var eventImpactPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/impact$`)

func handleIncidentImpact(w http.ResponseWriter, r *http.Request, db HealthStore, id string, scope []string) {
	store, ok := db.(interface {
		IncidentImpact(context.Context, string, []string) (any, error)
		UpdateIncidentLink(context.Context, string, storepkg.IncidentLinkInput) error
	})
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "跨实例影响暂不可用"})
		return
	}
	var impact any
	var err error
	if r.Method == http.MethodPost {
		var input storepkg.IncidentLinkInput
		if readJSONLimit(w, r, &input, 2048) != nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "事件关联格式无效"})
			return
		}
		err = store.UpdateIncidentLink(r.Context(), id, input)
		impact = map[string]bool{"ok": true}
	} else {
		impact, err = store.IncidentImpact(r.Context(), id, scope)
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, impact)
	case errors.Is(err, storepkg.ErrEventNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "事件不存在"})
	case errors.Is(err, storepkg.ErrInvalidIncidentLink):
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, storepkg.ErrIncidentLinkConflict):
		respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "跨实例影响暂不可用"})
	}
}
