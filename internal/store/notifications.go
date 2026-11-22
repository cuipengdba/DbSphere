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
	ID             string          `json:"deliveryId"`
	Channel        string          `json:"-"`
	Kind           string          `json:"kind"`
	ObservedAt     time.Time       `json:"observedAt"`
	EventID        string          `json:"eventId"`
	EventTitle     string          `json:"eventTitle"`
	EventStatus    string          `json:"eventStatus"`
	AssetID        string          `json:"assetId"`
	AssetName      string          `json:"assetName"`
	Payload        json.RawMessage `json:"-"`
	OrganizationID string          `json:"-"`
}

// ConfigureOperationsWebhook is called before starting the collector.
func (s *Postgres) ConfigureOperationsWebhook(organization string) {
	s.operationsWebhookOrganization = organization
}

func (s *Postgres) queueGoNotifications(ctx context.Context, tx pgx.Tx, evidenceID string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO notification_outbox(id,evidence_id,channel,next_attempt_at)
		SELECT gen_random_uuid(),source.id,route.channel,$2
		FROM evidence source JOIN event_keys target ON target.id=source.event_id
		JOIN assets a ON a.id=target.asset_id
		CROSS JOIN unnest(ARRAY['webhook','email','operations_webhook']) AS route(channel)
		WHERE source.id=$1 AND (route.channel<>'operations_webhook' OR a.organization_id=$3::uuid)
		AND NOT EXISTS (
			SELECT FROM evidence prior JOIN notification_outbox queued ON queued.evidence_id=prior.id
			WHERE prior.event_id=source.event_id AND queued.channel=route.channel
			AND ((source.kind='alert_recovered' AND prior.kind='alert_recovered')
				OR (source.kind IN ('alert_firing','alert_unsuppressed') AND prior.kind IN ('alert_firing','alert_unsuppressed')
					AND source.payload_json->>'ruleId'=prior.payload_json->>'ruleId')))
		ON CONFLICT(evidence_id,channel) DO NOTHING`, evidenceID, now, nullable(s.operationsWebhookOrganization))
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
			v.id::text,v.title,v.status,a.id::text,a.name,e.payload_json,a.organization_id::text
			FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
			JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
			WHERE n.status='pending' AND n.next_attempt_at<=now()
			AND (e.kind<>'alert_recovered' OR NOT EXISTS (
				SELECT FROM evidence original JOIN notification_outbox previous ON previous.evidence_id=original.id
				WHERE original.event_id=v.id AND original.kind IN ('alert_firing','alert_unsuppressed')
				AND previous.channel=n.channel AND previous.status='pending'))
			ORDER BY n.next_attempt_at,n.id LIMIT 1 FOR UPDATE OF n SKIP LOCKED`).
			Scan(&item.ID, &item.Channel, &attempts, &item.Kind, &item.ObservedAt,
				&item.EventID, &item.EventTitle, &item.EventStatus, &item.AssetID,
				&item.AssetName, &item.Payload, &item.OrganizationID)
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
		status := outcome
		if sendErr == nil {
			_, err = tx.Exec(ctx, `UPDATE notification_outbox SET status=$1,
				delivered_at=now(),last_error=NULL WHERE id=$2`, outcome, item.ID)
		} else {
			attempts++
			backoff := 30 * time.Second << min(attempts-1, 7)
			var retryAfter interface{ RetryAfter() time.Duration }
			if errors.As(sendErr, &retryAfter) {
				backoff = max(backoff, retryAfter.RetryAfter())
			}
			backoff = min(backoff, time.Hour)
			status = "pending"
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
		if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'notification_delivery',now(),
				jsonb_build_object('deliveryId',$2::text,'channel',$3::text,'status',$4::text,'kind',$5::text))`,
			item.EventID, item.ID, item.Channel, status, item.Kind); err != nil {
			_ = tx.Rollback(ctx)
			return processed, fmt.Errorf("record notification delivery: %w", err)
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
	result, err := s.pool.Exec(ctx, `WITH retried AS (
		UPDATE notification_outbox SET status='pending',attempts=0,next_attempt_at=now(),last_error=NULL
		WHERE id=$1 AND status='failed' RETURNING id,evidence_id,channel
	) INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
	SELECT gen_random_uuid(),e.event_id,'notification_retry',now(),
		jsonb_build_object('deliveryId',r.id,'channel',r.channel,'status','pending')
	FROM retried r JOIN evidence e ON e.id=r.evidence_id`, id)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

// QueueCriticalEscalations adds one escalation per rule and open event after the
// acknowledgement window. It checks current silences even when no new sample arrived.
func (s *Postgres) QueueCriticalEscalations(ctx context.Context, now time.Time, afterMinutes int) (int, error) {
	if afterMinutes < 1 || afterMinutes > 1440 {
		return 0, errors.New("invalid escalation delay")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT s.asset_id::text,s.rule_id,s.state_json,r.definition_json
		FROM alert_states s JOIN alert_rules r ON r.id=s.rule_id
		WHERE s.state_json->>'status'='firing'`)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		assetID string
		ruleID  string
		state   alertState
		rule    alertRule
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		var stateJSON, ruleJSON []byte
		if err := rows.Scan(&item.assetID, &item.ruleID, &stateJSON, &ruleJSON); err != nil {
			rows.Close()
			return 0, err
		}
		if json.Unmarshal(stateJSON, &item.state) == nil && json.Unmarshal(ruleJSON, &item.rule) == nil &&
			item.rule.Enabled && item.rule.Severity == "critical" && item.state.MutedBy == nil &&
			item.state.EventID != "" {
			candidates = append(candidates, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	queued := 0
	for _, item := range candidates {
		var eventID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM events WHERE id=$1 AND status='open'
			AND acknowledged_at IS NULL AND started_at<=$2 FOR UPDATE`,
			item.state.EventID, now.Add(-time.Duration(afterMinutes)*time.Minute)).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		var suppressed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM silences
			WHERE revoked_at IS NULL AND starts_at<=$1 AND ends_at>$1
			AND (asset_id IS NULL OR asset_id=$2) AND (rule_id IS NULL OR rule_id=$3))`,
			now, item.assetID, item.ruleID).Scan(&suppressed); err != nil {
			return 0, err
		}
		if suppressed {
			continue
		}
		var already bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence WHERE event_id=$1
			AND kind='alert_escalation' AND payload_json->>'ruleId'=$2)`,
			eventID, item.ruleID).Scan(&already); err != nil {
			return 0, err
		}
		if already {
			continue
		}
		payload, _ := json.Marshal(map[string]any{"ruleId": item.ruleID,
			"severity": item.rule.Severity, "source": item.rule.Source,
			"value": item.state.LastValue, "afterMinutes": afterMinutes})
		var evidenceID string
		if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'alert_escalation',$2,$3) RETURNING id::text`,
			eventID, now, payload).Scan(&evidenceID); err != nil {
			return 0, err
		}
		if err := s.queueGoNotifications(ctx, tx, evidenceID, now); err != nil {
			return 0, err
		}
		queued++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return queued, nil
}
