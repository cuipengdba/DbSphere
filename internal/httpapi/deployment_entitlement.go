package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/cuipengdba/dbsphere/internal/entitlement"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const deploymentEntitlementPath = "/api/admin/deployment-entitlement"

func handleDeploymentEntitlement(w http.ResponseWriter, r *http.Request, db HealthStore, role string) {
	if role != "admin" || storepkg.OrganizationFromContext(r.Context()) != "" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要平台管理员权限"})
		return
	}
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "部署授权暂不可用"})
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		wire, err := io.ReadAll(http.MaxBytesReader(w, r.Body, entitlement.MaxFileSize))
		if err == nil && r.Header.Get("Content-Type") != "application/octet-stream" {
			err = entitlement.ErrInvalid
		}
		if err != nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "请上传有效的授权文件"})
			return
		}
		switch err := store.ImportEntitlement(r.Context(), wire); {
		case errors.Is(err, entitlement.ErrInvalid):
			respond(w, http.StatusBadRequest, map[string]string{"error": "授权文件或签名无效"})
			return
		case errors.Is(err, storepkg.ErrEntitlementConflict):
			respond(w, http.StatusConflict, map[string]string{"error": "请核对授权部署编号、续期序号和服务器时钟"})
			return
		case err != nil:
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "部署授权存储暂不可用"})
			return
		}
	default:
		w.Header().Set("Allow", "GET, PUT")
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "不支持此授权操作"})
		return
	}
	state, err := store.EntitlementStatus(r.Context())
	if err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "部署授权暂不可用"})
		return
	}
	respond(w, http.StatusOK, state)
}

func respondExpansionDenied(w http.ResponseWriter) {
	respond(w, http.StatusConflict, map[string]string{"error": "当前部署授权无法增加容量，请核对授权期限和已用额度"})
}
