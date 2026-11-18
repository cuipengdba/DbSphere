package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDropOnlyEmptyExpiredPartitions(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	now := time.Now().UTC()
	oldAt := now.AddDate(0, -5, 0)
	cutoff := now.AddDate(0, -2, 0)
	if err := pg.EnsureTimePartitions(ctx, oldAt); err != nil {
		t.Fatal(err)
	}
	month := time.Date(oldAt.Year(), oldAt.Month(), 1, 0, 0, 0, 0, time.UTC)
	samplePartition := fmt.Sprintf("samples_%04d%02d", month.Year(), month.Month())
	eventPartition := fmt.Sprintf("events_%04d%02d", month.Year(), month.Month())
	var assetID, sampleID, eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO assets
		(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at)
		VALUES(gen_random_uuid(),'partition retention test','127.0.0.1',5432,'postgres',
		'tester','unused','disable','linux',now()) RETURNING id::text`).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", assetID); err != nil {
			t.Errorf("clean up partition retention asset: %v", err)
		}
	}()
	if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status)
		VALUES(gen_random_uuid(),$1,$2,'ok') RETURNING id::text`, assetID, oldAt).Scan(&sampleID); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, assetID, oldAt).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at)
		VALUES($1,$2,'retention_test','retention partition','resolved',$3,$4)`,
		eventID, assetID, oldAt, oldAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DropEmptyExpiredPartitions(ctx, cutoff, cutoff, 4); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{samplePartition, eventPartition} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&present); err != nil || !present {
			t.Fatalf("nonempty expired partition %s was lost: %v", name, err)
		}
	}
	if _, err := pg.pool.Exec(ctx, "DELETE FROM samples WHERE id=$1", sampleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, "DELETE FROM event_keys WHERE id=$1", eventID); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := pg.DropEmptyExpiredPartitions(ctx, cutoff, time.Time{}, 4); err != nil {
			t.Fatal(err)
		}
	}
	var samplePresent, eventPresent bool
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+samplePartition).
		Scan(&samplePresent); err != nil || samplePresent {
		t.Fatalf("empty sample partition %s remains: %v", samplePartition, err)
	}
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+eventPartition).
		Scan(&eventPresent); err != nil || !eventPresent {
		t.Fatalf("event partition without retention was lost: %v", err)
	}
	for range 5 {
		if _, err := pg.DropEmptyExpiredPartitions(ctx, time.Time{}, cutoff, 4); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{samplePartition, eventPartition} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&present); err != nil || present {
			t.Fatalf("empty expired partition %s remains: %v", name, err)
		}
	}
	current := fmt.Sprintf("samples_%04d%02d", now.Year(), now.Month())
	var currentPresent bool
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+current).
		Scan(&currentPresent); err != nil || !currentPresent {
		t.Fatalf("current partition %s was lost: %v", current, err)
	}
	if _, err := pg.DropEmptyExpiredPartitions(ctx, cutoff, cutoff, 0); err == nil {
		t.Fatal("unbounded partition retention was accepted")
	}
}
