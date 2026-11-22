package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var assetRiskPath = regexp.MustCompile("^/api/assets/(" + accessUUID + ")/risk(?:/(review|mode|backtest))?$")

type assetRiskStore interface {
	RiskForecasts(context.Context, string) (any, error)
	BacktestRiskForecast(context.Context, string) (any, error)
	ReviewRiskForecast(context.Context, string, string, string) error
	SetRiskMode(context.Context, string, bool) error
}

func handleAssetRisk(w http.ResponseWriter, r *http.Request, db HealthStore, role string) bool {
	match := assetRiskPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return false
	}
	risk, ok := db.(assetRiskStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Risk forecasts unavailable"})
		return true
	}
	id, action := match[1], match[2]
	var result any = map[string]bool{"ok": true}
	var err error
	switch {
	case r.Method == http.MethodGet && action == "":
		result, err = risk.RiskForecasts(r.Context(), id)
	case r.Method == http.MethodGet && action == "backtest":
		result, err = risk.BacktestRiskForecast(r.Context(), id)
	case r.Method == http.MethodPost && action == "review" && (role == "admin" || role == "operator"):
		var input struct {
			ObservationID string `json:"observationId"`
			Verdict       string `json:"verdict"`
		}
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrRiskInput
		} else {
			err = risk.ReviewRiskForecast(r.Context(), id, input.ObservationID, input.Verdict)
		}
	case r.Method == http.MethodPut && action == "mode" && role == "admin":
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if readJSONLimit(w, r, &input, 4096) != nil || input.Enabled == nil {
			err = storepkg.ErrRiskInput
		} else {
			err = risk.SetRiskMode(r.Context(), id, *input.Enabled)
		}
	default:
		respond(w, http.StatusForbidden, map[string]string{"error": "Risk action unavailable to this role"})
		return true
	}
	switch {
	case err == nil:
		respond(w, http.StatusOK, result)
	case errors.Is(err, storepkg.ErrRiskInput):
		respond(w, http.StatusBadRequest, map[string]string{"error": "风险复核记录无效"})
	case errors.Is(err, storepkg.ErrRiskReviewRequired):
		respond(w, http.StatusConflict, map[string]string{"error": "启用前请确认一条影子预警"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "风险基线暂不可用"})
	}
	return true
}
