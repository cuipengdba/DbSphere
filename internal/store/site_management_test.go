package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/remoteobs"
)

func TestManagedSiteSummaryAndUpgradePG18(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	suffix := time.Now().UTC().Format("150405000000")
	first, second := "east-"+suffix, "west-"+suffix
	a, err := pg.CreateOrganization(ctx, "QA East "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pg.CreateOrganization(ctx, "QA West "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	account, err := pg.WriteAccessAccount(ctx, "", AccessAccountInput{Username: "qa-site-" + suffix, Role: "operator", OrganizationID: a.ID}, "unused-test-hash")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pg.pool.Exec(ctx, `DELETE FROM site_upgrade_history WHERE site IN ($1,$2)`, first, second)
		_, _ = pg.pool.Exec(ctx, `DELETE FROM managed_sites WHERE site IN ($1,$2)`, first, second)
		_, _ = pg.pool.Exec(ctx, `DELETE FROM access_accounts WHERE id=$1`, account.ID)
		_, _ = pg.pool.Exec(ctx, `DELETE FROM organizations WHERE id IN ($1,$2)`, a.ID, b.ID)
	}()
	for _, name := range []string{first, second} {
		if err := pg.SaveManagedSite(ctx, ManagedSiteInput{Site: name, Organization: a.ID, Label: name,
			ConsoleURL: "https://qa-site.example.invalid/", AssetCap: 100, Delegate: account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	ctxA := WithOrganization(ctx, a.ID)
	ctxB := WithOrganization(ctx, b.ID)
	if _, err := pg.SiteUpgradeHistory(ctxB, first, true, ""); !errors.Is(err, ErrSiteUnavailable) {
		t.Fatalf("cross-organization read: %v", err)
	}
	if _, err := pg.SiteUpgradeHistory(ctxA, first, false, account.ID); err != nil {
		t.Fatalf("delegate could not read site: %v", err)
	}
	if _, err := pg.SiteUpgradeHistory(ctxA, first, false, ""); !errors.Is(err, ErrSiteUnavailable) {
		t.Fatalf("undelegated read: %v", err)
	}
	newPoint := func(version string, assets int) remoteobs.SiteSummary {
		t.Helper()
		var id string
		if err := pg.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return remoteobs.SiteSummary{ID: id, Version: version, ObservedAt: time.Now().UTC(), IntervalSeconds: 10, AssetCount: assets, Missing: assets, Events: []remoteobs.SiteEvent{}}
	}
	write := func(site string, point remoteobs.SiteSummary, want string) {
		t.Helper()
		receipt, err := pg.SaveSiteSummary(ctx, site, point)
		if err != nil || receipt.ID != point.ID || receipt.Status != want {
			t.Fatalf("summary %s: %+v %v", site, receipt, err)
		}
	}
	point := newPoint("4.7.0", 100)
	write(first, point, "stored")
	write(first, point, "duplicate")
	changed := point
	changed.Missing, changed.Healthy = 99, 1
	if _, err := pg.SaveSiteSummary(ctx, first, changed); !errors.Is(err, ErrRemoteConflict) {
		t.Fatalf("same ID with changed payload: %v", err)
	}
	older := newPoint("4.7.0", 100)
	older.ObservedAt = point.ObservedAt.Add(-time.Second)
	write(first, older, "superseded")
	if _, err := pg.SaveSiteSummary(ctx, first, newPoint("4.7.0", 101)); !errors.Is(err, ErrSiteCapacity) {
		t.Fatalf("asset cap: %v", err)
	}
	write(first, newPoint("4.7.0", 100), "stored")
	write(second, newPoint("4.7.0", 100), "stored")
	schedule := func(wave int) SiteUpgradeInput {
		return SiteUpgradeInput{State: "scheduled", Batch: "qa-release", Wave: wave, TargetVersion: "4.8.0", Note: "QA upgrade"}
	}
	transition := func(site string, rev int, state string) error {
		return pg.RecordSiteUpgrade(ctxA, site, false, account.ID, SiteUpgradeInput{Revision: rev, State: state, Note: "QA observed change"})
	}
	if err := pg.RecordSiteUpgrade(ctxA, first, false, account.ID, schedule(1)); !errors.Is(err, ErrSiteTransition) {
		t.Fatalf("delegate scheduled a batch: %v", err)
	}
	if err := pg.RecordSiteUpgrade(ctxA, first, true, "", schedule(1)); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordSiteUpgrade(ctxA, second, true, "", schedule(2)); err != nil {
		t.Fatal(err)
	}
	if err := transition(second, 1, "upgrading"); !errors.Is(err, ErrSiteTransition) {
		t.Fatalf("wave skipped: %v", err)
	}
	if err := transition(first, 1, "upgrading"); err != nil {
		t.Fatal(err)
	}
	if err := transition(first, 2, "verified"); !errors.Is(err, ErrSiteTransition) {
		t.Fatalf("incorrect version verified: %v", err)
	}
	write(first, newPoint("4.8.0", 100), "stored")
	if err := transition(first, 2, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := transition(second, 1, "upgrading"); err != nil {
		t.Fatal(err)
	}
	if err := transition(second, 2, "failed"); err != nil {
		t.Fatal(err)
	}
	if err := transition(second, 3, "rolling_back"); err != nil {
		t.Fatal(err)
	}
	write(second, newPoint("4.7.0", 100), "stored")
	if err := transition(second, 4, "rolled_back"); err != nil {
		t.Fatal(err)
	}
	result, err := pg.SiteUpgradeHistory(ctxA, second, false, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var history struct {
		History []struct{ State, ObservedSummaryID string }
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &history); err != nil || len(history.History) != 5 || history.History[0].State != "rolled_back" || history.History[0].ObservedSummaryID == "" {
		t.Fatalf("rollback record: %s %v", encoded, err)
	}
}
