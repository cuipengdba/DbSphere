package collector

import (
	"testing"
	"time"
)

func TestBackupCatalogChecksOnlyMatchingRepositoryAndTimeline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	report, ok := readBackupCatalog([]byte(`{
		"pgbackrest":[{"name":"ledger","status":{"code":0},
			"repo":[{"key":1,"status":{"code":0}},{"key":2,"status":{"code":1}}],
			"archive":[{"database":{"id":1,"repo-key":1},"min":"000000010000000000000000","max":"000000010000000000000002"}],
			"backup":[
				{"label":"20251231-230000F","type":"full","timestamp":{"start":1767222000,"stop":1767222010},"database":{"id":1,"repo-key":1}},
				{"label":"20251231-230000F_20251231-233000I","type":"incr","timestamp":{"start":1767223800,"stop":1767223810},
				 "prior":"20251231-230000F","database":{"id":1,"repo-key":2}}
			]}],
		"walChecks":[{"repoKey":1,"databaseId":1,"from":"000000010000000000000000","to":"000000010000000000000002",
			"segmentBytes":16777216,"segments":["000000010000000000000000","000000020000000000000001","000000010000000000000002"]},
			{"repoKey":2,"databaseId":1,"from":"000000010000000000000000","to":"000000010000000000000002","segmentBytes":16777216,"segments":[]}],
		"restoreTests":[{"repoKey":1,"backup":"20251231-230000F","targetAt":"2025-12-31T23:05:00Z",
			"startedAt":"2025-12-31T23:10:00Z","finishedAt":"2025-12-31T23:11:00Z","confirmedBefore":true,"excludedAfter":true}]
	}`), now)
	if !ok {
		t.Fatal("valid catalog rejected")
	}
	checks := report["walChecks"].([]map[string]any)
	if len(checks) != 1 || checks[0]["missingCount"] != 1 || checks[0]["missing"].([]string)[0] != "000000010000000000000001" {
		t.Fatalf("WAL verification mixed repositories or timelines: %v", checks)
	}
	jobs := report["jobs"].([]map[string]any)
	if len(jobs) != 2 || jobs[0]["dependenciesAvailable"] != false || jobs[1]["dependenciesAvailable"] != true {
		t.Fatalf("backup dependency was matched across repositories: %v", jobs)
	}
	tests := report["restoreTests"].([]map[string]any)
	if len(tests) != 1 || tests[0]["rtoSeconds"] != float64(60) || tests[0]["rpoSeconds"] != nil {
		t.Fatalf("unmeasured RPO or incorrect RTO: %v", tests)
	}
}
