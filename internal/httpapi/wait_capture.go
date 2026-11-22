package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func runWaitCapture(w http.ResponseWriter, r *http.Request, db HealthStore, assetID string, key []byte) {
	collector, ok := db.(interface {
		RunWaitCapture(context.Context, string, string, string, []byte) (any, error)
	})
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "短时等待采样暂不可用"})
		return
	}
	var input struct {
		Profile string `json:"profile"`
		EventID string `json:"eventId"`
	}
	if err := readJSONLimit(w, r, &input, 1024); err != nil ||
		(input.Profile != "standard" && input.Profile != "enhanced") ||
		(input.EventID != "" && !eventPath.MatchString("/api/events/"+input.EventID)) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "采样档位或事件 ID 无效"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := collector.RunWaitCapture(ctx, assetID, input.EventID, input.Profile, key)
	status, message := http.StatusServiceUnavailable, "短时等待采样暂不可用"
	switch {
	case err == nil:
		respond(w, http.StatusCreated, result)
		return
	case errors.Is(err, storepkg.ErrAssetNotFound):
		status, message = http.StatusNotFound, "资产不存在"
	case errors.Is(err, storepkg.ErrEventNotFound):
		status, message = http.StatusNotFound, "该资产没有这个待处理事件"
	case errors.Is(err, storepkg.ErrWaitCaptureBusy):
		status, message = http.StatusTooManyRequests, "已有采样正在执行，或已达到实例间隔和事件保留上限"
	case errors.Is(err, storepkg.ErrWaitCaptureTarget):
		status, message = http.StatusBadGateway, "无法读取目标 PostgreSQL 的会话状态"
	}
	respond(w, status, map[string]string{"error": message})
}
