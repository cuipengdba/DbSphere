package httpapi

import (
	"context"
	"errors"
	"net/http"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func handleRecoveryInventory(w http.ResponseWriter, r *http.Request, db HealthStore, role string) bool {
	if r.URL.Path != "/api/admin/recovery-inventory" {
		return false
	}
	if role != "admin" || storepkg.OrganizationFromContext(r.Context()) != "" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要平台管理员权限"})
		return true
	}
	if r.Method != http.MethodGet {
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "恢复核对只支持读取"})
		return true
	}
	inventory, ok := db.(interface {
		RecoveryInventory(context.Context, string) (any, error)
	})
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "恢复核对暂不可用"})
		return true
	}
	value, err := inventory.RecoveryInventory(r.Context(), r.URL.Query().Get("event"))
	switch {
	case errors.Is(err, storepkg.ErrRecoveryInventoryInput):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请核对事件 ID"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "监控库核对暂不可用"})
	default:
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, value)
	}
	return true
}
