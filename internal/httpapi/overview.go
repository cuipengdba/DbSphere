package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const monitoringOverviewPath = "/api/monitoring/overview"

type monitoringOverviewReader interface {
	MonitoringOverview(context.Context, []string, storepkg.OverviewQuery) (json.RawMessage, error)
}

func readMonitoringOverview(w http.ResponseWriter, r *http.Request, db HealthStore, auth *AuthConfig, ids []string) {
	reader, ok := db.(monitoringOverviewReader)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Monitoring overview unavailable"})
		return
	}
	values := r.URL.Query()
	now := time.Now().UTC()
	query := storepkg.OverviewQuery{Organization: values.Get("organization"), Environment: values.Get("environment"),
		Cluster: values.Get("cluster"), From: now.Add(-time.Hour), To: now}
	if auth.AutoCollect {
		query.StaleSeconds = 2 * max(10, auth.CollectEvery)
	}
	var err error
	if values.Get("from") != "" || values.Get("to") != "" {
		query.From, err = time.Parse(time.RFC3339, values.Get("from"))
		if err == nil {
			query.To, err = time.Parse(time.RFC3339, values.Get("to"))
		}
	}
	if offset := values.Get("offset"); offset != "" && err == nil {
		query.Offset, err = strconv.Atoi(offset)
	}
	span := query.To.Sub(query.From)
	if err != nil || span < time.Minute || span > 7*24*time.Hour || query.To.After(now.Add(time.Minute)) ||
		query.Offset < 0 || query.Offset > 36000 || len(query.Organization) > 36 || len(query.Environment) > 80 || len(query.Cluster) > 256 {
		respond(w, http.StatusBadRequest, map[string]string{"error": "监控总览筛选或时间范围无效"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	data, err := reader.MonitoringOverview(r.Context(), ids, query)
	if err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "监控总览暂不可用"})
		return
	}
	respond(w, http.StatusOK, data)
}
