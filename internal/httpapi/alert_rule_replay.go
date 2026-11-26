package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var alertRuleReplayPath = regexp.MustCompile(`^/api/assets/(` + accessUUID + `)/alert-rules/([a-z][a-z0-9_]{1,63})/(replay|revisions)(?:/(` + accessUUID + `))?$`)

type alertRuleReplayStore interface {
	ReplayAlertRule(context.Context, string, string, storepkg.AlertReplayInput, bool) (any, error)
	AlertRuleRevisions(context.Context, string, string) (any, error)
	ChangeAlertRevision(context.Context, string, string, string, storepkg.AlertRevisionAction) (any, error)
}

func handleAlertRuleReplay(w http.ResponseWriter, r *http.Request, db HealthStore, role string) bool {
	match := alertRuleReplayPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return false
	}
	if role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限"})
		return true
	}
	store, ok := db.(alertRuleReplayStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "告警规则回放暂不可用"})
		return true
	}
	asset, rule, endpoint, revision := match[1], match[2], match[3], match[4]
	ctx := r.Context()
	var result any
	var err error
	status := http.StatusOK
	switch {
	case r.Method == http.MethodGet && endpoint == "revisions" && revision == "":
		result, err = store.AlertRuleRevisions(ctx, asset, rule)
	case r.Method == http.MethodPost && revision == "":
		var input storepkg.AlertReplayInput
		if readJSONLimit(w, r, &input, 8192) != nil {
			err = storepkg.ErrAlertReplayWindow
			break
		}
		result, err = store.ReplayAlertRule(ctx, asset, rule, input, endpoint == "revisions")
		if endpoint == "revisions" {
			status = http.StatusCreated
		}
	case r.Method == http.MethodPost && endpoint == "revisions":
		var input storepkg.AlertRevisionAction
		if readJSONLimit(w, r, &input, 8192) != nil {
			err = storepkg.ErrInvalidAlertRule
			break
		}
		result, err = store.ChangeAlertRevision(ctx, asset, rule, revision, input)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "不支持此操作"})
		return true
	}
	switch {
	case err == nil:
		respond(w, status, result)
	case errors.Is(err, storepkg.ErrAssetNotFound), errors.Is(err, storepkg.ErrAlertRuleNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "资产、规则或修订不存在"})
	case errors.Is(err, storepkg.ErrAlertReplayWindow):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请选择 7 天内且最多包含 2880 条原始采样的时间窗。保存修订需要有效读数。"})
	case errors.Is(err, storepkg.ErrInvalidAlertRule), errors.Is(err, storepkg.ErrAlertRuleUnsupported):
		respond(w, http.StatusBadRequest, map[string]string{"error": "规则配置或修订操作无效"})
	case errors.Is(err, storepkg.ErrAlertRevisionConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "请先核对影子采样和复核结论。基准规则改变时需要重新回放。"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "告警规则回放暂不可用"})
	}
	return true
}
