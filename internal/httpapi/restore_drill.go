package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var restoreDrillPath = regexp.MustCompile(`^/api/assets/(` + accessUUID + `)/restore-drills(?:/(` + accessUUID + `)/(approve|reject|start|complete|fail))?$`)

type restoreDrillStore interface {
	RestoreDrills(context.Context, string) (any, error)
	RequestRestoreDrill(context.Context, string, storepkg.RestoreDrillRequest) (any, error)
	UpdateRestoreDrill(context.Context, string, string, string, storepkg.RestoreDrillUpdate) (any, error)
}

func restoreDrillDecision(match []string, method string) bool {
	return match != nil && method == http.MethodPost && (match[3] == "approve" || match[3] == "reject")
}

func handleRestoreDrill(w http.ResponseWriter, r *http.Request, store HealthStore, match []string, role string) {
	asset, id, action := match[1], match[2], match[3]
	read := r.Method == http.MethodGet && action == ""
	decide := restoreDrillDecision(match, r.Method)
	operate := r.Method == http.MethodPost && !decide && (role == "admin" || role == "operator")
	if !read && !(decide && role == "approver") && !operate {
		respond(w, http.StatusForbidden, map[string]string{"error": "当前账号无权操作恢复演练"})
		return
	}
	db, ok := store.(restoreDrillStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "恢复演练服务不可用"})
		return
	}
	var value any
	var err error
	status := http.StatusOK
	switch {
	case read:
		value, err = db.RestoreDrills(r.Context(), asset)
	case action == "":
		var input storepkg.RestoreDrillRequest
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrInvalidRestoreDrill
		} else {
			value, err = db.RequestRestoreDrill(r.Context(), asset, input)
			status = http.StatusCreated
		}
	case decide:
		value, err = db.UpdateRestoreDrill(r.Context(), asset, id, action, storepkg.RestoreDrillUpdate{})
	default:
		var input storepkg.RestoreDrillUpdate
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrInvalidRestoreDrill
		} else {
			value, err = db.UpdateRestoreDrill(r.Context(), asset, id, action, input)
		}
	}
	if err == nil {
		w.Header().Set("Cache-Control", "no-store")
		respond(w, status, value)
		return
	}
	status, message := http.StatusServiceUnavailable, "恢复演练记录暂不可用"
	switch {
	case errors.Is(err, storepkg.ErrInvalidRestoreDrill):
		status, message = http.StatusBadRequest, "请核对演练参数和隔离目标"
	case errors.Is(err, storepkg.ErrRestoreDrillConflict):
		status, message = http.StatusConflict, "请核对演练状态、原事件、目标和采样证据"
	case errors.Is(err, storepkg.ErrAssetNotFound):
		status, message = http.StatusNotFound, "资产不存在"
	}
	respond(w, status, map[string]string{"error": message})
}
