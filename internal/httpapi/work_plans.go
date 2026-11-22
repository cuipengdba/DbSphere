package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type WorkOrderProfile struct {
	AssetID        string `json:"assetId"`
	Action         string `json:"action"`
	DSN            string `json:"dsn"`
	Schema         string `json:"schema"`
	Relation       string `json:"relation"`
	MarkerColumn   string `json:"markerColumn"`
	RequiredMarker string `json:"requiredMarker"`
	ExcludedMarker string `json:"excludedMarker"`
}

var profileName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)
var targetIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)
var profileAssetID = regexp.MustCompile("^" + accessUUID + "$")
var workOrderPreviewPath = regexp.MustCompile("^/api/events/(" + accessUUID + ")/work-orders/preview$")
var errJobProfile = errors.New("invalid or unavailable work order target")
var errJobResult = errors.New("restored data does not match recovery markers")

func ParseWorkOrderProfiles(raw string) (map[string]WorkOrderProfile, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var profiles map[string]WorkOrderProfile
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profiles); err != nil || !json.Valid([]byte(raw)) || len(profiles) > 30 {
		return nil, errJobProfile
	}
	for name, profile := range profiles {
		if !profileName.MatchString(name) || !profileAssetID.MatchString(profile.AssetID) ||
			!targetIdentifier.MatchString(profile.Schema) || !targetIdentifier.MatchString(profile.Relation) ||
			strings.HasPrefix(profile.Schema, "pg_") || profile.Schema == "information_schema" {
			return nil, errJobProfile
		}
		if _, err := profile.connectionConfig(); err != nil {
			return nil, errJobProfile
		}
		switch profile.Action {
		case storepkg.WorkAnalyze:
			if profile.MarkerColumn != "" || profile.RequiredMarker != "" || profile.ExcludedMarker != "" {
				return nil, errJobProfile
			}
		case storepkg.WorkVerifyRestore:
			if !targetIdentifier.MatchString(profile.MarkerColumn) || profile.RequiredMarker == "" ||
				profile.ExcludedMarker == "" || profile.RequiredMarker == profile.ExcludedMarker ||
				len(profile.RequiredMarker) > 250 || len(profile.ExcludedMarker) > 250 {
				return nil, errJobProfile
			}
		default:
			return nil, errJobProfile
		}
	}
	return profiles, nil
}

func (p WorkOrderProfile) connectionConfig() (*pgx.ConnConfig, error) {
	if !strings.HasPrefix(p.DSN, "postgres://") && !strings.HasPrefix(p.DSN, "postgresql://") {
		return nil, errJobProfile
	}
	cfg, err := pgx.ParseConfig(p.DSN)
	if err != nil || cfg.Host == "" || strings.HasPrefix(cfg.Host, "/") || cfg.Database == "" || cfg.User == "" ||
		cfg.Password == "" || len(cfg.Fallbacks) != 0 {
		return nil, errJobProfile
	}
	cfg.ConnectTimeout = 4 * time.Second
	cfg.RuntimeParams = map[string]string{"application_name": "dbsphere-controlled-work", "statement_timeout": "35000"}
	if p.Action == storepkg.WorkVerifyRestore {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	} else {
		cfg.RuntimeParams["lock_timeout"] = "1000"
	}
	return cfg, nil
}

const workPlainRelation = `SELECT EXISTS(SELECT FROM pg_class c WHERE c.oid=to_regclass($1)
	AND c.relkind='r' AND c.relpersistence='p' AND NOT c.relispartition
	AND NOT EXISTS(SELECT FROM pg_inherits i WHERE i.inhparent=c.oid OR i.inhrelid=c.oid))`

func (p WorkOrderProfile) revision(key []byte) string {
	value, _ := json.Marshal(p)
	return security.CSRFToken(key, "work-profile:"+string(value))
}

type plannedWorkStore interface {
	WorkOrderTarget(context.Context, string, storepkg.WorkOrderInput) (storepkg.WorkOrderTarget, error)
	ExistingWorkOrder(context.Context, string, storepkg.WorkOrderInput) (json.RawMessage, error)
	CreatePlannedWorkOrder(context.Context, string, storepkg.WorkOrderInput, *storepkg.WorkOrderPlan) (json.RawMessage, error)
	StartWorkPlan(context.Context, string) (storepkg.WorkOrderExecution, error)
	FinishWorkPlan(context.Context, string, string, map[string]any) (json.RawMessage, error)
}

type workProfileOption struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

func workProfilesForEvent(ctx context.Context, db HealthStore, config *AuthConfig, eventID string) []workProfileOption {
	options := []workProfileOption{}
	lookup, ok := db.(scopedResourceLookup)
	if !ok || len(config.WorkOrderProfiles) == 0 {
		return options
	}
	assetID, err := lookup.AssetIDForResource(ctx, "event", eventID)
	if err != nil {
		return options
	}
	for name, profile := range config.WorkOrderProfiles {
		if profile.AssetID == assetID {
			options = append(options, workProfileOption{Name: name, Action: profile.Action})
		}
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Name < options[j].Name })
	return options
}

func prepareWorkPlan(ctx context.Context, db plannedWorkStore, config *AuthConfig, event string, input storepkg.WorkOrderInput, probe bool) (storepkg.WorkOrderPlan, error) {
	target, err := db.WorkOrderTarget(ctx, event, input)
	if err != nil {
		return storepkg.WorkOrderPlan{}, err
	}
	plan := storepkg.WorkOrderPlan{Action: input.Action, AssetID: target.ID, TargetRevision: target.Revision, TemplateID: input.TemplateID, Profile: input.Profile}
	switch input.Action {
	case storepkg.WorkCollect:
		plan.Description = "对事件资产执行一次只读采样"
	case storepkg.WorkInspect:
		plan.Description = "使用模板 " + input.TemplateID + " 执行一次巡检并保存报告"
	case storepkg.WorkVerifyRestore, storepkg.WorkAnalyze:
		profile, ok := config.WorkOrderProfiles[input.Profile]
		if !ok || profile.AssetID != target.ID || profile.Action != input.Action {
			return plan, errJobProfile
		}
		cfg, err := profile.connectionConfig()
		if err != nil || cfg.User == target.Username {
			return plan, errJobProfile
		}
		sameHost := strings.EqualFold(cfg.Host, target.Host) && int(cfg.Port) == target.Port
		if input.Action == storepkg.WorkAnalyze {
			if !sameHost || cfg.Database != target.Database {
				return plan, errJobProfile
			}
			plan.Description = "使用独立执行账号分析预配置表 " + profile.Schema + "." + profile.Relation
		} else {
			if sameHost || cfg.Database == target.Database {
				return plan, errJobProfile
			}
			plan.Description = "在隔离恢复库 " + cfg.Database + " 只读核对恢复点前后的数据标记"
		}
		if probe {
			if _, err := runWorkProfile(ctx, profile, true); err != nil {
				return plan, errJobProfile
			}
		}
		plan.ProfileRevision = profile.revision(config.MasterKey)
	}
	return plan, nil
}

func runWorkProfile(ctx context.Context, profile WorkOrderProfile, preflight bool) (map[string]any, error) {
	cfg, err := profile.connectionConfig()
	if err != nil {
		return nil, err
	}
	if preflight {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer conn.Close(closeCtx)

	var version int
	var privileged, recovery bool
	err = conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,pg_is_in_recovery(),
		rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls
		FROM pg_roles WHERE rolname=current_user`).Scan(&version, &recovery, &privileged)
	if err != nil || privileged || !collector.PostgresMajorSupported(version/10000) {
		if err != nil {
			return nil, err
		}
		return nil, errJobProfile
	}
	relation := pgx.Identifier{profile.Schema, profile.Relation}.Sanitize()
	var table bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_class WHERE oid=to_regclass($1)
		AND relkind IN ('r','p') AND relpersistence='p')`, relation).Scan(&table); err != nil || !table {
		if err != nil {
			return nil, err
		}
		return nil, errJobProfile
	}
	if profile.Action == storepkg.WorkAnalyze {
		var allowed bool
		var standalone bool
		if err := conn.QueryRow(ctx, workPlainRelation, relation).Scan(&standalone); err != nil || !standalone {
			if err != nil {
				return nil, err
			}
			return nil, errJobProfile
		}
		if version >= 170000 {
			err = conn.QueryRow(ctx, "SELECT has_table_privilege(to_regclass($1),'MAINTAIN')", relation).Scan(&allowed)
		} else {
			err = conn.QueryRow(ctx, "SELECT pg_has_role(current_user,relowner,'USAGE') FROM pg_class WHERE oid=to_regclass($1)", relation).Scan(&allowed)
		}
		if err != nil || !allowed || recovery {
			if err != nil {
				return nil, err
			}
			return nil, errJobProfile
		}
		if preflight {
			return nil, nil
		}
		// A relation rename between checks and execution is serialized by the table lock.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "LOCK TABLE ONLY "+relation+" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
			return nil, err
		}
		err = tx.QueryRow(ctx, workPlainRelation, relation).Scan(&standalone)
		if err != nil || !standalone {
			if err != nil {
				return nil, err
			}
			return nil, errJobProfile
		}
		if _, err := tx.Exec(ctx, "ANALYZE "+relation); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"schema": profile.Schema, "relation": profile.Relation, "databaseVersion": version}, nil
	}
	column := pgx.Identifier{profile.MarkerColumn}.Sanitize()
	var readable bool
	err = conn.QueryRow(ctx, `SELECT coalesce((SELECT has_column_privilege(c.oid,a.attnum,'SELECT')
		FROM pg_class c JOIN pg_attribute a ON a.attrelid=c.oid WHERE c.oid=to_regclass($1)
		AND a.attname=$2 AND NOT a.attisdropped),false)`, relation, profile.MarkerColumn).Scan(&readable)
	if err != nil || !readable {
		if err != nil {
			return nil, err
		}
		return nil, errJobProfile
	}
	if preflight {
		return nil, nil
	}
	var required, excluded bool
	statement := fmt.Sprintf("SELECT EXISTS(SELECT FROM %s WHERE %s::text=$1), EXISTS(SELECT FROM %s WHERE %s::text=$2)", relation, column, relation, column)
	if err := conn.QueryRow(ctx, statement, profile.RequiredMarker, profile.ExcludedMarker).Scan(&required, &excluded); err != nil {
		return nil, err
	}
	result := map[string]any{"expectedPresent": required, "laterMarkerAbsent": !excluded, "restoredDatabase": cfg.Database,
		"schema": profile.Schema, "relation": profile.Relation, "databaseVersion": version}
	if !required || excluded {
		return result, errJobResult
	}
	return result, nil
}

func handleWorkOrder(w http.ResponseWriter, r *http.Request, store HealthStore, config *AuthConfig, event string, preview bool) {
	db, ok := store.(plannedWorkStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Controlled work plans unavailable"})
		return
	}
	var input storepkg.WorkOrderInput
	if readJSONLimit(w, r, &input, 8192) != nil {
		writeWorkOrderError(w, storepkg.ErrInvalidWorkOrder)
		return
	}
	input, err := storepkg.NormalizeWorkOrder(input)
	if err != nil || !preview && input.Reason == "" {
		writeWorkOrderError(w, storepkg.ErrInvalidWorkOrder)
		return
	}
	if !preview {
		existing, err := db.ExistingWorkOrder(r.Context(), event, input)
		if err != nil {
			writeWorkOrderError(w, err)
			return
		}
		if existing != nil {
			respond(w, http.StatusOK, map[string]any{"order": existing, "replayed": true})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	plan, err := prepareWorkPlan(ctx, db, config, event, input, preview)
	if err != nil {
		writeWorkOrderError(w, err)
		return
	}
	if preview {
		plan.TargetRevision, plan.ProfileRevision = "", ""
		respond(w, http.StatusOK, map[string]any{"plan": plan, "expiresMinutes": input.ExpiresMinutes, "timeoutSeconds": input.TimeoutSeconds})
		return
	}
	order, err := db.CreatePlannedWorkOrder(ctx, event, input, &plan)
	if err != nil {
		writeWorkOrderError(w, err)
		return
	}
	respond(w, http.StatusCreated, map[string]any{"order": order, "preview": plan.Description})
}

func writeWorkOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storepkg.ErrInvalidWorkOrder):
		respond(w, http.StatusBadRequest, map[string]string{"error": "作业参数、巡检模板或理由无效"})
	case errors.Is(err, errJobProfile):
		respond(w, http.StatusBadRequest, map[string]string{"error": "执行配置未启用、与事件目标不匹配或预检失败"})
	case errors.Is(err, storepkg.ErrEventNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
	case errors.Is(err, storepkg.ErrWorkOrderConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "事件已关闭、作业达到并发上限或幂等键参数冲突"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order storage unavailable"})
	}
}

func executeWorkOrder(w http.ResponseWriter, r *http.Request, store HealthStore, config *AuthConfig, id string) {
	db, ok := store.(plannedWorkStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Controlled work plans unavailable"})
		return
	}
	task, err := db.StartWorkPlan(r.Context(), id)
	if errors.Is(err, storepkg.ErrWorkOrderConflict) {
		respond(w, http.StatusConflict, map[string]string{"error": "Approval required, target changed, event resolved, or work order expired"})
		return
	}
	if err != nil {
		writeWorkOrderError(w, err)
		return
	}
	execution, cancel := context.WithDeadline(r.Context(), task.Deadline)
	defer cancel()
	details, result, runErr := runWorkOrder(execution, store, config, task)
	state := "completed"
	if runErr != nil || execution.Err() != nil {
		state = "failed"
		var remote *pgconn.PgError
		switch {
		case errors.Is(execution.Err(), context.DeadlineExceeded), errors.As(runErr, &remote) && remote.Code == "57014":
			details["errorCode"] = "execution_timeout"
		case errors.Is(runErr, errJobResult):
			details["errorCode"] = "restore_marker_mismatch"
		case errors.Is(runErr, errJobProfile):
			details["errorCode"] = "execution_profile_unavailable"
		default:
			details["errorCode"] = "execution_failed"
		}
	}
	persist, persistCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer persistCancel()
	order, err := db.FinishWorkPlan(persist, id, state, details)
	if err != nil {
		slog.Error("work order result was not recorded", "workOrderId", id)
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order execution audit unavailable"})
		return
	}
	respond(w, http.StatusOK, map[string]any{"order": order, "result": result})
}

func runWorkOrder(ctx context.Context, store HealthStore, config *AuthConfig, task storepkg.WorkOrderExecution) (map[string]any, any, error) {
	details := map[string]any{}
	switch task.Plan.Action {
	case storepkg.WorkCollect:
		checker, ok := store.(AssetChecker)
		if !ok {
			return details, nil, errJobProfile
		}
		result, err := checker.CheckAsset(ctx, task.AssetID, config.MasterKey)
		details["sampleId"], details["sampleStatus"] = result.SampleID, result.Status
		if err == nil && result.Status != "ok" {
			err = errors.New("collection failed")
		}
		return details, result, err
	case storepkg.WorkInspect:
		runner, ok := store.(InspectionRunner)
		if !ok {
			return details, nil, errJobProfile
		}
		value, err := runner.RunInspection(ctx, task.AssetID, task.Plan.TemplateID, config.MasterKey, "")
		if err != nil {
			return details, nil, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return details, nil, err
		}
		var report struct {
			Run struct{ ID, SampleID, Status string } `json:"run"`
		}
		if err := json.Unmarshal(raw, &report); err != nil {
			return details, nil, err
		}
		details["inspectionRunId"], details["sampleId"], details["inspectionStatus"] = report.Run.ID, report.Run.SampleID, report.Run.Status
		if report.Run.Status != "completed" {
			return details, details, errors.New("inspection did not complete")
		}
		return details, details, nil
	case storepkg.WorkVerifyRestore, storepkg.WorkAnalyze:
		profile, ok := config.WorkOrderProfiles[task.Plan.Profile]
		if !ok || profile.AssetID != task.AssetID || profile.Action != task.Plan.Action ||
			profile.revision(config.MasterKey) != task.Plan.ProfileRevision {
			return details, nil, errJobProfile
		}
		value, err := runWorkProfile(ctx, profile, false)
		if value == nil {
			value = details
		}
		value["profile"] = task.Plan.Profile
		return value, value, err
	default:
		return details, nil, errJobProfile
	}
}
