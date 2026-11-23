package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestCollectionLeaseTakeoverFencesSamplesAndRetries(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "collection lease fixture", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "monitor", Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	const interval = 15 * time.Second
	claim := func() CollectionLease {
		t.Helper()
		jobs, err := pg.ClaimCollectionJobs(ctx, interval, 100, CollectionAny)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.AssetID == asset.ID {
				return job
			}
		}
		return CollectionLease{}
	}
	first := claim()
	if first.Token == "" || claim().Token != "" {
		t.Fatal("active collection lease was missing or could be claimed twice")
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE collection_jobs SET lease_until=now()-interval '1 second' WHERE asset_id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	second := claim()
	if second.Token == "" || second.Token == first.Token || !second.DueAt.Equal(first.DueAt) {
		t.Fatalf("takeover lost logical job or fencing token: first=%+v second=%+v", first, second)
	}
	if _, err := pg.CheckCollectionLease(ctx, first, interval, key); !errors.Is(err, ErrCollectionLeaseLost) {
		t.Fatalf("superseded worker wrote a sample: %v", err)
	}
	result, err := pg.CheckCollectionLease(ctx, second, interval, key)
	if err != nil || result.Status != "error" || result.SampleID == "" {
		t.Fatalf("replacement worker did not persist the real connection failure: %+v %v", result, err)
	}
	if err := pg.RetryCollectionLease(ctx, second); err != nil {
		t.Fatal(err)
	}
	var outcome, sampleID string
	var failures, samples, events int
	var delay float64
	if err := pg.pool.QueryRow(ctx, `SELECT last_outcome,last_sample_id::text,failures,
		extract(epoch FROM due_at-last_finished_at)::float8,
		(SELECT count(*) FROM samples WHERE asset_id=j.asset_id),
		(SELECT count(*) FROM events WHERE asset_id=j.asset_id AND event_group='availability')
		FROM collection_jobs j WHERE asset_id=$1`, asset.ID).Scan(&outcome, &sampleID, &failures, &delay, &samples, &events); err != nil {
		t.Fatal(err)
	}
	if outcome != "target_error" || sampleID != result.SampleID || failures != 1 || delay != 15 || samples != 1 || events != 1 {
		t.Fatalf("retry changed a committed job: outcome=%s failures=%d delay=%v samples=%d events=%d", outcome, failures, delay, samples, events)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE collection_jobs SET due_at=now()-interval '1 second' WHERE asset_id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	next := claim()
	if next.AssetID != asset.ID || next.Token == second.Token {
		t.Fatal("the next due job was unavailable")
	}
	if err := pg.RetryCollectionLease(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT last_outcome,failures,extract(epoch FROM due_at-last_finished_at)::float8
		FROM collection_jobs WHERE asset_id=$1`, asset.ID).Scan(&outcome, &failures, &delay); err != nil {
		t.Fatal(err)
	}
	if outcome != "execution_error" || failures != 2 || delay != 10 || claim().Token != "" {
		t.Fatalf("execution failure did not enter persistent backoff: %s failures=%d delay=%v", outcome, failures, delay)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE collection_jobs SET due_at=now()-interval '1 second' WHERE asset_id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	regular, err := pg.ClaimCollectionJobs(ctx, interval, 100, CollectionRegular)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range regular {
		if job.AssetID == asset.ID {
			t.Fatal("repeatedly failing asset was claimed by the regular lane")
		}
	}
	isolated, err := pg.ClaimCollectionJobs(ctx, interval, 100, CollectionIsolated)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, job := range isolated {
		if job.AssetID == asset.ID {
			found = job.Isolated && !job.ClaimedAt.Before(job.DueAt)
		}
	}
	if !found {
		t.Fatal("the isolated lane did not claim the delayed asset")
	}
}
