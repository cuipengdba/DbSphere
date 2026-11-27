package httpapi

import (
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var postmortemPath = regexp.MustCompile(`^/api/events/(` + accessUUID + `)/postmortem$`)

func handlePostmortem(w http.ResponseWriter, r *http.Request, db HealthStore, id, role string, scope []string) {
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "复盘记录暂不可用"})
		return
	}
	var value any
	var err error
	status := http.StatusOK
	switch {
	case r.Method == http.MethodGet:
		value, err = store.ReadPostmortem(r.Context(), scope, id)
	case r.Method == http.MethodPost && (role == "admin" || role == "operator"):
		var input storepkg.PostmortemInput
		if readJSONLimit(w, r, &input, 32768) != nil {
			err = storepkg.ErrServiceReportInput
		} else {
			value, err = store.SavePostmortem(r.Context(), scope, id, input)
			status = http.StatusCreated
		}
	default:
		respond(w, http.StatusForbidden, map[string]string{"error": "当前账号无权更新复盘"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		respond(w, status, value)
		return
	}
	status, message := http.StatusServiceUnavailable, "复盘记录暂不可用"
	switch {
	case errors.Is(err, storepkg.ErrEventNotFound):
		status, message = http.StatusNotFound, "事件不存在"
	case errors.Is(err, storepkg.ErrServiceReportInput):
		status, message = http.StatusBadRequest, "请核对复盘内容和操作"
	case errors.Is(err, storepkg.ErrServiceReportConflict):
		status, message = http.StatusConflict, "复盘状态或版本已变化，请重新读取"
	}
	respond(w, status, map[string]string{"error": message})
}
