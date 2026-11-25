package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func (a *AuthConfig) activeAI(ctx context.Context) (ai.ActiveSettings, error) {
	if a.AIRuntime != nil {
		return a.AIRuntime.Current(ctx)
	}
	return ai.ActiveSettings{Client: a.AI, Settings: ai.Settings{DailyLimit: a.AIDailyLimit}}, nil
}

func handleAISettings(w http.ResponseWriter, r *http.Request, a *AuthConfig, role string) bool {
	const path = "/api/admin/ai-model"
	probe := r.URL.Path == path+"/test"
	if r.URL.Path != path && !probe {
		return false
	}
	if role != "admin" || storepkg.OrganizationFromContext(r.Context()) != "" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要平台管理员权限"})
		return true
	}
	if a.AIRuntime == nil {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "模型管理暂不可用"})
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var err error
	switch {
	case !probe && r.Method == http.MethodGet:
		var state ai.ActiveSettings
		state, err = a.AIRuntime.Current(ctx)
		if err == nil {
			respond(w, http.StatusOK, state.Public())
			return true
		}
	case probe && r.Method == http.MethodPost || !probe && r.Method == http.MethodPut:
		var request struct {
			ai.Settings
			Revision string `json:"revision"`
			KeepKey  bool   `json:"keepKey"`
		}
		if readJSONLimit(w, r, &request, 10000) != nil || len(request.Revision) != 64 {
			err = ai.ErrInvalidConfiguration
		} else if probe {
			err = a.AIRuntime.Probe(ctx, request.Revision, request.Settings, request.KeepKey)
		} else {
			err = a.AIRuntime.Save(ctx, request.Revision, &request.Settings, request.KeepKey)
		}
		if err == nil {
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return true
		}
	case !probe && r.Method == http.MethodDelete:
		var request struct {
			Revision string `json:"revision"`
		}
		if readJSONLimit(w, r, &request, 256) != nil || len(request.Revision) != 64 {
			err = ai.ErrInvalidConfiguration
		} else {
			err = a.AIRuntime.Save(ctx, request.Revision, nil, false)
		}
		if err == nil {
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return true
		}
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方法不可用"})
		return true
	}
	switch {
	case errors.Is(err, ai.ErrInvalidConfiguration):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请检查模型名称、接口地址和参数。远端 HTTPS 地址须由部署端授权，变更接口时需重新填写密钥。"})
	case errors.Is(err, ai.ErrSettingsChanged):
		respond(w, http.StatusConflict, map[string]string{"error": "模型配置已更新，请刷新后重试"})
	case errors.Is(err, ai.ErrProbeBusy):
		respond(w, http.StatusTooManyRequests, map[string]string{"error": "模型测试频繁，请稍后重试"})
	case errors.Is(err, ai.ErrUnavailable):
		respond(w, http.StatusBadGateway, map[string]string{"error": "模型接口未返回有效结果，请检查服务、模型和密钥"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "模型配置暂不可读写"})
	}
	return true
}
