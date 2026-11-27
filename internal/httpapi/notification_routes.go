package httpapi

import (
	"errors"
	"net/http"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const notificationRoutesPath = "/api/admin/notification-routes"

func handleNotificationRoutes(w http.ResponseWriter, r *http.Request, db HealthStore, role string) {
	if role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需使用管理员账号配置通知路由"})
		return
	}
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "通知路由暂不可用"})
		return
	}
	var result any
	var err error
	switch r.Method {
	case http.MethodGet:
		result, err = store.NotificationRoutes(r.Context(), r.URL.Query().Get("organization"))
	case http.MethodPut:
		var input storepkg.NotificationRoute
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrNotificationRoute
		} else {
			err = store.SaveNotificationRoute(r.Context(), input)
		}
		result = map[string]bool{"ok": true}
	default:
		w.Header().Set("Allow", "GET, PUT")
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "不支持此通知路由操作"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, storepkg.ErrNotificationRoute):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请核对组织、告警分组、通道和升级时限"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "通知路由暂不可用"})
	default:
		respond(w, http.StatusOK, result)
	}
}
