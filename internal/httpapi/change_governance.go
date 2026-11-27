package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var changeGovernancePath = regexp.MustCompile(`^/api/assets/(` + accessUUID + `)/change-governance(?:/(windows|requests)|/(` + accessUUID + `)/(approve|reject|baseline|review|recovery))?$`)

type changeGovernanceStore interface {
	ChangeGovernance(context.Context, string) (any, error)
	CreateChangeWindow(context.Context, string, storepkg.ChangeWindowInput) (any, error)
	RequestChange(context.Context, string, storepkg.ChangeOrderInput) (any, error)
	DecideChange(context.Context, string, string, bool) (any, error)
	CheckChange(context.Context, string, string, string, storepkg.ChangeReview, []byte) (any, error)
}

type changeReceiptStore interface {
	RecordChangeReceipt(context.Context, string, storepkg.ChangeReceipt) (any, bool, error)
}

func changeGovernanceRole(match []string, method, role string) bool {
	if method == http.MethodGet {
		return match[2] == "" && match[4] == ""
	}
	if method != http.MethodPost {
		return false
	}
	if match[2] == "windows" {
		return role == "admin"
	}
	switch match[4] {
	case "approve", "reject":
		return role == "approver"
	case "baseline", "review", "recovery":
		return role == "admin" || role == "operator"
	default:
		return match[2] == "requests" && (role == "admin" || role == "operator")
	}
}

func handleChangeGovernance(w http.ResponseWriter, r *http.Request, store HealthStore, match []string, role string, key []byte) {
	if !changeGovernanceRole(match, r.Method, role) {
		respond(w, http.StatusForbidden, map[string]string{"error": "当前账号无权执行此变更操作"})
		return
	}
	db, ok := store.(changeGovernanceStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "变更治理服务不可用"})
		return
	}
	ctx, asset, change, action := r.Context(), match[1], match[3], match[4]
	var value any
	var err error
	status := http.StatusOK
	switch {
	case r.Method == http.MethodGet:
		value, err = db.ChangeGovernance(ctx, asset)
	case match[2] == "windows":
		var input storepkg.ChangeWindowInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrInvalidChange
			break
		}
		value, err = db.CreateChangeWindow(ctx, asset, input)
		status = http.StatusCreated
	case match[2] == "requests":
		var input storepkg.ChangeOrderInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrInvalidChange
			break
		}
		value, err = db.RequestChange(ctx, asset, input)
		status = http.StatusCreated
	case action == "approve" || action == "reject":
		value, err = db.DecideChange(ctx, asset, change, action == "approve")
	default:
		var input storepkg.ChangeReview
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrInvalidChange
			break
		}
		value, err = db.CheckChange(ctx, asset, change, action, input, key)
	}
	if err != nil {
		writeChangeGovernanceError(w, err)
		return
	}
	respond(w, status, value)
}

func writeChangeGovernanceError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "变更记录暂不可用"
	switch {
	case errors.Is(err, storepkg.ErrAssetNotFound):
		status, message = http.StatusNotFound, "资产或变更单不存在"
	case errors.Is(err, storepkg.ErrChangeState):
		status, message = http.StatusConflict, "请核对变更状态、时间窗、基线与外部单号"
	case errors.Is(err, storepkg.ErrInvalidChange):
		status, message = http.StatusBadRequest, "变更参数无效"
	case errors.Is(err, storepkg.ErrCollectionUnavailable):
		status, message = http.StatusConflict, "该资产无法现场复采"
	}
	respond(w, status, map[string]string{"error": message})
}

func handleChangeReceipt(w http.ResponseWriter, r *http.Request, store HealthStore, organization string) {
	db, ok := store.(changeReceiptStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Change receipts unavailable"})
		return
	}
	var input storepkg.ChangeReceipt
	if readJSONLimit(w, r, &input, 4096) != nil {
		writeChangeGovernanceError(w, storepkg.ErrInvalidChange)
		return
	}
	value, duplicate, err := db.RecordChangeReceipt(r.Context(), organization, input)
	if err != nil {
		writeChangeGovernanceError(w, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	respond(w, status, map[string]any{"schemaVersion": 1, "data": value})
}
