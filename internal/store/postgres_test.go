package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
)

func TestRealPostgreSQL18Connection(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version, partitions int
	if err := store.pool.QueryRow(context.Background(),
		"SELECT max(version) FROM dbsphere_schema_migrations").Scan(&version); err != nil || version < 13 {
		t.Fatalf("Go schema migration absent: %d %v", version, err)
	}
	if err := store.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_partition_tree('samples'::regclass)").Scan(&partitions); err != nil || partitions < 3 {
		t.Fatalf("monthly sample partitions absent: %d %v", partitions, err)
	}
	hash, err := store.AdminPasswordHash(context.Background(), "first-bootstrap-password")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.AdminPasswordHash(context.Background(), "different-bootstrap-password")
	if err != nil || kept != hash {
		t.Fatal("existing administrator hash was replaced")
	}
	for _, path := range []string{"/api/assets", "/api/events", "/api/alerts", "/api/alert-rules",
		"/api/silences", "/api/notifications", "/api/storage", "/api/inspection-templates",
		"/api/inspection-schedules", "/api/inspection-runs"} {
		value, found, err := store.ReadCollection(context.Background(), path)
		if err != nil || !found {
			t.Fatalf("Go read API %s failed: %v", path, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "password_enc") || strings.Contains(string(encoded), "pooler_url_enc") {
			t.Fatalf("Go read API %s exposed secrets or invalid JSON: %v", path, err)
		}
	}
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "Go CRUD test", Host: "127.0.0.1", Port: 5432, Database: "postgres",
		Username: "dbsphere_monitor", Password: "fixture-secret", SSLMode: "disable", Runtime: "linux"}
	created, err := store.CreateAsset(context.Background(), input, key)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(created, &public); err != nil {
		t.Fatal(err)
	}
	defer store.DeleteAsset(context.Background(), public.ID)
	if strings.Contains(string(created), "fixture-secret") || strings.Contains(string(created), "password_enc") {
		t.Fatal("asset API exposed encrypted or plaintext credential")
	}
	var oldCipher string
	if err := store.pool.QueryRow(context.Background(), "SELECT password_enc FROM assets WHERE id=$1", public.ID).Scan(&oldCipher); err != nil {
		t.Fatal(err)
	}
	plain, err := security.Decrypt(key, oldCipher)
	if err != nil || plain != "fixture-secret" {
		t.Fatal("asset secret did not survive PG18 storage")
	}
	input.Name = "Updated Go CRUD test"
	input.Password = ""
	updated, err := store.UpdateAsset(context.Background(), public.ID, input, key)
	if err != nil || !strings.Contains(string(updated), "Updated Go CRUD test") {
		t.Fatal("asset update failed", err)
	}
	var keptCipher string
	if err := store.pool.QueryRow(context.Background(), "SELECT password_enc FROM assets WHERE id=$1", public.ID).Scan(&keptCipher); err != nil || keptCipher != oldCipher {
		t.Fatal("blank edit replaced the existing credential")
	}
	deleted, err := store.DeleteAsset(context.Background(), public.ID)
	if err != nil || !deleted {
		t.Fatal("asset deletion failed", err)
	}
}

func TestOneEventIDAcrossFailureAndRecovery(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	if url == "" || portText == "" {
		t.Skip("PG18 store and PostgreSQL target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "Go event ID test", Host: "127.0.0.1", Port: port,
		Database: "postgres", Username: "postgres", Password: os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"),
		SSLMode: "disable", Runtime: "linux"}
	created, err := pg.CreateAsset(context.Background(), input, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	first, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || first.Status != "ok" {
		t.Fatalf("initial PG14–18 sample failed: %v %v", first, err)
	}
	input.Port = 1
	input.Password = ""
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := pg.CheckAsset(context.Background(), asset.ID, key)
		if err != nil || result.Status != "error" {
			t.Fatalf("connection failure was not sampled: %v %v", result, err)
		}
	}
	var eventID string
	var count int
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text
		FROM events WHERE asset_id=$1 AND status='open' LIMIT 1`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("failure event missing", err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int
		FROM events WHERE asset_id=$1 AND status='open'`, asset.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated failures did not share one event: %q %d %v", eventID, count, err)
	}
	input.Port = port
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	recovered, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || recovered.Status != "ok" {
		t.Fatalf("recovery sample failed: %v %v", recovered, err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), "SELECT status FROM events WHERE id=$1", eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("event was not resolved under the same ID: %s %v", status, err)
	}
	if err := pg.pool.QueryRow(context.Background(), "SELECT count(*)::int FROM evidence WHERE event_id=$1", eventID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("failure/recovery evidence did not share one ID: %d %v", count, err)
	}
	input.Runtime = "k8s"
	input.K8sCluster = "test"
	input.Namespace = "database"
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CheckAsset(context.Background(), asset.ID, key); !errors.Is(err, ErrCollectionUnavailable) {
		t.Fatal("Go collector incorrectly marked a Kubernetes asset healthy", err)
	}
}
