package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

const scaleAssets = 300
const scaleDays = 90
const scaleSamples = scaleAssets * scaleDays * 24 * 12

func TestPG18HistoryCapacity(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_HISTORY_SCALE_URL")
	phase := os.Getenv("DBSPHERE_TEST_HISTORY_SCALE_PHASE")
	if dsn == "" || phase == "" {
		t.Skip("disposable PostgreSQL 18 scale database and phase are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var measure any
	switch phase {
	case "seed":
		measure = seedHistoryCapacity(t, ctx, pg)
	case "measure":
		measure = measureHistoryCapacity(t, ctx, pg)
	case "restore":
		measure = verifyHistoryCapacityRestore(t, ctx, pg)
	default:
		t.Fatalf("unknown scale phase %q", phase)
	}
	data, err := json.MarshalIndent(measure, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
	if path := os.Getenv("DBSPHERE_TEST_HISTORY_SCALE_OUTPUT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func seedHistoryCapacity(t *testing.T, ctx context.Context, pg *Postgres) any {
	t.Helper()
	started := time.Now()
	to := started.UTC().Truncate(time.Hour)
	from := to.AddDate(0, 0, -scaleDays)
	for at := from; !at.After(to); at = at.AddDate(0, 0, 21) {
		if err := pg.EnsureTimePartitions(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	a, err := pg.CreateOrganization(ctx, "Scale fixture organization")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `CREATE TABLE history_scale_fixture(
		asset_id uuid PRIMARY KEY,position integer NOT NULL,start_at timestamptz NOT NULL,end_at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO history_scale_fixture
		SELECT md5('dbsphere-capacity-'||n)::uuid,n,$1,$2 FROM generate_series(1,300) n`, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,
		created_at,last_sample_at,last_status,version_num,role,cluster_name,organization_id)
		SELECT asset_id,'Capacity-'||position,'fixture-'||position,5432,'postgres','qa','fixture','require','linux',
		start_at,end_at-interval '5 minutes','ok',180000,'primary','Capacity fixture',
		CASE WHEN position<=5 THEN $1::uuid ELSE $2::uuid END FROM history_scale_fixture`, a.ID, DefaultOrganizationID); err != nil {
		t.Fatal(err)
	}
	var inserted int64
	for offset := 0; offset < scaleDays; offset += 10 {
		tx, err := pg.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Bulk fixture generation is separate from the measured normal write path.
		if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role='replica'"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		first, last := from.AddDate(0, 0, offset), from.AddDate(0, 0, min(offset+10, scaleDays)).Add(-5*time.Minute)
		batch, err := tx.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			SELECT gen_random_uuid(),f.asset_id,stamp,CASE WHEN slot%288=0 THEN 'error' ELSE 'ok' END,
				jsonb_build_object('connections',4+((slot+position)%73),'database_bytes',(1<<24)+slot*1024+position*1024,
				'replication_lag_bytes',(slot%127)*65536,'linux',jsonb_build_object('cpu',jsonb_build_object('usedPercent',slot%100)))
			FROM history_scale_fixture f CROSS JOIN generate_series($1::timestamptz,$2::timestamptz,interval '5 minutes') stamp
			CROSS JOIN LATERAL (SELECT (extract(epoch FROM stamp-f.start_at)/300)::integer AS slot) q`, first, last)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		inserted += batch.RowsAffected()
		t.Logf("seed offset=%d total=%d", offset, inserted)
	}
	if inserted != scaleSamples {
		t.Fatalf("bench workload mismatch: %d", inserted)
	}
	if _, err := pg.pool.Exec(ctx, "ANALYZE assets; ANALYZE samples"); err != nil {
		t.Fatal(err)
	}
	// Seeding and measuring are separate workflow steps. Leave a retention margin so
	// crossing a UTC hour between them does not trim the measured 90-day window.
	policy, err := pg.SetHistoryTierPolicy(ctx, HistoryTierPolicy{OrganizationID: a.ID, Metric: "connections", RawDays: 7, HourlyDays: 91})
	if err != nil {
		t.Fatal(err)
	}
	archiveStart := time.Now()
	var archived int64
	for i := 0; i < 16; i++ {
		pass, err := pg.AdvanceHistoryTier(ctx, to, 5000)
		if err != nil {
			t.Fatal(err)
		}
		archived += pass.Batched
	}
	if archived != 5*4*24*12 {
		t.Fatalf("four historical days for five assets were not archived: %d", archived)
	}
	var bytes int64
	if err := pg.pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"assets": scaleAssets, "days": scaleDays, "sampleIntervalSeconds": 300, "insertedSamples": inserted,
		"seedAndTierSeconds": time.Since(started).Seconds(), "archiveSeconds": time.Since(archiveStart).Seconds(),
		"archivedRawValues": archived, "databaseBytes": bytes, "rawDays": policy.RawDays, "hourlyDays": policy.HourlyDays}
}

func measureHistoryCapacity(t *testing.T, ctx context.Context, pg *Postgres) any {
	t.Helper()
	rows, err := pg.pool.Query(ctx, `SELECT asset_id::text,start_at,end_at FROM history_scale_fixture WHERE position<=5 ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		id       string
		from, to time.Time
	}
	var items []fixture
	for rows.Next() {
		var item fixture
		if err := rows.Scan(&item.id, &item.from, &item.to); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		items = append(items, item)
	}
	rows.Close()
	if len(items) != 5 || rows.Err() != nil {
		t.Fatalf("expected five query assets: %d %v", len(items), rows.Err())
	}
	type observation struct {
		Elapsed float64
		Buckets int
		Error   error
	}
	batch := func(days int) []observation {
		values := make([]observation, len(items))
		var workers sync.WaitGroup
		start := make(chan struct{})
		for i, item := range items {
			workers.Go(func() {
				<-start
				from := item.to.AddDate(0, 0, -days)
				params := url.Values{"from": {from.Format(time.RFC3339)}, "to": {item.to.Format(time.RFC3339)}}
				at := time.Now()
				value, err := pg.ReadHistoryTier(ctx, item.id, "connections", params)
				values[i] = observation{Elapsed: float64(time.Since(at).Microseconds()) / 1000, Error: err}
				if err == nil {
					data := value.(map[string]any)
					values[i].Buckets = len(data["buckets"].([]json.RawMessage))
				}
			})
		}
		close(start)
		workers.Wait()
		return values
	}
	summarize := func(items []observation, expected int) map[string]any {
		values := make([]float64, 0, len(items))
		for _, result := range items {
			if result.Error != nil || result.Buckets != expected {
				t.Fatalf("unexpected capacity query result: %+v expected=%d", result, expected)
			}
			values = append(values, result.Elapsed)
		}
		sort.Float64s(values)
		index := (95*len(values)+99)/100 - 1
		return map[string]any{"p95Ms": values[index], "queries": len(values), "bucketsPerQuery": expected}
	}
	cold := batch(90)
	var warm, hot []observation
	for i := 0; i < 5; i++ {
		warm = append(warm, batch(90)...)
		hot = append(hot, batch(7)...)
	}
	at := time.Now()
	var successful int
	for i, item := range items {
		if _, err := pg.recordCheckAt(ctx, item.id, map[string]any{"connections": 10, "max_connections": 100, "version_num": 180000},
			"", "", time.Now().UTC().Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		successful++
	}
	return map[string]any{"clients": 5, "cold90Days": summarize(cold, 2160), "warm90Days": summarize(warm, 2160),
		"warm7Days": summarize(hot, 168), "collectionWrites": successful, "collectionWriteMs": float64(time.Since(at).Microseconds()) / 1000,
		"fixtureMissedPercent": 0, "fixtureFailedSamples": "one per UTC day per asset"}
}

func verifyHistoryCapacityRestore(t *testing.T, ctx context.Context, pg *Postgres) any {
	t.Helper()
	var raw, hourly, assets int64
	if err := pg.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM samples),(SELECT count(*) FROM history_tier_hours),
		(SELECT count(*) FROM assets)`).Scan(&raw, &hourly, &assets); err != nil {
		t.Fatal(err)
	}
	if raw != scaleSamples+5 || hourly != 5*4*24 || assets != scaleAssets {
		t.Fatalf("restored scale database counts did not match: samples=%d hours=%d assets=%d", raw, hourly, assets)
	}
	return map[string]any{"assets": assets, "samples": raw, "archivedHours": hourly}
}
