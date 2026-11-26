package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/jackc/pgx/v5"
)

var ErrOnboardingBusy = errors.New("another onboarding check is in progress")
var ErrOnboardingNotFound = errors.New("onboarding batch not found")

const onboardingCheckLimit = 5

func migrateAssetOnboarding(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=63)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE asset_onboarding_batches (
		id uuid PRIMARY KEY, request_digest text NOT NULL, plan jsonb NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now());
		CREATE INDEX asset_onboarding_batches_recent ON asset_onboarding_batches(created_at DESC,id DESC);
		CREATE TABLE asset_onboarding_items (
		batch_id uuid NOT NULL REFERENCES asset_onboarding_batches(id) ON DELETE CASCADE,
		asset_id uuid NOT NULL, position integer NOT NULL, name text NOT NULL, runtime text NOT NULL,
		first_report jsonb, latest_report jsonb, last_attempt_at timestamptz,
		PRIMARY KEY(batch_id,asset_id), UNIQUE(batch_id,position));
		INSERT INTO dbsphere_schema_migrations(version) VALUES(63)`)
	return err
}

func recordOnboardingBatch(ctx context.Context, tx pgx.Tx, plan importPlan, digest string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO asset_onboarding_batches(id,request_digest,plan) VALUES($1,$2,$3)`, plan.BatchID, digest, plan); err != nil {
		return err
	}
	// The input is bounded by ParseImport and this transaction also saves the assets.
	for _, row := range plan.Rows {
		if _, err := tx.Exec(ctx, `INSERT INTO asset_onboarding_items(batch_id,asset_id,position,name,runtime)
			VALUES($1,$2,$3,$4,$5)`, plan.BatchID, row.ID, row.Index, row.Name, row.Runtime); err != nil {
			return err
		}
	}
	return nil
}

func (s *Postgres) OnboardingBatches(ctx context.Context, after string) (any, error) {
	if after != "" && !evidenceUUID.MatchString(after) {
		return nil, assets.ErrInvalidImport
	}
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',b.id,'createdAt',b.created_at,
		'assetCount',jsonb_array_length(b.plan->'rows')) FROM asset_onboarding_batches b
		WHERE $1::uuid IS NULL OR (b.created_at,b.id)<(SELECT created_at,id FROM asset_onboarding_batches WHERE id=$1)
		ORDER BY b.created_at DESC,b.id DESC LIMIT 21`, nullable(after))
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	var next struct{ ID string }
	if len(rows) > 20 {
		rows = rows[:20]
		if err := json.Unmarshal(rows[19], &next); err != nil {
			return nil, err
		}
	}
	return map[string]any{"batches": rows, "next": next.ID}, nil
}

func (s *Postgres) OnboardingBatch(ctx context.Context, id string) (any, error) {
	if !evidenceUUID.MatchString(id) {
		return nil, assets.ErrInvalidImport
	}
	var batch json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',b.id,'createdAt',b.created_at,'items',
		(SELECT jsonb_agg(jsonb_strip_nulls(jsonb_build_object('assetId',i.asset_id,'name',coalesce(a.name,i.name),
			'runtime',i.runtime,'available',a.id IS NOT NULL,'firstReport',i.first_report,'latestReport',i.latest_report,
			'lastAttemptAt',i.last_attempt_at)) ORDER BY i.position)
		 FROM asset_onboarding_items i LEFT JOIN assets a ON a.id=i.asset_id WHERE i.batch_id=b.id))
		FROM asset_onboarding_batches b WHERE b.id=$1`, id).Scan(&batch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOnboardingNotFound
	}
	return batch, err
}

func (s *Postgres) CheckOnboardingBatch(ctx context.Context, id string, ids []string, key []byte) (any, error) {
	if !evidenceUUID.MatchString(id) || len(ids) < 1 || len(ids) > onboardingCheckLimit {
		return nil, assets.ErrInvalidImport
	}
	seen := make(map[string]bool, len(ids))
	for _, assetID := range ids {
		if !evidenceUUID.MatchString(assetID) || seen[assetID] {
			return nil, assets.ErrInvalidImport
		}
		seen[assetID] = true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	// Separate from periodic collection and shared by all application replicas.
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1742026, 63)`).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrOnboardingBusy
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_onboarding_items i JOIN assets a ON a.id=i.asset_id
		WHERE i.batch_id=$1 AND i.asset_id=ANY($2::uuid[])`, id, ids).Scan(&count); err != nil {
		return nil, err
	}
	if count != len(ids) {
		return nil, ErrOnboardingNotFound
	}

	reports := make([]*readinessReport, len(ids))
	jobs := make(chan int, len(ids))
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(len(ids), 2) {
		workers.Go(func() {
			for i := range jobs {
				reports[i] = s.checkOnboardingAsset(ctx, ids[i], key)
			}
		})
	}
	workers.Wait()
	for i, assetID := range ids {
		var report any
		if reports[i] != nil {
			report = reports[i]
		}
		if _, err := tx.Exec(ctx, `UPDATE asset_onboarding_items SET first_report=coalesce(first_report,$3::jsonb),
			latest_report=$3::jsonb,last_attempt_at=now() WHERE batch_id=$1 AND asset_id=$2`, id, assetID, report); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.OnboardingBatch(ctx, id)
}

func (s *Postgres) checkOnboardingAsset(ctx context.Context, id string, key []byte) *readinessReport {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	result, err := s.CheckAsset(ctx, id, key)
	if err != nil {
		return nil
	}
	report, err := s.assetReadiness(ctx, id, &result.CollectedAt)
	if err != nil || report.SampleID != result.SampleID {
		return nil
	}
	return &report
}
