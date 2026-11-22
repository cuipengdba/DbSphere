package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var routingPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/routing$`)

type routingStore interface {
	ReadRouting(context.Context, string) (storepkg.RoutingSettings, error)
	SetRouting(context.Context, string, storepkg.RoutingInput, []byte) (storepkg.RoutingSettings, error)
}

func handleRouting(w http.ResponseWriter, r *http.Request, db HealthStore, id string, key []byte) {
	s, ok := db.(routingStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "入口诊断暂不可用"})
		return
	}
	var settings storepkg.RoutingSettings
	var err error
	if r.Method == http.MethodPatch {
		var input storepkg.RoutingInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "入口配置无效"})
			return
		}
		settings, err = s.SetRouting(r.Context(), id, input, key)
	} else {
		settings, err = s.ReadRouting(r.Context(), id)
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, settings)
	case errors.Is(err, storepkg.ErrAssetNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "资产不存在"})
	case errors.Is(err, storepkg.ErrRoutingSettings):
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "入口诊断暂不可用"})
	}
}
