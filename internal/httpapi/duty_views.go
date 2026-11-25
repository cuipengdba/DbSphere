package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var dutyViewPath = regexp.MustCompile("^/api/duty-views(?:/(" + accessUUID + "))?$")

type dutyViewStore interface {
	DutyViews(context.Context, string, string) ([]storepkg.DutyView, []storepkg.DutyViewTeam, error)
	SaveDutyView(context.Context, string, string, string, string, string, string, bool) (string, error)
	DeleteDutyView(context.Context, string, string, string) error
}

func handleDutyViews(w http.ResponseWriter, r *http.Request, db HealthStore, match []string,
	principal storepkg.AccessAccount, role string, scope []string) {
	storage, ok := db.(dutyViewStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "值班视图存储不可用"})
		return
	}
	owner := "builtin:" + role
	if principal.ID != "" {
		owner = "account:" + principal.ID
	} else if role != "admin" && r.Method != http.MethodGet {
		respond(w, http.StatusForbidden, map[string]string{"error": "请使用独立账号保存值班视图"})
		return
	}
	ctx := r.Context()
	var result any
	var err error
	id := match[1]
	switch {
	case r.Method == http.MethodGet && id == "":
		var views []storepkg.DutyView
		var teams []storepkg.DutyViewTeam
		views, teams, err = storage.DutyViews(ctx, principal.ID, owner)
		for index := range views {
			views[index].Path, _ = storepkg.DutyViewRoute(views[index].Path, scope)
		}
		result = map[string]any{"views": views, "teams": teams, "canSave": principal.ID != "" || role == "admin"}
	case r.Method == http.MethodPut:
		var input struct {
			Name   string `json:"name"`
			Path   string `json:"path"`
			TeamID string `json:"teamId"`
			Import bool   `json:"import"`
		}
		if readJSONLimit(w, r, &input, 9000) != nil || (input.Import && (id != "" || input.TeamID != "")) {
			err = storepkg.ErrAccessInput
			break
		}
		var route string
		route, err = storepkg.DutyViewRoute(input.Path, scope)
		if err != nil {
			break
		}
		id, err = storage.SaveDutyView(ctx, principal.ID, owner, id, input.TeamID, input.Name, route, input.Import)
		result = map[string]string{"id": id}
	case r.Method == http.MethodDelete && id != "":
		err = storage.DeleteDutyView(ctx, principal.ID, owner, id)
		result = map[string]bool{"ok": true}
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方法不可用"})
		return
	}
	switch {
	case errors.Is(err, storepkg.ErrAccessInput):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请检查视图名称、范围和团队"})
	case errors.Is(err, storepkg.ErrAccessMissing):
		respond(w, http.StatusNotFound, map[string]string{"error": "视图或团队不可用"})
	case errors.Is(err, storepkg.ErrAccessConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "该团队已有同名视图，请使用其他名称"})
	case errors.Is(err, storepkg.ErrDutyViewLimit):
		respond(w, http.StatusConflict, map[string]string{"error": "个人或每个团队最多保存 20 个视图"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "值班视图暂不可读写"})
	default:
		respond(w, http.StatusOK, result)
	}
}
