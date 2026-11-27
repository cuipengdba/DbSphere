package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var eventStatementPath = regexp.MustCompile(`^/api/events/(` + accessUUID + `)/statement-comparison$`)
var eventStatementTextPath = regexp.MustCompile(`^/api/events/(` + accessUUID + `)/statement-comparison/(` + accessUUID + `)/text$`)

const statementTextPolicyPath = "/api/admin/statement-text-retention"

type statementComparisonStore interface {
	StatementComparisonEvent(context.Context, string) (map[string]any, error)
	CompareEventStatements(context.Context, string, storepkg.StatementComparisonInput, bool, []byte) (storepkg.StatementComparison, error)
	StatementTextPolicy(context.Context, string) (storepkg.StatementTextPolicy, error)
	SetStatementTextPolicy(context.Context, storepkg.StatementTextPolicy) (storepkg.StatementTextPolicy, error)
	ReadStatementText(context.Context, string, string, []byte) (map[string]any, error)
}

func handleStatementComparison(w http.ResponseWriter, r *http.Request, db HealthStore, role string, key []byte) bool {
	match := eventStatementPath.FindStringSubmatch(r.URL.Path)
	text := eventStatementTextPath.FindStringSubmatch(r.URL.Path)
	policy := r.URL.Path == statementTextPolicyPath
	if match == nil && text == nil && !policy {
		return false
	}
	if (policy || text != nil) && role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "SQL 原文管理需要管理员权限"})
		return true
	}
	s, ok := db.(statementComparisonStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "SQL 对比服务不可用"})
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var result any
	var err error
	status := http.StatusOK
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case policy && r.Method == http.MethodGet:
		result, err = s.StatementTextPolicy(ctx, r.URL.Query().Get("organizationId"))
	case policy && r.Method == http.MethodPut:
		var input storepkg.StatementTextPolicy
		if readJSONLimit(w, r, &input, 2048) != nil {
			err = storepkg.ErrStatementComparison
			break
		}
		result, err = s.SetStatementTextPolicy(ctx, input)
	case text != nil && r.Method == http.MethodPost:
		result, err = s.ReadStatementText(ctx, text[1], text[2], key)
	case match != nil && r.Method == http.MethodGet:
		result, err = s.StatementComparisonEvent(ctx, match[1])
	case match != nil && r.Method == http.MethodPost:
		if role != "admin" && role != "operator" {
			respond(w, http.StatusForbidden, map[string]string{"error": "仅管理员和运维人员可执行 SQL 对比"})
			return true
		}
		var input struct {
			storepkg.StatementComparisonInput
			BaselinePlanJSON string `json:"baselinePlanJson"`
			CurrentPlanJSON  string `json:"currentPlanJson"`
			Save             bool   `json:"save"`
		}
		if readJSONLimit(w, r, &input, 256<<10) != nil ||
			(input.BaselinePlanJSON != "" && len(input.BaselinePlan) > 0) ||
			(input.CurrentPlanJSON != "" && len(input.CurrentPlan) > 0) {
			respond(w, http.StatusBadRequest, map[string]string{"error": "请检查 SQL 指纹、时间范围和计划 JSON"})
			return true
		}
		if input.BaselinePlanJSON != "" {
			input.BaselinePlan = []byte(input.BaselinePlanJSON)
		}
		if input.CurrentPlanJSON != "" {
			input.CurrentPlan = []byte(input.CurrentPlanJSON)
		}
		if input.Save {
			status = http.StatusCreated
		}
		if input.SQLText != "" && role != "admin" {
			respond(w, http.StatusForbidden, map[string]string{"error": "只有管理员可保存 SQL 原文"})
			return true
		}
		result, err = s.CompareEventStatements(ctx, match[1], input.StatementComparisonInput, input.Save, key)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "请求方法不可用"})
		return true
	}
	switch {
	case errors.Is(err, storepkg.ErrEventNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "事件不存在"})
	case errors.Is(err, storepkg.ErrStatementTextUnavailable):
		if text != nil {
			respond(w, http.StatusNotFound, map[string]string{"error": "SQL 原文未保留或已到期"})
		} else {
			respond(w, http.StatusConflict, map[string]string{"error": "该组织未启用 SQL 原文保留"})
		}
	case errors.Is(err, storepkg.ErrStatementComparison):
		respond(w, http.StatusBadRequest, map[string]string{"error": "请核对引擎、指纹、事件时间、计划格式及复测记录上限"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "SQL 对比数据暂不可用"})
	default:
		respond(w, status, result)
	}
	return true
}
