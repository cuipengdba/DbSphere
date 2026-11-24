package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogArchiveCapacity(t *testing.T) {
	url, limit := os.Getenv("DBSPHERE_TEST_PG18_URL"), os.Getenv("DBSPHERE_TEST_LOG_CAPACITY_MIB")
	if url == "" || limit == "" {
		t.Skip("requires the PostgreSQL 18 test store and DBSPHERE_TEST_LOG_CAPACITY_MIB")
	}
	totalMiB, err := strconv.Atoi(limit)
	if err != nil || totalMiB < 2 || totalMiB > 32768 {
		t.Fatal("capacity input must be between 2 and 32768 MiB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
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
		data, _ := json.Marshal(value)
		if err := json.Unmarshal(data, &result); err != nil || result.ID == "" {
			t.Fatalf("invalid resource %s: %v", data, err)
		}
		return result.ID
	}
	name := "Archive capacity " + time.Now().Format(time.RFC3339Nano)
	project := idOf(pg.CreateLogProject(ctx, LogProjectInput{Name: name}))
	retain := os.Getenv("DBSPHERE_LOG_CAPACITY_KEEP_FIXTURE") != ""
	if !retain {
		defer pg.pool.Exec(ctx, "DELETE FROM log_projects WHERE id=$1", project)
	}
	var asset string
	err = pg.pool.QueryRow(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at,organization_id)
		SELECT gen_random_uuid(),'Archive capacity asset','127.0.0.1',5432,'postgres','fixture','','disable','linux',now(),organization_id
		FROM log_projects WHERE id=$1 RETURNING id::text`, project).Scan(&asset)
	if err != nil {
		t.Fatal(err)
	}
	if !retain {
		defer pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", asset)
	}
	instance := idOf(pg.CreateLogInstance(ctx, LogInstanceInput{
		ProjectID: project, AssetID: asset, Name: "Capacity fixture", Environment: "qa", Engine: "postgres", Version: "18",
	}))
	var batches [2]string
	var bytesInput int64
	started := time.Now()
	baseAt := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	for side := range batches {
		batches[side] = idOf(pg.CreateLogBatch(ctx, LogBatchInput{InstanceID: instance, Coverage: "full", Name: strconv.Itoa(side)}))
		fileMiB := totalMiB/2 + side*(totalMiB%2)
		step := 30 * 24 * time.Hour / time.Duration(max(1, fileMiB-1))
		partIndex := 0
		for pending := fileMiB; pending > 0; {
			blocks := min(LogFileLimit/LogArchiveBlockBytes, pending)
			file := idOf(pg.CreateLogFile(ctx, batches[side], LogFileInput{
				Name: fmt.Sprintf("fixture-%d-%d.log", side, partIndex), Format: "text", Size: int64(blocks) * LogArchiveBlockBytes,
			}))
			for part := range blocks {
				dayOffset := time.Duration(partIndex) * step
				at := baseAt.Add(time.Duration(side)*30*24*time.Hour + dayOffset)
				block := capacityLogBlock(at, side, partIndex)
				if _, err := pg.AppendLogBlock(ctx, file, part, block); err != nil {
					t.Fatal(err)
				}
				bytesInput += int64(len(block))
				partIndex++
			}
			if _, err := pg.QueueLogFile(ctx, file, nil); err != nil {
				t.Fatal(err)
			}
			if ok, err := pg.ProcessNextLogFile(ctx); err != nil || !ok {
				t.Fatalf("parse %s: %v %v", file, ok, err)
			}
			pending -= blocks
			t.Logf("batch %d: committed %d/%d MiB", side, partIndex, fileMiB)
		}
	}
	ingest := time.Since(started)
	warm := time.Now()
	initial, err := pg.LogComparison(ctx, batches[0], batches[1])
	if err != nil {
		t.Fatal(err)
	}
	initialDuration := time.Since(warm)
	rows := initial.(map[string]any)["rows"].([]*logCompareRow)
	if len(rows) < 3 {
		t.Fatalf("mixed fixture yielded %d groups", len(rows))
	}
	runs := make([]time.Duration, 20)
	for attempt := range runs {
		now := time.Now()
		if _, err := pg.LogComparison(ctx, batches[0], batches[1]); err != nil {
			t.Fatal(err)
		}
		runs[attempt] = time.Since(now)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i] < runs[j] })
	anchor := evidenceFor(rows[0])
	now := time.Now()
	if _, err := pg.LogOriginal(ctx, anchor); err != nil {
		t.Fatal(err)
	}
	original := time.Since(now)
	if _, err := pg.StartLogInvestigation(ctx, anchor, ""); err != nil {
		t.Fatal(err)
	}
	var compressed int64
	if err := pg.pool.QueryRow(ctx, "SELECT sum(stored_size) FROM log_files WHERE batch_id IN ($1,$2)", batches[0], batches[1]).Scan(&compressed); err != nil {
		t.Fatal(err)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	t.Logf("raw=%d compressed=%d ratio=%.4f%% ingest=%s ingestMiBps=%.2f rollupBuild=%s comparisonP95=%s original=%s goHeapMiB=%.2f",
		bytesInput, compressed, float64(compressed)/float64(bytesInput)*100, ingest, float64(totalMiB)/ingest.Seconds(),
		initialDuration, runs[18], original, float64(mem.HeapInuse)/(1<<20))
	if runs[18] > 2*time.Second {
		t.Errorf("indexed comparisons exceeded the 2 second P95 target: %s", runs[18])
	}
}

func TestLogArchiveRestore(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_LOG_RESTORED_URL")
	if url == "" {
		t.Skip("requires a restored PostgreSQL 18 archive test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var original, file, investigation, event string
	err = pg.pool.QueryRow(ctx, `SELECT r.id::text,r.file_id::text,i.id::text,i.event_id::text FROM log_investigations i
		JOIN log_records r ON r.id=i.primary_record ORDER BY i.created_at DESC LIMIT 1`).Scan(&original, &file, &investigation, &event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.LogInvestigation(ctx, investigation); err != nil {
		t.Fatal(err)
	}
	report, ids, err := pg.EventAIEvidence(ctx, event)
	if err != nil || len(ids) == 0 || !strings.Contains(report, original) {
		t.Fatalf("restored event must preserve the original record reference: %v", err)
	}
	if _, err := pg.LogOriginal(ctx, original); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.VerifyLogFile(ctx, file); err != nil {
		t.Fatal(err)
	}
	t.Log("restored event, investigation, original log and compressed-block digests")
}

func capacityLogBlock(at time.Time, side, part int) []byte {
	const recordSize = 1 << 16
	var output bytes.Buffer
	output.Grow(LogArchiveBlockBytes)
	for index := range LogArchiveBlockBytes / recordSize {
		at = at.Add(time.Second)
		prefix := at.Format("2006-01-02 15:04:05.000") + " UTC "
		statement := fmt.Sprintf("[52] app=orders-%d LOG: duration: %d ms statement: select id from orders_%d where key='%d-%d-%d'",
			part%7, 700+side*500+part%47, part%101, side, part, index)
		switch index % 8 {
		case 1:
			statement = "[52] app=orders-api ERROR: 22012: division by zero"
		case 2:
			statement = "[52] app=orders-api LOG: temporary file: path \"base/pgsql_tmp/example\", size 1048576"
		case 3:
			statement = "[52] app=orders-api LOG: process 52 still waiting for ShareLock on transaction 91 after 1000.000 ms"
		case 4:
			statement = "[53] app=billing-api LOG: duration: 1200 ms statement: update invoices set verified=true"
		}
		output.WriteString(prefix)
		output.WriteString(statement)
		output.WriteByte('\n')
		output.WriteString(strings.Repeat(" ", recordSize-len(prefix)-len(statement)-2))
		output.WriteByte('\n')
	}
	return output.Bytes()
}
