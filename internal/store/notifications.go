package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Notification struct {
	ID          string          `json:"deliveryId"`
	Channel     string          `json:"-"`
	Kind        string          `json:"kind"`
	ObservedAt  time.Time       `json:"observedAt"`
	EventID     string          `json:"eventId"`
	EventTitle  string          `json:"eventTitle"`
	EventStatus string          `json:"eventStatus"`
	AssetID     string          `json:"assetId"`
	AssetName   string          `json:"assetName"`
	Payload     json.RawMessage `json:"-"`
}

func queueGoNotifications(ctx context.Context, tx pgx.Tx, evidenceID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO notification_outbox(id,evidence_id,channel,next_attempt_at)
		SELECT gen_random_uuid(),$1,channel,$2 FROM unnest(ARRAY['webhook','email']) channel
		ON CONFLICT(evidence_id,channel) DO NOTHING`, evidenceID, now)
	return err
}

// DispatchDueNotifications holds each outbox row lock during the bounded send, so two
// server instances cannot deliver the same row concurrently. The receiver should be
// idempotent by deliveryId because network acknowledgement can be lost.
func (s *Postgres) DispatchDueNotifications(ctx context.Context, limit int,
	send func(context.Context, Notification) (string, error)) (int, error) {
	if limit < 1 || limit > 50 || send == nil {
		return 0, errors.New("invalid notification dispatch request")
	}
	processed := 0
	for processed < limit {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return processed, err
		}
		var item Notification
		var attempts int
		err = tx.QueryRow(ctx, `SELECT n.id::text,n.channel,n.attempts,e.kind,e.observed_at,
			v.id::text,v.title,v.status,a.id::text,a.name,e.payload_json
			FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
			JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
			WHERE n.status='pending' AND n.next_attempt_at<=now()
			ORDER BY n.next_attempt_at,n.id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`).
			Scan(&item.ID, &item.Channel, &attempts, &item.Kind, &item.ObservedAt,
				&item.EventID, &item.EventTitle, &item.EventStatus, &item.AssetID,
				&item.AssetName, &item.Payload)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			break
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return processed, fmt.Errorf("read due notification: %w", err)
		}
		sendCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		outcome, sendErr := send(sendCtx, item)
		cancel()
		if sendErr == nil && outcome != "delivered" && outcome != "skipped" {
			sendErr = errors.New("invalid notification outcome")
		}
		if sendErr == nil {
			_, err = tx.Exec(ctx, `UPDATE notification_outbox SET status=$1,
				delivered_at=now(),last_error=NULL WHERE id=$2`, outcome, item.ID)
		} else {
			attempts++
			backoff := 30 * time.Second << min(attempts-1, 7)
			if backoff > time.Hour {
				backoff = time.Hour
			}
			status := "pending"
			if attempts >= 5 {
				status = "failed"
			}
			reason := sendErr.Error()
			if len(reason) > 300 {
				reason = reason[:300]
			}
			_, err = tx.Exec(ctx, `UPDATE notification_outbox SET status=$1,attempts=$2,
				next_attempt_at=now()+($3::int * interval '1 second'),last_error=$4 WHERE id=$5`,
				status, attempts, int(backoff.Seconds()), reason, item.ID)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return processed, fmt.Errorf("update notification delivery: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return processed, fmt.Errorf("commit notification delivery: %w", err)
		}
		processed++
	}
	return processed, nil
}

func (s *Postgres) RetryNotification(ctx context.Context, id string) (bool, error) {
	if !silenceUUID.MatchString(id) {
		return false, nil
	}
	result, err := s.pool.Exec(ctx, `UPDATE notification_outbox SET status='pending',attempts=0,
		next_attempt_at=now(),last_error=NULL WHERE id=$1 AND status='failed'`, id)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}
