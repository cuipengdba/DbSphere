package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/store"
)

type assetImportStore interface {
	ImportAssets(context.Context, assets.ImportRequest, bool, []byte) (any, error)
	ExportAssets(context.Context, string) (any, error)
	ProbeImportedAssets(context.Context, []string, []byte) (any, error)
}

type assetOnboardingStore interface {
	OnboardingBatches(context.Context, string) (any, error)
	OnboardingBatch(context.Context, string) (any, error)
	CheckOnboardingBatch(context.Context, string, []string, []byte) (any, error)
}

func handleAssetImport(w http.ResponseWriter, r *http.Request, db any, role string, key []byte) bool {
	const prefix = "/api/assets/import/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	if role != "admin" || store.OrganizationFromContext(r.Context()) != "" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限"})
		return true
	}
	importer, ok := db.(assetImportStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "资产批量纳管暂不可用"})
		return true
	}
	var result any
	var err error
	action := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case action == "batches" || strings.HasPrefix(action, "batches/"):
		batches, ok := db.(assetOnboardingStore)
		if !ok {
			respond(w, http.StatusNotImplemented, map[string]string{"error": "接入批次暂不可用"})
			return true
		}
		id := strings.TrimPrefix(action, "batches/")
		switch {
		case action == "batches" && r.Method == http.MethodGet:
			result, err = batches.OnboardingBatches(r.Context(), r.URL.Query().Get("after"))
		case action != "batches" && r.Method == http.MethodGet:
			result, err = batches.OnboardingBatch(r.Context(), id)
		case strings.HasSuffix(id, "/check") && r.Method == http.MethodPost:
			var request struct{ IDs []string `json:"ids"` }
			if readJSON(w, r, &request) != nil {
				err = assets.ErrInvalidImport
			} else {
				result, err = batches.CheckOnboardingBatch(r.Context(), strings.TrimSuffix(id, "/check"), request.IDs, key)
			}
		default:
			respond(w, http.StatusNotFound, map[string]string{"error": "批量纳管接口不存在"})
			return true
		}
	case action == "cloudnativepg" && r.Method == http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		result = collector.PreviewCloudNativePG(ctx, collector.KubernetesTarget{
			Cluster: r.URL.Query().Get("context"), Namespace: r.URL.Query().Get("namespace"),
		})
	case action == "export" && r.Method == http.MethodGet:
		result, err = importer.ExportAssets(r.Context(), r.URL.Query().Get("after"))
	case action == "probe" && r.Method == http.MethodPost:
		var request struct {
			IDs []string `json:"ids"`
		}
		if readJSON(w, r, &request) != nil {
			err = assets.ErrInvalidImport
		} else {
			result, err = importer.ProbeImportedAssets(r.Context(), request.IDs, key)
		}
	case (action == "preview" || action == "apply") && r.Method == http.MethodPost:
		var request assets.ImportRequest
		if readJSONLimit(w, r, &request, 2<<20) != nil {
			err = assets.ErrInvalidImport
		} else {
			result, err = importer.ImportAssets(r.Context(), request, action == "apply", key)
		}
	default:
		respond(w, http.StatusNotFound, map[string]string{"error": "批量纳管接口不存在"})
		return true
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, result)
	case errors.Is(err, assets.ErrInvalidImport):
		respond(w, http.StatusBadRequest, map[string]string{"error": "清单、资产 ID 或分页参数无效，清单每批最多 100 项"})
	case errors.Is(err, store.ErrAssetImportConflict):
		respond(w, http.StatusConflict, map[string]any{"error": "资产配置已变化或存在冲突，请重新预览", "plan": result})
	case errors.Is(err, store.ErrOnboardingBusy):
		respond(w, http.StatusConflict, map[string]string{"error": "已有接入批次正在体检，请稍后重试"})
	case errors.Is(err, store.ErrOnboardingNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "接入批次或所选资产已不存在"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "资产批量纳管暂不可用"})
	}
	return true
}
