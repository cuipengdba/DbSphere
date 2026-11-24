package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogAnalysisUploadParseArchiveAndScope(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	idOf := func(value any, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ ID string }
		raw, _ := json.Marshal(value)
		if err := json.Unmarshal(raw, &result); err != nil || result.ID == "" {
			t.Fatalf("invalid resource: %s (%v)", raw, err)
		}
		return result.ID
	}
	project := idOf(pg.CreateLogProject(ctx, LogProjectInput{Name: "Log analysis " + time.Now().Format(time.RFC3339Nano)}))
	defer pg.pool.Exec(ctx, "DELETE FROM log_projects WHERE id=$1", project)
	var asset string
	err = pg.pool.QueryRow(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at,organization_id)
		SELECT gen_random_uuid(),'Log investigation fixture','127.0.0.1',5432,'orders','fixture','','disable','linux',now(),organization_id
		FROM log_projects WHERE id=$1 RETURNING id::text`, project).Scan(&asset)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", asset)
	instance := idOf(pg.CreateLogInstance(ctx, LogInstanceInput{ProjectID: project, Name: "Orders", Environment: "production", Engine: "postgres", Version: "18", AssetID: asset}))
	batch := idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Name: "Rotation 1", Coverage: "slow"}))
	line := "2026-11-24 19:10:00.123 UTC [12] app=orders-api LOG: duration: 2100.5 ms statement: select a from orders where code='fixture-private'\n"
	payload := []byte(strings.Repeat("x", LogArchiveBlockBytes-18) + "\n" + line)
	abandoned := idOf(pg.CreateLogFile(ctx, batch, LogFileInput{Name: "abandoned.log", Size: int64(len(payload))}))
	if _, err := pg.AppendLogBlock(ctx, abandoned, 0, payload[:LogArchiveBlockBytes]); err != nil {
		t.Fatal(err)
	}
	taskView, err := pg.LogTasks(ctx, LogTaskQuery{State: "uploading"})
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskView.(map[string]any)["tasks"].([]json.RawMessage)
	if len(tasks) == 0 || !strings.Contains(string(tasks[0]), abandoned) {
		t.Fatalf("incomplete upload missing from task queue: %s", tasks)
	}
	otherOrg := WithOrganization(ctx, "00000000-0000-4000-8000-000000000002")
	if _, err := pg.CancelLogUpload(otherOrg, abandoned); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("out-of-scope cancellation: %v", err)
	}
	if _, err := pg.CancelLogUpload(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.LogUploadStatus(ctx, abandoned); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("cancelled upload still exists: %v", err)
	}
	var abandonedBlocks int
	if err := pg.pool.QueryRow(ctx, "SELECT count(*) FROM log_archive_blocks WHERE file_id=$1", abandoned).Scan(&abandonedBlocks); err != nil || abandonedBlocks != 0 {
		t.Fatalf("cancelled upload retained %d blocks: %v", abandonedBlocks, err)
	}
	upload := func(batch string) string {
		t.Helper()
		file := idOf(pg.CreateLogFile(ctx, batch, LogFileInput{Name: "server.log", Node: "node01", Format: "text", Zone: "UTC", Size: int64(len(payload))}))
		for part, offset := 0, 0; offset < len(payload); part, offset = part+1, offset+LogArchiveBlockBytes {
			block := payload[offset:min(offset+LogArchiveBlockBytes, len(payload))]
			if _, err := pg.AppendLogBlock(ctx, file, part, block); err != nil {
				t.Fatal(err)
			}
			if _, err := pg.AppendLogBlock(ctx, file, part, block); err != nil {
				t.Fatal("idempotent retry:", err)
			}
			if part == 0 {
				value, err := pg.LogUploadStatus(ctx, file)
				if err != nil || !strings.Contains(string(value.(json.RawMessage)), `"parts": 1`) {
					t.Fatalf("upload continuation status: %v %v", value, err)
				}
			}
		}
		return file
	}
	file := upload(batch)
	if value, err := pg.QueueLogFile(ctx, file, nil); err != nil || value.(map[string]any)["state"] != "queued" {
		t.Fatalf("queue: %v %v", value, err)
	}
	if _, err := pg.CancelLogUpload(ctx, file); !errors.Is(err, ErrLogConflict) {
		t.Fatalf("submitted file was cancellable: %v", err)
	}
	if processed, err := pg.ProcessNextLogFile(ctx); err != nil || !processed {
		t.Fatalf("worker: %v %v", processed, err)
	}
	list, err := pg.LogRecords(ctx, batch, LogRecordQuery{Kind: "statement"})
	if err != nil {
		t.Fatal(err)
	}
	rows := list.(map[string]any)["records"].([]json.RawMessage)
	if len(rows) != 1 || strings.Contains(string(rows[0]), "fixture-private") || !strings.Contains(string(rows[0]), "2100.5") {
		t.Fatalf("wrong sanitized record: %s", rows)
	}
	id := idOf(rows[0], nil)
	raw, err := pg.LogOriginal(ctx, id)
	if err != nil || !strings.Contains(raw.(map[string]any)["text"].(string), line) {
		t.Fatalf("original crossed block boundary: %v", err)
	}
	report, err := pg.LogReport(ctx, batch, LogRecordQuery{})
	if err != nil {
		t.Fatal(err)
	}
	reportJSON, _ := json.Marshal(report)
	if !strings.Contains(string(reportJSON), "orders-api") || !strings.Contains(string(reportJSON), "\"p95\": 2100.5") &&
		!strings.Contains(string(reportJSON), "\"p95\":2100.5") {
		t.Fatalf("report did not reflect the timed SQL: %s", reportJSON)
	}
	outOfScope := WithOrganization(ctx, "00000000-0000-4000-8000-000000000002")
	if _, err := pg.LogOriginal(outOfScope, id); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("cross-organization log origin exposed: %v", err)
	}
	second := idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Name: "Repeated file"}))
	duplicate := upload(second)
	value, err := pg.QueueLogFile(ctx, duplicate, nil)
	if err != nil || value.(map[string]any)["duplicateBatch"] != batch {
		t.Fatalf("duplicate did not resolve to original batch: %v %v", value, err)
	}
	comparison, err := pg.LogComparison(ctx, batch, second)
	if err != nil {
		t.Fatal(err)
	}
	changes := comparison.(map[string]any)["rows"].([]*logCompareRow)
	if len(changes) != 1 || changes[0].Status != "unobserved" || changes[0].Baseline.EvidenceID != id {
		t.Fatalf("comparison lost original evidence: %+v", changes)
	}
	health, err := pg.LogProjectHealth(ctx, project)
	if err != nil || len(health.(map[string]any)["history"].([]logBatchSummary)) != 2 {
		t.Fatalf("project history: %v %v", health, err)
	}
	caseID := idOf(pg.StartLogInvestigation(ctx, id, ""))
	if retry := idOf(pg.StartLogInvestigation(ctx, id, "")); retry != caseID {
		t.Fatalf("retry produced a different investigation: %s", retry)
	}
	retest := []byte(strings.Replace(line, "2100.5", "450.0", 1))
	retestFile := idOf(pg.CreateLogFile(ctx, second, LogFileInput{Name: "retest.log", Format: "text", Size: int64(len(retest))}))
	if _, err := pg.AppendLogBlock(ctx, retestFile, 0, retest); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.QueueLogFile(ctx, retestFile, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := pg.ProcessNextLogFile(ctx); err != nil || !done {
		t.Fatalf("parse retest: %v %v", done, err)
	}
	retestRows, err := pg.LogRecords(ctx, second, LogRecordQuery{})
	if err != nil {
		t.Fatal(err)
	}
	retestID := idOf(retestRows.(map[string]any)["records"].([]json.RawMessage)[0], nil)
	linked, err := pg.LinkLogInvestigation(ctx, caseID, retestID)
	if err != nil {
		t.Fatal(err)
	}
	investigation := linked.(map[string]any)
	event := investigation["eventId"].(*string)
	if event == nil || investigation["diagnosis"].(LogDiagnosis).Priority != "P2" ||
		len(investigation["records"].([]json.RawMessage)) != 2 {
		t.Fatalf("missing event or evidence: %+v", investigation)
	}
	reportText, evidence, err := pg.EventAIEvidence(ctx, *event)
	if err != nil || len(evidence) < 2 || !strings.Contains(reportText, id) || !strings.Contains(reportText, retestID) ||
		strings.Contains(reportText, "fixture-private") {
		t.Fatalf("event failed to preserve sanitized log evidence: %s %v", reportText, err)
	}
	if _, err := pg.LogInvestigation(outOfScope, caseID); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("cross-organization investigation exposed: %v", err)
	}
	if _, err := pg.LogEventCandidates(outOfScope, *event); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("cross-organization event logs exposed: %v", err)
	}
	if existing := idOf(pg.AdoptEventLogRecord(ctx, *event, retestID)); existing != caseID {
		t.Fatalf("re-adopting an existing retest changed its investigation: %s", existing)
	}
	discovered := idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Name: "Event search fixture"}))
	extra := []byte(strings.Replace(line, "select a from orders", "select a from orders_archive", 1) +
		strings.ReplaceAll(strings.Replace(line, "select a from orders", "select a from long_ago", 1), "19:10:00.123", "15:10:00.123"))
	newLog := idOf(pg.CreateLogFile(ctx, discovered, LogFileInput{Name: "incident.log", Size: int64(len(extra)), Format: "text"}))
	if _, err := pg.AppendLogBlock(ctx, newLog, 0, extra); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.QueueLogFile(ctx, newLog, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := pg.ProcessNextLogFile(ctx); err != nil || !done {
		t.Fatalf("prepare incident candidates: %v %v", done, err)
	}
	candidates, err := pg.LogEventCandidates(ctx, *event)
	if err != nil {
		t.Fatal(err)
	}
	var candidate string
	for _, row := range candidates.(map[string]any)["records"].([]json.RawMessage) {
		if strings.Contains(string(row), "long_ago") {
			t.Fatal("out-of-window record leaked into incident candidates")
		}
		if strings.Contains(string(row), "orders_archive") {
			candidate = idOf(row, nil)
		}
	}
	if candidate == "" {
		t.Fatal("same-asset, same-window candidate was missing")
	}
	adopted := idOf(pg.AdoptEventLogRecord(ctx, *event, candidate))
	if again := idOf(pg.AdoptEventLogRecord(ctx, *event, candidate)); again != adopted {
		t.Fatalf("adoption was not idempotent: %s", again)
	}
	updatedReport, _, err := pg.EventAIEvidence(ctx, *event)
	if err != nil || !strings.Contains(updatedReport, candidate) {
		t.Fatalf("adopted record missing from original event: %v", err)
	}
	retestInput := LogRetestInput{BaselineBatchID: batch, CurrentBatchID: second, TargetP95MS: 600}
	decision, err := pg.SaveLogRetest(ctx, caseID, retestInput)
	if err != nil || decision.(map[string]any)["status"] != "insufficient" {
		t.Fatalf("small-sample retest should retain coverage reason: %v %v", decision, err)
	}
	measurement := idOf(decision, nil)
	if repeated := idOf(pg.SaveLogRetest(ctx, caseID, retestInput)); repeated != measurement {
		t.Fatalf("unchanged evidence produced a different retest: %s", repeated)
	}
	var results int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE event_id=$1
		AND kind='log_retest' AND payload_json->>'retestId'=$2`, *event, measurement).Scan(&results); err != nil || results != 1 {
		t.Fatalf("retest must appear once on the original event: %d %v", results, err)
	}
	hotspot := idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Name: "Application latency", Coverage: "sampled"}))
	var sample strings.Builder
	for index, message := range []string{
		"alpha LOG: duration: 9 ms statement: select a from hotspot where code='a'",
		"alpha LOG: duration: 50 ms statement: select a from hotspot where code='b'",
		"alpha LOG: duration: 1200 ms statement: select a from hotspot where code='c'",
		"alpha LOG: statement: select a from hotspot where code='d'",
		"beta LOG: duration: 30000 ms statement: select a from hotspot where code='e'",
	} {
		fmt.Fprintf(&sample, "2026-11-24 19:20:%02d UTC [13] app=%s\n", index, message)
	}
	trace := idOf(pg.CreateLogFile(ctx, hotspot, LogFileInput{Name: "latency.log", Format: "text", Size: int64(sample.Len())}))
	if _, err := pg.AppendLogBlock(ctx, trace, 0, []byte(sample.String())); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.QueueLogFile(ctx, trace, nil); err != nil {
		t.Fatal(err)
	}
	if done, err := pg.ProcessNextLogFile(ctx); err != nil || !done {
		t.Fatalf("prepare latency samples: %v %v", done, err)
	}
	full, err := pg.LogReport(ctx, hotspot, LogRecordQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var target struct{ Fingerprint string }
	if err := json.Unmarshal(full.(map[string]any)["slowSQL"].([]json.RawMessage)[0], &target); err != nil || target.Fingerprint == "" {
		t.Fatalf("fingerprint for hotspot: %v", err)
	}
	filtered, err := pg.LogReport(ctx, hotspot, LogRecordQuery{Application: "alpha", Fingerprint: target.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	var latency struct {
		Statements, Timed int
		LatencyBuckets    []int
	}
	rel := filtered.(map[string]any)
	if err := json.Unmarshal(rel["summary"].(json.RawMessage), &latency); err != nil || latency.Statements != 4 || latency.Timed != 3 ||
		!slices.Equal(latency.LatencyBuckets, []int{1, 0, 1, 0, 0, 1, 0}) || len(rel["examples"].([]json.RawMessage)) != 3 {
		t.Fatalf("filtered latency profile: %+v %v", latency, err)
	}
	if result, err := pg.VerifyLogFile(ctx, file); err != nil || result.(map[string]any)["rawSize"] != int64(len(payload)) {
		t.Fatalf("archive validation: %v %v", result, err)
	}
	if _, err := pg.RebuildLogBatchRollup(ctx, batch); err != nil {
		t.Fatal(err)
	}
	indexed, err := pg.LogComparison(ctx, batch, second)
	if err != nil {
		t.Fatal(err)
	}
	pairs := indexed.(map[string]any)["rows"].([]*logCompareRow)
	if len(pairs) != 1 || pairs[0].Current == nil || pairs[0].Current.EvidenceID != retestID {
		t.Fatal("indexed comparison did not see the newly parsed retest")
	}
	if _, err := pg.RebuildLogBatchRollup(ctx, second); err != nil {
		t.Fatal(err)
	}
	reindexed, err := pg.LogComparison(ctx, batch, second)
	expected, _ := json.Marshal(indexed)
	actual, _ := json.Marshal(reindexed)
	if err != nil || string(expected) != string(actual) {
		t.Fatalf("rebuilt index changed comparison: %s %v", actual, err)
	}
	archived, err := pg.LogArchiveOverview(ctx)
	if err != nil || !strings.Contains(string(archived.(map[string]any)["organizations"].([]json.RawMessage)[0]), "storedSize") {
		t.Fatalf("archive usage: %v %v", archived, err)
	}
	for index := range 41 {
		idOf(pg.CreateLogFile(ctx, batch, LogFileInput{Name: "paging-" + strconv.Itoa(index), Size: 1}))
	}
	first, err := pg.LogTasks(ctx, LogTaskQuery{State: "uploading"})
	if err != nil {
		t.Fatal(err)
	}
	rawPage, _ := json.Marshal(first)
	var page struct {
		Tasks []json.RawMessage
		Next  struct {
			ID        string
			CreatedAt time.Time
		}
	}
	if err := json.Unmarshal(rawPage, &page); err != nil || len(page.Tasks) != 40 || page.Next.ID == "" {
		t.Fatalf("first task page: %d %v", len(page.Tasks), err)
	}
	older, err := pg.LogTasks(ctx, LogTaskQuery{State: "uploading", Before: &page.Next.CreatedAt, BeforeID: page.Next.ID})
	if err != nil {
		t.Fatal(err)
	}
	tail := older.(map[string]any)["tasks"].([]json.RawMessage)
	if len(tail) == 0 || strings.Contains(string(tail[0]), page.Next.ID) {
		t.Fatalf("older task page repeated its cursor: %s", tail)
	}
	workspace, err := pg.CreateOrganization(ctx, "Log archive retention "+time.Now().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	archiveCtx := WithOrganization(ctx, workspace.ID)
	defer func() {
		_, _ = pg.pool.Exec(ctx, "DELETE FROM log_projects WHERE organization_id=$1", workspace.ID)
		_, _ = pg.pool.Exec(ctx, "DELETE FROM organizations WHERE id=$1", workspace.ID)
	}()
	retentionProject := idOf(pg.CreateLogProject(archiveCtx, LogProjectInput{Name: "Retention fixture"}))
	retentionInstance := idOf(pg.CreateLogInstance(archiveCtx, LogInstanceInput{ProjectID: retentionProject, Name: "Retention instance",
		Environment: "qa", Engine: "postgres", Version: "18"}))
	prepareArchive := func(name string) (string, string) {
		t.Helper()
		batch := idOf(pg.CreateLogBatch(archiveCtx, LogBatchInput{InstanceID: retentionInstance, Name: name}))
		file := idOf(pg.CreateLogFile(archiveCtx, batch, LogFileInput{Name: name + ".log", Node: name, Format: "text", Size: int64(len(line))}))
		if _, err := pg.AppendLogBlock(archiveCtx, file, 0, []byte(line)); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.QueueLogFile(archiveCtx, file, nil); err != nil {
			t.Fatal(err)
		}
		if done, err := pg.ProcessNextLogFile(archiveCtx); err != nil || !done {
			t.Fatalf("parse archive fixture: %v %v", done, err)
		}
		result, err := pg.LogRecords(archiveCtx, batch, LogRecordQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(ctx, "UPDATE log_files SET updated_at=now()-interval '9 days' WHERE id=$1", file); err != nil {
			t.Fatal(err)
		}
		return file, idOf(result.(map[string]any)["records"].([]json.RawMessage)[0], nil)
	}
	retainedFile, retainedRecord := prepareArchive("protected")
	dueFile, dueRecord := prepareArchive("eligible")
	protection := idOf(pg.StartLogInvestigation(archiveCtx, retainedRecord, ""))
	if _, err := pg.PreviewLogArchivePolicy(outOfScope, workspace.ID, 7); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("archive preview crossed organization boundary: %v", err)
	}
	var plan struct{ ExpiringFiles, ProtectedFiles, ReclaimableBytes int }
	preview, err := pg.PreviewLogArchivePolicy(archiveCtx, workspace.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(preview.(json.RawMessage), &plan); err != nil || plan.ExpiringFiles != 1 || plan.ProtectedFiles != 1 || plan.ReclaimableBytes == 0 {
		t.Fatalf("retention preview: %+v %v", plan, err)
	}
	if _, err := pg.SetLogArchivePolicy(archiveCtx, workspace.ID, 7); err != nil {
		t.Fatal(err)
	}
	if cleaned, err := pg.SweepLogArchives(archiveCtx); err != nil || !cleaned {
		t.Fatalf("retention policy did not sweep eligible file %s: %v %v", dueFile, cleaned, err)
	}
	if _, err := pg.LogOriginal(archiveCtx, dueRecord); !errors.Is(err, ErrLogNotFound) {
		t.Fatalf("expired archive still served raw data: %v", err)
	}
	if _, err := pg.VerifyLogFile(archiveCtx, retainedFile); err != nil {
		t.Fatal(err)
	}
	listed, err := pg.LogArchiveFiles(archiveCtx, workspace.ID, "protected", 0)
	if err != nil {
		t.Fatal(err)
	}
	preserved := listed.(map[string]any)["files"].([]json.RawMessage)
	if len(preserved) != 1 || !strings.Contains(string(preserved[0]), protection) {
		t.Fatalf("protected archive lookup lost its investigation: %s", preserved)
	}
}
