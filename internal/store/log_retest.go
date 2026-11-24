package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateLogRetests(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=59)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE log_retests(
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), investigation_id uuid NOT NULL REFERENCES log_investigations(id) ON DELETE CASCADE,
		baseline uuid NOT NULL REFERENCES log_batches(id) ON DELETE CASCADE,
		current_batch uuid NOT NULL REFERENCES log_batches(id) ON DELETE CASCADE,
		baseline_revision bigint NOT NULL,current_revision bigint NOT NULL,
		target_ms double precision NOT NULL CHECK(target_ms>0 AND target_ms<=3600000),
		result jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),
		UNIQUE(investigation_id,baseline,current_batch,baseline_revision,current_revision,target_ms)
	); CREATE INDEX log_retests_investigation_time ON log_retests(investigation_id,created_at DESC,id);
	INSERT INTO dbsphere_schema_migrations(version) VALUES (59)`)
	return err
}

type LogRetestInput struct {
	BaselineBatchID string
	CurrentBatchID  string
	TargetP95MS     float64
}

type logRetestSample struct {
	BatchID  string   `json:"batchId"`
	RecordID string   `json:"recordId"`
	Timed    int64    `json:"timed"`
	P95      *float64 `json:"p95Ms"`
}

func logRetestDecision(reason string, base, current logCompareMeasure, target float64) (string, string) {
	if reason != "" {
		return "insufficient", reason
	}
	if base.Timed < 5 || current.Timed < 5 || base.P95 == nil || current.P95 == nil {
		return "insufficient", "两批次至少各需五个同指纹、同应用的计时样本。"
	}
	change := classifyLogChange(&logCompareRow{Kind: "statement", Baseline: &base, Current: &current}, true)
	if change == "regressed" {
		return "regressed", "相同口径下 P95 至少上升 25% 和 10 毫秒。"
	}
	if *current.P95 <= target {
		return "met", "复测 P95 已达到目标。"
	}
	return "missed", "复测 P95 仍高于目标。"
}

func (s *Postgres) SaveLogRetest(ctx context.Context, investigation string, input LogRetestInput) (any, error) {
	if input.BaselineBatchID == input.CurrentBatchID || input.TargetP95MS <= 0 || input.TargetP95MS > 3600000 || math.IsNaN(input.TargetP95MS) {
		return nil, ErrLogInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var primary, instance, fingerprint, database, application string
	var event *string
	err = tx.QueryRow(ctx, `SELECT r.id::text,i.id::text,r.fingerprint,r.database_name,r.application,k.id::text
		FROM log_investigations q JOIN log_instances i ON i.id=q.instance_id JOIN log_projects p ON p.id=i.project_id
		JOIN log_records r ON r.id=q.primary_record
		LEFT JOIN assets a ON a.id=i.asset_id AND a.organization_id=p.organization_id
		LEFT JOIN event_keys k ON k.id=q.event_id AND k.asset_id=a.id
		WHERE q.id=$1 AND ($2='' OR p.organization_id::text=$2) AND r.kind='statement' AND r.fingerprint<>''`,
		investigation, OrganizationFromContext(ctx)).Scan(&primary, &instance, &fingerprint, &database, &application, &event)
	if err != nil {
		return nil, logWriteError(err)
	}
	batchIDs := [2]string{input.BaselineBatchID, input.CurrentBatchID}
	order := [2]int{0, 1}
	if batchIDs[0] > batchIDs[1] {
		order = [2]int{1, 0}
	}
	var summaries [2]logBatchSummary
	var samples [2]logRetestSample
	var measures [2]logCompareMeasure
	var revisions [2]int64
	for _, index := range order {
		batch := batchIDs[index]
		summary, err := loadLogBatchRollup(ctx, tx, batch, false)
		if err != nil {
			return nil, err
		}
		if summary.InstanceID != instance {
			return nil, ErrLogNotFound
		}
		summaries[index] = summary
		query := `SELECT b.archive_revision,r.id::text FROM log_investigation_records j
			JOIN log_records r ON r.id=j.record_id JOIN log_batches b ON b.id=r.batch_id
			WHERE j.investigation_id=$1 AND b.id=$2 AND r.fingerprint=$3 AND r.database_name=$4 AND r.application=$5
			ORDER BY (r.id=$6) DESC,r.observed_at DESC NULLS LAST,r.id LIMIT 1`
		err = tx.QueryRow(ctx, query, investigation, batch, fingerprint, database, application, primary).
			Scan(&revisions[index], &samples[index].RecordID)
		if err != nil {
			return nil, logWriteError(err)
		}
		measure := &measures[index]
		err = tx.QueryRow(ctx, `SELECT calls,timed,slow,p95 FROM log_issue_rollups
			WHERE batch_id=$1 AND kind='statement' AND issue=$2 AND database_name=$3 AND application=$4`,
			batch, fingerprint, database, application).Scan(&measure.Count, &measure.Timed, &measure.Slow, &measure.P95)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		samples[index].BatchID, samples[index].Timed, samples[index].P95 = batch, measure.Timed, measure.P95
	}
	reason := logComparisonCoverage(summaries[0], summaries[1])
	if summaries[0].From != nil && summaries[1].From != nil && summaries[1].From.Before(*summaries[0].From) {
		return nil, ErrLogInput
	}
	status, reason := logRetestDecision(reason, measures[0], measures[1], input.TargetP95MS)
	result := map[string]any{"status": status, "reason": reason, "caseId": investigation, "eventId": event,
		"baseline": samples[0], "current": samples[1], "targetP95Ms": input.TargetP95MS}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var id string
	var created time.Time
	err = tx.QueryRow(ctx, `INSERT INTO log_retests(investigation_id,baseline,current_batch,baseline_revision,current_revision,target_ms,result)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING id::text,created_at`, investigation,
		batchIDs[0], batchIDs[1], revisions[0], revisions[1], input.TargetP95MS, payload).Scan(&id, &created)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `SELECT id::text,created_at,result FROM log_retests
			WHERE investigation_id=$1 AND baseline=$2 AND current_batch=$3 AND baseline_revision=$4 AND current_revision=$5 AND target_ms=$6`,
			investigation, batchIDs[0], batchIDs[1], revisions[0], revisions[1], input.TargetP95MS).Scan(&id, &created, &payload)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &result); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case event != nil:
		payload, err := json.Marshal(map[string]any{"retestId": id, "caseId": investigation, "code": status,
			"targetP95Ms": input.TargetP95MS, "currentP95Ms": samples[1].P95, "batchId": samples[1].BatchID,
			"recordId": samples[1].RecordID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'log_retest',$2,$3)`, *event, created, payload); err != nil {
			return nil, err
		}
	}
	result["id"], result["createdAt"] = id, created
	return result, tx.Commit(ctx)
}
