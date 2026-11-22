package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/jackc/pgx/v5"
)

func fixtureRiskHours(start, end time.Time) []riskHour {
	var hours []riskHour
	for at := start; at.Before(end); at = at.Add(time.Hour) {
		connection := 30 + float64(at.Hour()%3)
		size := 100 * riskGiB
		wal := float64(2 * 1024 * 1024)
		hours = append(hours, riskHour{At: at, Connections: &connection, DatabaseBytes: &size, WALRate: &wal})
	}
	return hours
}

func risingRiskFixture(hours []riskHour, end time.Time) {
	for i := range hours {
		bucket := &hours[i]
		offset := int(end.Sub(bucket.At).Hours())
		if offset < 1 || offset > 5 {
			continue
		}
		*bucket.Connections = 75 - float64(offset*5)
		*bucket.DatabaseBytes = (112 - float64(offset)) * riskGiB
		*bucket.WALRate = float64(16-offset*2) * 1024 * 1024
	}
}

func TestRiskBaselineLabeledScenarios(t *testing.T) {
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	hours := fixtureRiskHours(at.AddDate(0, 0, -21), at.Add(48*time.Hour))
	risingRiskFixture(hours, at)
	for i := range hours {
		if hours[i].At.Equal(at.Add(2 * time.Hour)) {
			*hours[i].Connections = 89
		}
	}
	risk := evaluateRisks(at, riskSignals{Hours: hours, RecentFailures: 3, FailureEvent: "original-failure"})
	if len(risk) != 4 || risk[0].Status != "risk" || risk[0].LeadMinutes != 180 ||
		risk[1].Status != "risk" || risk[2].Status != "risk" || risk[3].SourceEventID != "original-failure" {
		t.Fatalf("expected connection, capacity, WAL and recurrence fixture hits: %+v", risk)
	}
	safe := evaluateRisks(at, riskSignals{Hours: fixtureRiskHours(at.AddDate(0, 0, -21), at)})
	for _, assessment := range safe {
		if assessment.Status != "clear" {
			t.Fatalf("typical periodic measurements produced a false positive: %+v", assessment)
		}
	}
	planned := evaluateRisks(at, riskSignals{Hours: hours, Planned: true, RecentFailures: 3})
	for _, assessment := range planned {
		if assessment.Status != "planned" {
			t.Fatalf("maintenance window was not reported: %+v", assessment)
		}
	}
	missing := evaluateRisks(at, riskSignals{Hours: hours[:5]})
	for _, assessment := range missing[:3] {
		if assessment.Status != "insufficient" {
			t.Fatalf("missing evidence yielded a forecast: %+v", assessment)
		}
	}
	quality := assessRiskQuality(forecastSpecs[0], riskSignals{Hours: hours}, at.Add(8*time.Hour), 7)
	if quality.TruePositive < 1 || quality.TrueNegative < 1 || quality.MedianLeadMinutes < 60 {
		t.Fatalf("historical replay did not recognize planted crossing or lead time: %+v", quality)
	}
	t.Logf("labeled current risks=4 clear=4 maintenance=4 insufficient=3 predicted connection lead=%d minutes; historical=%+v",
		risk[0].LeadMinutes, quality)
}

func TestPG18RiskShadowReviewAndSameEvent(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_RISK_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PG18 forecast database is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	raw, err := pg.CreateAsset(ctx, assets.Input{Name: "Risk review fixture", Host: "127.0.0.1", Port: 1, Database: "postgres",
		Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"}, key)
	var asset struct{ ID string }
	if err != nil || json.Unmarshal(raw, &asset) != nil || asset.ID == "" {
		t.Fatalf("create risk asset: %v", err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	hour := time.Now().UTC().Truncate(time.Hour)
	metrics := func(connections int) map[string]any {
		return map[string]any{"connections": connections, "max_connections": 100, "version_num": 180000}
	}
	source, err := pg.recordCheckAt(ctx, asset.ID, metrics(70), "", "", hour.Add(-59*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	hours := fixtureRiskHours(hour.AddDate(0, 0, -21), hour.Add(time.Hour))
	risingRiskFixture(hours, hour)
	var fixture pgx.Batch
	for _, bucket := range hours {
		value := *bucket.Connections
		if bucket.At.Equal(hour) {
			value = 75
		}
		fixture.Queue(`INSERT INTO monitoring_hourly_buckets(asset_id,bucket,value)
			VALUES($1,$2,jsonb_build_object('latestSampleId',$3::text,'averageConnectionUsagePercent',$4::float8,
			'averageDatabaseBytes',$5::float8,'walBytesPerSecond',$6::float8))`, asset.ID, bucket.At, source.SampleID,
			value, *bucket.DatabaseBytes, *bucket.WALRate)
	}
	if err := pg.pool.SendBatch(ctx, &fixture).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRiskBaseline(ctx, hour, 10); err != nil {
		t.Fatal(err)
	}
	var shadowID string
	var lead int
	if err := pg.pool.QueryRow(ctx, `SELECT id::text,(forecast->>'leadMinutes')::int FROM risk_observations
		WHERE asset_id=$1 AND metric='connections' AND status='risk' AND mode='shadow' AND event_id IS NULL`, asset.ID).
		Scan(&shadowID, &lead); err != nil || lead != 180 {
		t.Fatalf("expected shadow risk with lead and no event: %v %d", err, lead)
	}
	if err := pg.SetRiskMode(ctx, asset.ID, true); !errors.Is(err, ErrRiskReviewRequired) {
		t.Fatalf("notifications were enabled without review: %v", err)
	}
	if err := pg.ReviewRiskForecast(WithAccessActor(ctx, "fixture-operator"), asset.ID, shadowID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRiskMode(ctx, asset.ID, true); err != nil {
		t.Fatal(err)
	}
	next := hour.Add(time.Hour)
	if _, err := pg.AdvanceRiskBaseline(ctx, next, 10); err != nil {
		t.Fatal(err)
	}
	var event, evidenceID string
	if err := pg.pool.QueryRow(ctx, `SELECT r.event_id::text,e.id::text FROM risk_observations r JOIN evidence e ON e.event_id=r.event_id
		WHERE r.asset_id=$1 AND r.metric='connections' AND r.mode='active' AND e.kind='risk_forecast' AND e.payload_json->>'sampleId'=$2`,
		asset.ID, source.SampleID).Scan(&event, &evidenceID); err != nil {
		t.Fatalf("active risk did not keep the original sample reference: %v", err)
	}
	for _, at := range []time.Time{next.Add(time.Minute), next.Add(3 * time.Minute)} {
		if _, err := pg.recordCheckAt(ctx, asset.ID, metrics(98), "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	var actual string
	if err := pg.pool.QueryRow(ctx, `SELECT e.event_id::text FROM evidence e JOIN event_keys k ON k.id=e.event_id
		WHERE k.asset_id=$1 AND e.kind='alert_firing' AND e.payload_json->>'ruleId'='pg_connections_high'`,
		asset.ID).Scan(&actual); err != nil || actual != event {
		t.Fatalf("the observed capacity alert did not reuse the forecast event: %v %s %s", err, actual, event)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO silences(id,kind,asset_id,starts_at,ends_at,reason)
		VALUES(gen_random_uuid(),'maintenance',$1,$2,$3,'forecast maintenance fixture')`, asset.ID, next, next.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AdvanceRiskBaseline(ctx, next.Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	var planned int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::integer FROM risk_observations WHERE asset_id=$1 AND evaluated_at=$2 AND status='planned'`,
		asset.ID, next.Add(time.Hour)).Scan(&planned); err != nil || planned != 4 {
		t.Fatalf("forecast ignored planned windows: %v %d", err, planned)
	}
	t.Logf("shadow=%s leadMinutes=%d reviewed=true sample=%s event=%s firingEvent=%s evidence=%s planned=%d",
		shadowID, lead, source.SampleID, event, actual, evidenceID, planned)
}
