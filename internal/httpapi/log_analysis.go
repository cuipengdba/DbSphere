package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/loganalysis"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const logAnalysisPath = "/api/log-analysis/"

func serveLogAnalysis(w http.ResponseWriter, r *http.Request, db HealthStore, role, actor string) {
	w.Header().Set("Cache-Control", "no-store")
	if role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "日志分析需要管理员权限"})
		return
	}
	pg, ok := db.(*storepkg.Postgres)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "日志分析存储不可用"})
		return
	}
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, logAnalysisPath)
	sections := strings.Split(path, "/")
	var value any
	var err error
	status := http.StatusOK
	switch {
	case path == "directory" && r.Method == http.MethodGet:
		value, err = pg.LogDirectory(ctx)
	case path == "tasks" && r.Method == http.MethodGet:
		query := r.URL.Query()
		filter := storepkg.LogTaskQuery{State: query.Get("state"), BeforeID: query.Get("beforeId")}
		if before := query.Get("before"); before != "" || filter.BeforeID != "" {
			at, parseErr := time.Parse(time.RFC3339Nano, before)
			if parseErr != nil || !opsUUID.MatchString(filter.BeforeID) {
				err = storepkg.ErrLogInput
				break
			}
			filter.Before = &at
		}
		value, err = pg.LogTasks(ctx, filter)
	case path == "archive" && r.Method == http.MethodGet:
		value, err = pg.LogArchiveOverview(ctx)
	case len(sections) == 3 && sections[0] == "archive" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "preview" && r.Method == http.MethodGet:
		days, parseErr := strconv.Atoi(r.URL.Query().Get("keepDays"))
		if parseErr != nil {
			err = storepkg.ErrLogInput
			break
		}
		value, err = pg.PreviewLogArchivePolicy(ctx, sections[1], days)
	case len(sections) == 3 && sections[0] == "archive" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "files" && r.Method == http.MethodGet:
		query := r.URL.Query()
		var offset int
		if raw := query.Get("offset"); raw != "" {
			offset, err = strconv.Atoi(raw)
			if err != nil {
				err = storepkg.ErrLogInput
				break
			}
		}
		value, err = pg.LogArchiveFiles(ctx, sections[1], query.Get("state"), offset)
	case len(sections) == 3 && sections[0] == "archive" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "policy" && r.Method == http.MethodPost:
		var input struct{ KeepDays int }
		if readJSONLimit(w, r, &input, 1024) != nil {
			err = storepkg.ErrLogInput
			break
		}
		var audit string
		audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
		if err == nil {
			value, err = pg.SetLogArchivePolicy(ctx, sections[1], input.KeepDays)
			if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(status, err)); auditErr != nil {
				err = auditErr
			}
		}
	case path == "projects" && r.Method == http.MethodPost:
		var input storepkg.LogProjectInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrLogInput
			break
		}
		status = http.StatusCreated
		value, err = pg.CreateLogProject(ctx, input)
	case path == "instances" && r.Method == http.MethodPost:
		var input storepkg.LogInstanceInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrLogInput
			break
		}
		status = http.StatusCreated
		value, err = pg.CreateLogInstance(ctx, input)
	case path == "batches" && r.Method == http.MethodGet:
		value, err = pg.LogBatches(ctx, r.URL.Query().Get("instance"))
	case path == "compare" && r.Method == http.MethodGet:
		base, current := r.URL.Query().Get("baseline"), r.URL.Query().Get("current")
		if !opsUUID.MatchString(base) || !opsUUID.MatchString(current) {
			err = storepkg.ErrLogInput
			break
		}
		value, err = pg.LogComparison(ctx, base, current)
	case path == "health" && r.Method == http.MethodGet:
		value, err = pg.LogProjectHealth(ctx, "")
	case len(sections) == 3 && sections[0] == "projects" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "health" && r.Method == http.MethodGet:
		value, err = pg.LogProjectHealth(ctx, sections[1])
	case path == "batches" && r.Method == http.MethodPost:
		var input storepkg.LogBatchInput
		if readJSONLimit(w, r, &input, 4096) != nil {
			err = storepkg.ErrLogInput
			break
		}
		status = http.StatusCreated
		value, err = pg.CreateLogBatch(ctx, input)
	case path == "preview" && r.Method == http.MethodPost:
		var sample []byte
		sample, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
		if err == nil {
			preview := loganalysis.Detect(sample)
			value = map[string]string{"format": preview.Format, "reason": preview.Reason}
		}
	case len(sections) >= 2 && sections[0] == "batches" && opsUUID.MatchString(sections[1]):
		switch {
		case len(sections) == 3 && sections[2] == "sources" && r.Method == http.MethodGet:
			value, err = pg.LogStreams(ctx, sections[1])
		case len(sections) == 3 && sections[2] == "sources" && r.Method == http.MethodPost:
			var input storepkg.LogStreamInput
			if readJSONLimit(w, r, &input, 4096) != nil {
				err = storepkg.ErrLogInput
				break
			}
			status = http.StatusCreated
			value, err = pg.CreateLogStream(ctx, sections[1], input)
		case len(sections) == 2 && r.Method == http.MethodGet:
			value, err = pg.LogBatch(ctx, sections[1])
		case len(sections) == 3 && sections[2] == "files" && r.Method == http.MethodPost:
			var input storepkg.LogFileInput
			if readJSONLimit(w, r, &input, 4096) != nil {
				err = storepkg.ErrLogInput
				break
			}
			status = http.StatusCreated
			value, err = pg.CreateLogFile(ctx, sections[1], input)
		case len(sections) == 3 && sections[2] == "records" && r.Method == http.MethodGet:
			var filters storepkg.LogRecordQuery
			filters, err = parseLogRecordFilters(r)
			if err == nil {
				value, err = pg.LogRecords(ctx, sections[1], filters)
			}
		case len(sections) == 3 && sections[2] == "report" && r.Method == http.MethodGet:
			var filters storepkg.LogRecordQuery
			filters, err = parseLogRecordFilters(r)
			if err == nil {
				value, err = pg.LogReport(ctx, sections[1], filters)
			}
		case len(sections) == 3 && sections[2] == "rebuild" && r.Method == http.MethodPost:
			value, err = pg.RebuildLogBatchRollup(ctx, sections[1])
		default:
			err = storepkg.ErrLogNotFound
		}
	case len(sections) == 3 && sections[0] == "sources" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "revoke" && r.Method == http.MethodPost:
		err = pg.RevokeLogStream(ctx, sections[1])
		value = map[string]bool{"revoked": err == nil}
	case len(sections) == 3 && sections[0] == "files" && opsUUID.MatchString(sections[1]):
		switch {
		case sections[2] == "status" && r.Method == http.MethodGet:
			value, err = pg.LogUploadStatus(ctx, sections[1])
		case sections[2] == "cancel" && r.Method == http.MethodPost:
			var audit string
			audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
			if err == nil {
				value, err = pg.CancelLogUpload(ctx, sections[1])
				if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(status, err)); auditErr != nil {
					err = auditErr
				}
			}
		case sections[2] == "verify" && r.Method == http.MethodPost:
			value, err = pg.VerifyLogFile(ctx, sections[1])
		case sections[2] == "blocks" && r.Method == http.MethodPut:
			var part int
			part, err = strconv.Atoi(r.URL.Query().Get("part"))
			if err != nil {
				err = storepkg.ErrLogInput
				break
			}
			var raw []byte
			raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, storepkg.LogArchiveBlockBytes))
			if err == nil {
				value, err = pg.AppendLogBlock(ctx, sections[1], part, raw)
			}
		case sections[2] == "queue" && r.Method == http.MethodPost:
			var override *storepkg.LogParseInput
			if r.ContentLength != 0 {
				if readJSONLimit(w, r, &override, 4096) != nil {
					err = storepkg.ErrLogInput
					break
				}
			}
			status = http.StatusAccepted
			value, err = pg.QueueLogFile(ctx, sections[1], override)
		default:
			err = storepkg.ErrLogNotFound
		}
	case len(sections) == 3 && sections[0] == "records" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "investigate" && r.Method == http.MethodPost:
		var input struct{ EventID string }
		if readJSONLimit(w, r, &input, 2048) != nil || input.EventID != "" && !opsUUID.MatchString(input.EventID) {
			err = storepkg.ErrLogInput
			break
		}
		value, err = pg.StartLogInvestigation(ctx, sections[1], input.EventID)
	case len(sections) == 3 && sections[0] == "events" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "candidates" && r.Method == http.MethodGet:
		value, err = pg.LogEventCandidates(ctx, sections[1])
	case len(sections) == 3 && sections[0] == "events" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "retests" && r.Method == http.MethodGet:
		value, err = pg.EventLogRetestCases(ctx, sections[1])
	case len(sections) == 3 && sections[0] == "events" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "retests" && r.Method == http.MethodPost:
		var input struct {
			CaseID string
			storepkg.LogRetestInput
		}
		if readJSONLimit(w, r, &input, 4096) != nil || !opsUUID.MatchString(input.CaseID) ||
			!opsUUID.MatchString(input.BaselineBatchID) || !opsUUID.MatchString(input.CurrentBatchID) ||
			input.BaselinePlanID != "" && !opsUUID.MatchString(input.BaselinePlanID) ||
			input.CurrentPlanID != "" && !opsUUID.MatchString(input.CurrentPlanID) {
			err = storepkg.ErrLogInput
			break
		}
		var audit string
		audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
		if err == nil {
			input.Actor = actor
			value, err = pg.SaveEventLogRetest(ctx, sections[1], input.CaseID, input.LogRetestInput)
			if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(status, err)); auditErr != nil {
				err = auditErr
			}
		}
	case len(sections) == 3 && sections[0] == "events" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "records" && r.Method == http.MethodPost:
		var input struct{ RecordID string }
		if readJSONLimit(w, r, &input, 2048) != nil || !opsUUID.MatchString(input.RecordID) {
			err = storepkg.ErrLogInput
			break
		}
		var audit string
		audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
		if err == nil {
			value, err = pg.AdoptEventLogRecord(ctx, sections[1], input.RecordID)
			if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(status, err)); auditErr != nil {
				err = auditErr
			}
		}
	case len(sections) == 2 && sections[0] == "investigations" && opsUUID.MatchString(sections[1]) && r.Method == http.MethodGet:
		value, err = pg.LogInvestigation(ctx, sections[1])
	case len(sections) == 3 && sections[0] == "investigations" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "retests" && r.Method == http.MethodPost:
		var input storepkg.LogRetestInput
		if readJSONLimit(w, r, &input, 2048) != nil || !opsUUID.MatchString(input.BaselineBatchID) || !opsUUID.MatchString(input.CurrentBatchID) {
			err = storepkg.ErrLogInput
			break
		}
		var audit string
		audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
		if err == nil {
			value, err = pg.SaveLogRetest(ctx, sections[1], input)
			if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(status, err)); auditErr != nil {
				err = auditErr
			}
		}
	case len(sections) == 3 && sections[0] == "investigations" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "records" && r.Method == http.MethodPost:
		var input struct{ RecordID string }
		if readJSONLimit(w, r, &input, 2048) != nil || !opsUUID.MatchString(input.RecordID) {
			err = storepkg.ErrLogInput
			break
		}
		value, err = pg.LinkLogInvestigation(ctx, sections[1], input.RecordID)
	case len(sections) == 3 && sections[0] == "records" && opsUUID.MatchString(sections[1]) &&
		sections[2] == "original" && r.Method == http.MethodPost:
		var audit string
		audit, err = pg.StartRoleAudit(ctx, actor, r.Method, r.URL.Path)
		if err == nil {
			value, err = pg.LogOriginal(ctx, sections[1])
			if auditErr := pg.FinishAdminAudit(ctx, audit, logAnalysisStatus(http.StatusOK, err)); auditErr != nil {
				err = auditErr
			}
		}
	default:
		err = storepkg.ErrLogNotFound
	}
	status = logAnalysisStatus(status, err)
	if err != nil {
		message := "日志分析暂不可用"
		switch status {
		case http.StatusBadRequest:
			message = "请检查日志分析输入"
		case http.StatusNotFound:
			message = "找不到该日志分析资源"
		case http.StatusConflict:
			message = "名称已存在或文件状态已改变"
		case http.StatusRequestEntityTooLarge:
			message = "上传内容超过单块上限"
		case http.StatusTooManyRequests:
			message = "当前组织的上传或解析队列已满，请等候已有任务完成"
		}
		value = map[string]string{"error": message}
	}
	respond(w, status, value)
}

func logAnalysisStatus(success int, err error) int {
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return success
	case errors.Is(err, storepkg.ErrLogInput):
		return http.StatusBadRequest
	case errors.Is(err, storepkg.ErrLogNotFound):
		return http.StatusNotFound
	case errors.Is(err, storepkg.ErrLogConflict):
		return http.StatusConflict
	case errors.Is(err, storepkg.ErrLogQuota):
		return http.StatusTooManyRequests
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusServiceUnavailable
	}
}

func parseLogRecordFilters(r *http.Request) (storepkg.LogRecordQuery, error) {
	query := r.URL.Query()
	filters := storepkg.LogRecordQuery{Kind: query.Get("kind"), Application: query.Get("application"), Fingerprint: query.Get("fingerprint")}
	if raw := query.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			return filters, storepkg.ErrLogInput
		}
		filters.Offset = offset
	}
	for key, value := range map[string]**time.Time{"from": &filters.From, "to": &filters.To} {
		if raw := query.Get(key); raw != "" {
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return filters, storepkg.ErrLogInput
			}
			*value = &at
		}
	}
	if filters.From != nil && filters.To != nil && !filters.From.Before(*filters.To) {
		return filters, storepkg.ErrLogInput
	}
	return filters, nil
}
