package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidEventResponse = errors.New("invalid event response")
var ErrRecoveryNotVerified = errors.New("recovery requires a successful post-resolution sample")

// RecordEventResponse records an operator's action without running commands on
// the monitored database. Recovery verification must cite an actual PG18 sample.
func (s *Postgres) RecordEventResponse(ctx context.Context, eventID, phase, note, sampleID string) error {
	note = strings.TrimSpace(note)
	if (phase != "diagnosis" && phase != "mitigation" && phase != "recovery_verification" && phase != "handoff" && phase != "false_positive") ||
		len([]rune(note)) < 5 || len([]rune(note)) > 500 ||
		(phase != "recovery_verification" && sampleID != "") ||
		(phase == "recovery_verification" && !evidenceUUID.MatchString(sampleID)) {
		return ErrInvalidEventResponse
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var assetID, status string
	var endedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT asset_id::text,status,ended_at FROM events
		WHERE id=$1 ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, eventID).Scan(&assetID, &status, &endedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEventNotFound
	}
	if err != nil {
		return err
	}
	if phase == "recovery_verification" {
		if status != "resolved" || endedAt == nil {
			return ErrRecoveryNotVerified
		}
		var sampleStatus string
		var collectedAt time.Time
		err := tx.QueryRow(ctx, `SELECT status,collected_at FROM samples
			WHERE asset_id=$1 AND id=$2 ORDER BY collected_at DESC LIMIT 1`, assetID, sampleID).
			Scan(&sampleStatus, &collectedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecoveryNotVerified
		}
		if err != nil {
			return err
		}
		if sampleStatus != "ok" || collectedAt.Before(*endedAt) {
			return ErrRecoveryNotVerified
		}
	}
	kind := "operator_response"
	if phase == "recovery_verification" {
		kind = "recovery_verified"
	}
	payload, err := json.Marshal(map[string]string{"phase": phase, "note": note, "actor": accessActor(ctx, "admin"), "sampleId": sampleID})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,now(),$3)`, eventID, kind, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
