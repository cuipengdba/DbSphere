package store

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPG18OIDCGroupChangesRevokeSessions(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL 18 is required")
	}
	ctx := t.Context()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	name := fmt.Sprintf("oidc-group-%x", time.Now().UnixNano())
	org, err := s.CreateOrganization(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://oidc.example.test"
	account, err := s.WriteAccessAccount(ctx, "", AccessAccountInput{
		Username: name, Role: "viewer", OrganizationID: org.ID, OIDCIssuer: issuer, OIDCSubject: &name,
	}, "unused-test-hash")
	if err != nil {
		t.Fatal(err)
	}
	grant := OIDCGroupGrant{OrganizationID: org.ID, Role: "operator"}
	account, err = s.ApplyOIDCGroupGrant(ctx, issuer, name, now, &grant)
	if err != nil || account.Role != "operator" {
		t.Fatalf("expected operator grant, got %q: %v", account.Role, err)
	}
	if err := s.CreateAccessSession(ctx, account, name, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertDenied := func(at time.Time, mapped *OIDCGroupGrant) {
		t.Helper()
		if _, err := s.ApplyOIDCGroupGrant(ctx, issuer, name, at, mapped); !errors.Is(err, ErrAccessMissing) {
			t.Fatalf("expected rejected claim, got %v", err)
		}
	}
	assertDenied(now.Add(time.Second), nil)
	if _, err := s.AccessSession(ctx, name, now); !errors.Is(err, ErrAccessMissing) {
		t.Fatalf("expected revoked session, got %v", err)
	}
	assertDenied(now, &grant)
	assertDenied(now.Add(time.Second), &grant)
	grant.Role = "viewer"
	grant.OrganizationID = DefaultOrganizationID
	updated, err := s.ApplyOIDCGroupGrant(ctx, issuer, name, now.Add(2*time.Second), &grant)
	if err != nil || updated.OrganizationID != DefaultOrganizationID || updated.Role != "viewer" {
		t.Fatalf("new grant not applied: %+v: %v", updated, err)
	}
}
