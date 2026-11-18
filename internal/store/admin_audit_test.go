package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAdminAuditPersistsOutcomeWithoutSecretsInPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.StartAdminAudit(context.Background(), "GET", "/api/assets"); err == nil {
		t.Fatal("read request was accepted as mutation")
	}
	if _, err := s.StartAdminAudit(context.Background(), "POST", "/api/assets\nsecret"); err == nil {
		t.Fatal("unsafe audit path was accepted")
	}
	for _, tc := range []struct {
		method, path string
		status       int
		outcome      string
	}{
		{"PUT", "/api/inspection-schedules", 201, "accepted"},
		{"PATCH", "/api/alert-rules/pg_connections_high", 400, "rejected"},
	} {
		id, err := s.StartAdminAudit(context.Background(), tc.method, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishAdminAudit(context.Background(), id, tc.status); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishAdminAudit(context.Background(), id, tc.status); err == nil {
			t.Fatal("audit outcome was updated twice")
		}
		var actor, outcome string
		var status int
		if err := s.pool.QueryRow(context.Background(), "SELECT actor,outcome,status_code FROM admin_audit WHERE id=$1", id).Scan(&actor, &outcome, &status); err != nil {
			t.Fatal(err)
		}
		if actor != "admin" || outcome != tc.outcome || status != tc.status {
			t.Fatalf("unexpected audit: %s %s %d", actor, outcome, status)
		}
	}
	eventID := "11111111-1111-1111-1111-111111111111"
	linkedID, err := s.StartAdminAudit(context.Background(), "POST", "/api/events/"+eventID+"/response")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAdminAudit(context.Background(), linkedID, 200); err != nil {
		t.Fatal(err)
	}
	linked, err := s.RecentEventAdminAudit(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	linkedJSON, err := json.Marshal(linked)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(linkedJSON), linkedID) || strings.Contains(string(linkedJSON), "/api/inspection-schedules") {
		t.Fatalf("event operation audit was not isolated: %s", linkedJSON)
	}
	value, err := s.RecentAdminAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "cookie") {
		t.Fatal("audit response contains a secret field")
	}
}
