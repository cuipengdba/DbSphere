package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var rcaRulePath = regexp.MustCompile(`^/api/rca/rules/([a-z][a-z0-9_]{1,63})$`)
var rcaReplayPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/rca-replay$`)

type rcaRuleStore interface {
	RCARules(context.Context, string) (any, error)
	UpdateRCARule(context.Context, string, storepkg.RCARuleInput) (any, error)
	ReplayRCARule(context.Context, string, string) (any, error)
}

func handleRCAPolicy(w http.ResponseWriter, r *http.Request, db HealthStore, role, code, eventID string) {
	if role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要管理员权限"})
		return
	}
	store, ok := db.(rcaRuleStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "根因规则工作台暂不可用"})
		return
	}
	var value any
	var err error
	switch {
	case eventID != "":
		value, err = store.ReplayRCARule(r.Context(), eventID, r.URL.Query().Get("rule"))
	case r.Method == http.MethodPatch:
		var input storepkg.RCARuleInput
		if readJSONLimit(w, r, &input, 2048) != nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "根因规则格式无效"})
			return
		}
		value, err = store.UpdateRCARule(r.Context(), code, input)
	default:
		value, err = store.RCARules(r.Context(), code)
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, value)
	case errors.Is(err, storepkg.ErrEventNotFound), errors.Is(err, storepkg.ErrRCARuleNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, storepkg.ErrInvalidRCARule):
		respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, storepkg.ErrRCARuleConflict):
		respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "根因规则工作台暂不可用"})
	}
}
