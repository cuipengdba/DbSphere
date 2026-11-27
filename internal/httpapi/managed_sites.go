package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var managedSitesPath = regexp.MustCompile(`^/api/managed-sites(?:/([a-z0-9][a-z0-9-]{0,62})/upgrades)?$`)

func handleManagedSites(w http.ResponseWriter, r *http.Request, db HealthStore, site, role string, account storepkg.AccessAccount) {
	admin := role == "admin"
	if !admin && (role != "operator" || account.ID == "" || account.OrganizationID == "") {
		respond(w, http.StatusForbidden, map[string]string{"error": "需要站点管理员或受委派的值班员权限"})
		return
	}
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "站点管理暂不可用"})
		return
	}
	if site == "" && r.Method == http.MethodPut && (!admin || account.OrganizationID != "") {
		respond(w, http.StatusForbidden, map[string]string{"error": "请由平台管理员登记站点及委派范围"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var value any
	var err error
	switch {
	case site == "" && r.Method == http.MethodGet:
		value, err = store.ManagedSites(ctx, admin, account.ID)
	case site == "" && r.Method == http.MethodPut:
		var input storepkg.ManagedSiteInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = remoteobs.ErrInvalid
		} else {
			err = store.SaveManagedSite(ctx, input)
		}
		value = map[string]bool{"ok": true}
	case site != "" && r.Method == http.MethodGet:
		value, err = store.SiteUpgradeHistory(ctx, site, admin, account.ID)
	case site != "" && r.Method == http.MethodPost:
		var input storepkg.SiteUpgradeInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = remoteobs.ErrInvalid
		} else {
			err = store.RecordSiteUpgrade(ctx, site, admin, account.ID, input)
		}
		value = map[string]bool{"ok": true}
	default:
		if site == "" {
			w.Header().Set("Allow", "GET, PUT")
		} else {
			w.Header().Set("Allow", "GET, POST")
		}
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "不支持此站点操作"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case err == nil:
		respond(w, http.StatusOK, value)
	case errors.Is(err, remoteobs.ErrInvalid):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请核对站点参数、版本和维护记录"})
	case errors.Is(err, storepkg.ErrSiteUnavailable):
		respond(w, http.StatusNotFound, map[string]string{"error": "站点、组织或委派账号不可用"})
	case errors.Is(err, storepkg.ErrSiteTransition):
		respond(w, http.StatusConflict, map[string]string{"error": "请核对当前修订、前序批次或站点上报版本"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "站点管理暂不可用"})
	}
}
