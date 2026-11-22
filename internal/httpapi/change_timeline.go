package httpapi

import (
	"context"
	"errors"
	"net/http"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func addAssetChange(w http.ResponseWriter, r *http.Request, db HealthStore, assetID string) {
	changes, ok := db.(interface {
		AddChange(context.Context, string, storepkg.ChangeInput) (any, error)
	})
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "变更时间线暂不可用"})
		return
	}
	var input storepkg.ChangeInput
	if readJSONLimit(w, r, &input, 2048) != nil {
		respond(w, http.StatusBadRequest, map[string]string{"error": "变更格式无效"})
		return
	}
	change, err := changes.AddChange(r.Context(), assetID, input)
	switch {
	case err == nil:
		respond(w, http.StatusCreated, change)
	case errors.Is(err, storepkg.ErrInvalidChange):
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, storepkg.ErrAssetNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "资产不存在"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "变更记录暂不可用"})
	}
}
