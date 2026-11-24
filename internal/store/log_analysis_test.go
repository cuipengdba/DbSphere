package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	instance := idOf(pg.CreateLogInstance(ctx, LogInstanceInput{ProjectID: project, Name: "Orders", Environment: "production", Engine: "postgres", Version: "18"}))
	batch := idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Name: "Rotation 1", Coverage: "slow"}))
	line := "2026-11-24 19:10:00.123 UTC [12] app=orders-api LOG: duration: 2100.5 ms statement: select a from orders where code='fixture-private'\n"
	payload := []byte(strings.Repeat("x", LogArchiveBlockBytes-18) + "\n" + line)
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
		}
		return file
	}
	file := upload(batch)
	if value, err := pg.QueueLogFile(ctx, file, nil); err != nil || value.(map[string]any)["state"] != "queued" {
		t.Fatalf("queue: %v %v", value, err)
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
}
