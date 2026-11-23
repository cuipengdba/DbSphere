package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

var (
	ErrWaitCaptureBusy     = errors.New("wait capture limit reached")
	ErrWaitCaptureTarget   = errors.New("wait capture target unavailable")
	ErrWaitCaptureNotFound = errors.New("wait capture not found")
)

func migrateWaitCaptures(ctx context.Context, tx pgx.Tx) error {
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dbsphere_schema_migrations WHERE version=37)`).Scan(&ready); err != nil {
		return fmt.Errorf("check wait capture schema: %w", err)
	}
	if ready {
		return nil
	}
	_, err := tx.Exec(ctx, `CREATE TABLE wait_captures (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		event_id uuid REFERENCES event_keys(id) ON DELETE CASCADE, started_at timestamptz NOT NULL, payload jsonb NOT NULL
	);
	CREATE INDEX wait_captures_asset_time ON wait_captures(asset_id,started_at DESC,id);
	CREATE INDEX wait_captures_event_time ON wait_captures(event_id,started_at DESC,id) WHERE event_id IS NOT NULL;
	INSERT INTO dbsphere_schema_migrations(version) VALUES(37)`)
	if err != nil {
		return fmt.Errorf("create wait capture schema: %w", err)
	}
	return nil
}

func (s *Postgres) RunWaitCapture(ctx context.Context, assetID, eventID, profile string, key []byte) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1742027)`).Scan(&acquired); err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrWaitCaptureBusy
	}
	var target collector.Target
	var encrypted string
	err = tx.QueryRow(ctx, `SELECT host,port,database_name,username,ssl_mode,password_enc FROM assets WHERE id=$1 AND runtime<>'mysql'`, assetID).
		Scan(&target.Host, &target.Port, &target.Database, &target.Username, &target.SSLMode, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	if eventID != "" {
		var open bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_keys k JOIN events e ON e.id=k.id AND e.started_at=k.started_at
			WHERE k.id=$1 AND k.asset_id=$2 AND e.status='open')`, eventID, assetID).Scan(&open)
		if err != nil {
			return nil, err
		}
		if !open {
			return nil, ErrEventNotFound
		}
	}
	var limited bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wait_captures WHERE asset_id=$1 AND started_at>now()-interval '1 minute')
		OR (SELECT count(*) FROM wait_captures WHERE event_id=nullif($2,'')::uuid)>=5`, assetID, eventID).Scan(&limited)
	if err != nil {
		return nil, err
	}
	if limited {
		return nil, ErrWaitCaptureBusy
	}
	password, err := security.Decrypt(key, encrypted)
	if err != nil {
		return nil, ErrWaitCaptureTarget
	}
	capture, err := collector.CaptureWaits(ctx, target, password, profile)
	if err != nil {
		return nil, ErrWaitCaptureTarget
	}
	encoded, err := json.Marshal(capture)
	if err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO wait_captures(asset_id,event_id,started_at,payload)
		VALUES($1,nullif($2,'')::uuid,$3,$4) RETURNING id::text`, assetID, eventID, capture.StartedAt, encoded).Scan(&id)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM wait_captures WHERE id IN (
		SELECT id FROM wait_captures WHERE asset_id=$1 AND event_id IS NULL ORDER BY started_at DESC,id OFFSET 20)`, assetID); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "eventId": eventID, "result": capture}, nil
}

const waitCaptureSummary = `jsonb_build_object('id',id,'eventId',event_id) || (payload-'frames')`

func (s *Postgres) waitCaptures(ctx context.Context, assetID, id string) (any, error) {
	if id == "" {
		captures, err := s.queryJSON(ctx, `SELECT `+waitCaptureSummary+` FROM wait_captures WHERE asset_id=$1
			ORDER BY started_at DESC,id LIMIT 20`, assetID)
		return map[string]any{"captures": captures}, err
	}
	if !evidenceUUID.MatchString(id) {
		return nil, ErrWaitCaptureNotFound
	}
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',id,'eventId',event_id,'result',payload)
		FROM wait_captures WHERE id=$1 AND asset_id=$2`, id, assetID).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWaitCaptureNotFound
	}
	return result, err
}

func (s *Postgres) EventWaitCaptures(ctx context.Context, eventID string) ([]json.RawMessage, error) {
	return s.queryJSON(ctx, `SELECT `+waitCaptureSummary+` FROM wait_captures WHERE event_id=$1 ORDER BY started_at DESC,id`, eventID)
}
