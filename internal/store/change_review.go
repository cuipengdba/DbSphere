package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type changeSnapshot struct {
	SampleID    string    `json:"sampleId"`
	At          time.Time `json:"at"`
	Risk        string    `json:"risk"`
	Connections *float64  `json:"connections"`
	LockWaits   *float64  `json:"lockWaits"`
	SQLCoverage string    `json:"sqlCoverage"`
	OpenEvents  int       `json:"openEvents"`
}

func changeSnapshotFromSample(id string, at time.Time, status string, raw []byte) (changeSnapshot, error) {
	point := changeSnapshot{SampleID: id, At: at, Risk: "limited", SQLCoverage: "unavailable"}
	if status != "ok" {
		point.Risk = "failed"
		return point, nil
	}
	var metrics map[string]any
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return point, err
	}
	adapter := object(metrics["adapter"])
	if object(adapter["transport"])["readOnly"] != true {
		return point, nil
	}
	if adapter["engine"] == "mysql" {
		data := object(metrics["mysql"])
		if value, ok := numeric(data["connections"]); ok {
			point.Connections = &value
		}
		for _, value := range list(adapter["signals"]) {
			signal := object(value)
			if signal["status"] != "ok" {
				continue
			}
			switch signal["key"] {
			case "lockWaits":
				if waits, ok := data["lockWaits"].([]any); ok {
					count := float64(len(waits))
					point.LockWaits = &count
				}
			case "sqlPerformance":
				point.SQLCoverage = "available"
			}
		}
	} else {
		if value, ok := numeric(metrics["connections"]); ok {
			point.Connections = &value
		}
		if signal := readGoSignal("pg.blocked_sessions", metrics, at); signal.Available {
			point.LockWaits = &signal.Value
		}
		if object(metrics["sqlPerformance"])["status"] == "ok" {
			point.SQLCoverage = "available"
		}
	}
	if point.LockWaits != nil && *point.LockWaits > 0 {
		point.Risk = "lock_wait"
	} else if point.LockWaits != nil && point.Connections != nil {
		point.Risk = "ready"
	}
	return point, nil
}

func (s *Postgres) CheckChange(ctx context.Context, assetID, changeID, phase string, in ChangeReview, key []byte) (any, error) {
	if !evidenceUUID.MatchString(assetID) || !evidenceUUID.MatchString(changeID) {
		return nil, ErrInvalidChange
	}
	in.Note = strings.TrimSpace(in.Note)
	if phase == "baseline" {
		if in.Note != "" || in.EventID != "" {
			return nil, ErrInvalidChange
		}
	} else if (phase != "review" && phase != "recovery") || !validChangeText(in.Note, 500) ||
		(in.EventID != "" && !evidenceUUID.MatchString(in.EventID)) {
		return nil, ErrInvalidChange
	}
	org := OrganizationFromContext(ctx)
	var runtime, status string
	err := s.pool.QueryRow(ctx, `SELECT a.runtime,c.status FROM change_orders c JOIN assets a ON a.id=c.asset_id
		AND a.organization_id=c.organization_id WHERE c.id=$1 AND a.id=$2 AND ($3::uuid IS NULL OR c.organization_id=$3)`,
		changeID, assetID, nullable(org)).Scan(&runtime, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	if runtime != "mysql" && runtime != "linux" && runtime != "managed" && runtime != "k8s" {
		return nil, ErrInvalidChange
	}
	if !changeCheckAllowed(phase, status) {
		return nil, ErrChangeState
	}
	result, err := s.CheckAsset(ctx, assetID, key)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	change, err := lockChangeOrder(ctx, tx, changeID, assetID, org)
	if err != nil {
		return nil, err
	}
	if !changeCheckAllowed(phase, change.status) || change.revision != change.currentRevision {
		return nil, ErrChangeState
	}
	var sampleStatus string
	var metrics []byte
	if err := tx.QueryRow(ctx, `SELECT status,metrics_json FROM samples WHERE asset_id=$1 AND id=$2 AND collected_at=$3`,
		assetID, result.SampleID, result.CollectedAt).Scan(&sampleStatus, &metrics); err != nil {
		return nil, err
	}
	snapshot, err := changeSnapshotFromSample(result.SampleID, result.CollectedAt, sampleStatus, metrics)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE asset_id=$1 AND started_at<=$2
		AND (ended_at IS NULL OR ended_at>$2)`, assetID, result.CollectedAt).Scan(&snapshot.OpenEvents); err != nil {
		return nil, err
	}
	if phase == "baseline" {
		if !change.now.Before(change.endsAt) {
			return nil, ErrChangeState
		}
		encoded, _ := json.Marshal(snapshot)
		_, err = tx.Exec(ctx, `UPDATE change_orders SET baseline=$2 WHERE id=$1`, changeID, encoded)
	} else {
		err = saveChangeReview(ctx, tx, changeID, phase, change, in, snapshot)
	}
	if err != nil {
		return nil, err
	}
	return snapshot, tx.Commit(ctx)
}

func changeCheckAllowed(phase, state string) bool {
	switch phase {
	case "baseline":
		return state == "requested" || state == "approved"
	case "review":
		return state == "succeeded" || state == "failed"
	case "recovery":
		return state == "rolled_back"
	}
	return false
}

func saveChangeReview(ctx context.Context, tx pgx.Tx, id, phase string, change changeStateRow, review ChangeReview, sample changeSnapshot) error {
	if (phase == "review" && (change.finishedAt == nil || sample.At.Before(*change.finishedAt))) ||
		(phase == "recovery" && (change.rolledBackAt == nil || sample.At.Before(*change.rolledBackAt))) {
		return ErrChangeState
	}
	eventID := review.EventID
	if change.eventID != nil {
		if eventID != "" && eventID != *change.eventID {
			return ErrChangeState
		}
		eventID = *change.eventID
	}
	if eventID != "" {
		var sameAsset bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM events WHERE id=$1 AND asset_id=$2
			AND started_at>=$3 AND started_at<=$4)`, eventID, change.assetID, change.startedAt, sample.At).Scan(&sameAsset); err != nil {
			return err
		}
		if !sameAsset {
			return ErrChangeState
		}
	}
	actor := accessActor(ctx, "admin")
	encoded, _ := json.Marshal(sample)
	if _, err := tx.Exec(ctx, `UPDATE change_orders SET
		after_sample=CASE WHEN $2='review' THEN $3::jsonb ELSE after_sample END,
		recovery_sample=CASE WHEN $2='recovery' THEN $3::jsonb ELSE recovery_sample END,
		review_note=CASE WHEN $2='review' THEN $4 ELSE review_note END,
		review_actor=CASE WHEN $2='review' THEN $5 ELSE review_actor END,
		review_at=CASE WHEN $2='review' THEN $6 ELSE review_at END,
		recovery_note=CASE WHEN $2='recovery' THEN $4 ELSE recovery_note END,
		recovery_actor=CASE WHEN $2='recovery' THEN $5 ELSE recovery_actor END,
		recovery_at=CASE WHEN $2='recovery' THEN $6 ELSE recovery_at END,event_id=$7 WHERE id=$1`,
		id, phase, encoded, review.Note, actor, change.now, nullable(eventID)); err != nil {
		return err
	}
	if eventID == "" {
		return nil
	}
	if change.eventID == nil {
		var receiptHistory []byte
		if err := tx.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object('receiptId',receipt_id,'state',state,'receivedAt',received_at)
			ORDER BY received_at,receipt_id) FROM change_receipts WHERE change_id=$1`, id).Scan(&receiptHistory); err != nil {
			return err
		}
		if err := insertChangeEvidence(ctx, tx, eventID, "change_receipt", change.now, map[string]any{
			"changeId": id, "externalId": change.externalID, "receipts": json.RawMessage(receiptHistory),
		}); err != nil {
			return err
		}
	}
	return insertChangeEvidence(ctx, tx, eventID, "change_review", change.now, map[string]any{
		"changeId": id, "externalId": change.externalID, "sampleId": sample.SampleID, "baseline": change.baseline,
		"phase": phase, "note": review.Note, "actor": actor, "risk": sample.Risk,
	})
}

func insertChangeEvidence(ctx context.Context, tx pgx.Tx, eventID, kind string, at time.Time, payload map[string]any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4)`, eventID, kind, at, encoded)
	return err
}
