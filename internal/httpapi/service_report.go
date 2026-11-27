package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var serviceReportPath = regexp.MustCompile(`^/api/service-reports(?:/(saved|(` + accessUUID + `)(?:/(signoff))?))?$`)

func handleServiceReports(w http.ResponseWriter, r *http.Request, db HealthStore, match []string, role string, scope []string, interval int) {
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "服务报告暂不可用"})
		return
	}
	ctx := r.Context()
	canWrite := role == "admin" || role == "operator"
	method := r.Method
	status := http.StatusOK
	var result any
	var err error
	switch {
	case match[1] == "saved" && method == http.MethodGet:
		result, err = store.ListServiceReports(ctx, scope, r.URL.Query().Get("organization"))
	case match[2] != "" && match[3] == "" && method == http.MethodGet:
		result, err = store.SavedServiceReport(ctx, scope, match[2], nil)
	case match[3] == "signoff" && method == http.MethodPost && canWrite:
		var input struct{ Note string }
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrServiceReportInput
			break
		}
		result, err = store.SavedServiceReport(ctx, scope, match[2], &input.Note)
	case match[1] == "" && method == http.MethodGet:
		values := r.URL.Query()
		query := storepkg.ServiceReportQuery{Organization: values.Get("organization"), Service: values.Get("service"),
			AssetGroup: values.Get("assetGroup"), Tag: values.Get("tag")}
		query.From, err = time.Parse(time.RFC3339Nano, values.Get("from"))
		if err == nil {
			query.To, err = time.Parse(time.RFC3339Nano, values.Get("to"))
		}
		if err != nil {
			err = storepkg.ErrServiceReportInput
			break
		}
		result, err = store.ServiceReport(ctx, scope, query, interval, false)
	case match[1] == "" && method == http.MethodPost && canWrite:
		var query storepkg.ServiceReportQuery
		if readJSONLimit(w, r, &query, 4096) != nil {
			err = storepkg.ErrServiceReportInput
			break
		}
		status = http.StatusCreated
		result, err = store.ServiceReport(ctx, scope, query, interval, true)
	default:
		respond(w, http.StatusForbidden, map[string]string{"error": "当前账号无权执行此报告操作"})
		return
	}
	if err == nil {
		w.Header().Set("Cache-Control", "no-store")
		respond(w, status, result)
		return
	}
	switch {
	case errors.Is(err, storepkg.ErrServiceReportInput):
		status = http.StatusBadRequest
		result = map[string]string{"error": "请核对组织、资产筛选和最长 31 天的时间窗"}
	case errors.Is(err, storepkg.ErrServiceReportLimit):
		status = http.StatusBadRequest
		result = map[string]string{"error": "单份报告最多包含 50 个资产，请缩小筛选范围"}
	case errors.Is(err, storepkg.ErrServiceReportConflict):
		status = http.StatusNotFound
		result = map[string]string{"error": "报告不可见或已签收"}
	default:
		status = http.StatusServiceUnavailable
		result = map[string]string{"error": "服务报告暂不可用"}
	}
	respond(w, status, result)
}
