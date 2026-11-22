package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidWorkOrder = errors.New("invalid work order")
var ErrWorkOrderConflict = errors.New("work order is not in the required state or has expired")

const workOrderJSON = `SELECT jsonb_build_object('id',id,'eventId',event_id,'assetId',asset_id,
	'action',action,'reason',reason,'status',CASE WHEN status IN ('requested','approved')
	AND expires_at<=now() THEN 'expired' ELSE status END,'requestedBy',requested_by,
	'approvedBy',approved_by,'requestedAt',requested_at,'expiresAt',expires_at,
	'approvedAt',approved_at,'executedAt',executed_at,'result',result_json)
	FROM work_orders WHERE id=$1`

func workOrderByID(ctx context.Context, tx pgx.Tx, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, workOrderJSON, id).Scan(&raw)
	return raw, err
}

// CreateWorkOrder only plans a read-only collection for an open incident. The
// request itself never connects to the monitored PostgreSQL target.
func (s *Postgres) CreateWorkOrder(ctx context.Context, eventID, reason string) (json.RawMessage, error) {
	reason = strings.TrimSpace(reason)
	if !evidenceUUID.MatchString(eventID) || len([]rune(reason)) < 5 || len([]rune(reason)) > 300 {
		return nil, ErrInvalidWorkOrder
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var assetID, eventStatus string
	err = tx.QueryRow(ctx, `SELECT asset_id::text,status FROM events WHERE id=$1
		ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, eventID).Scan(&assetID, &eventStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	if eventStatus != "open" {
		return nil, ErrWorkOrderConflict
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE event_id=$1
		AND status IN ('requested','approved','executing') AND expires_at>now()`, eventID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending >= 3 {
		return nil, ErrWorkOrderConflict
	}
	var id string
	actor := accessActor(ctx, "admin")
	err = tx.QueryRow(ctx, `INSERT INTO work_orders(id,event_id,asset_id,action,reason,status,
		requested_by,expires_at) VALUES(gen_random_uuid(),$1,$2,'collect_once',$3,'requested',
		$4,now()+interval '15 minutes') RETURNING id::text`, eventID, assetID, reason, actor).Scan(&id)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"workOrderId": id, "action": "collect_once",
		"reason": reason, "actor": actor, "assetId": assetID})
	if _, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'work_order_requested',now(),$2)`, eventID, payload); err != nil {
		return nil, err
	}
	order, err := workOrderByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return order, tx.Commit(ctx)
}

func (s *Postgres) EventWorkOrders(ctx context.Context, eventID string) (any, error) {
	if !evidenceUUID.MatchString(eventID) {
		return nil, ErrInvalidWorkOrder
	}
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object('id',id,'eventId',event_id,
		'assetId',asset_id,'action',action,'reason',reason,
		'status',CASE WHEN status IN ('requested','approved') AND expires_at<=now()
		THEN 'expired' ELSE status END,'requestedBy',requested_by,'approvedBy',approved_by,
		'requestedAt',requested_at,'expiresAt',expires_at,'approvedAt',approved_at,
		'executedAt',executed_at,'result',result_json)
		FROM work_orders WHERE event_id=$1 ORDER BY requested_at DESC,id DESC LIMIT 50`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := jsonRows(rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{"orders": items}, nil
}

// DecideWorkOrder is reachable only by the separate approver session.
func (s *Postgres) DecideWorkOrder(ctx context.Context, id, decision string) (json.RawMessage, error) {
	if !evidenceUUID.MatchString(id) || (decision != "approved" && decision != "rejected") {
		return nil, ErrInvalidWorkOrder
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var eventID string
	actor := accessActor(ctx, "approver")
	err = tx.QueryRow(ctx, `UPDATE work_orders SET status=$2,approved_by=$3,
		approved_at=now() WHERE id=$1 AND status='requested' AND expires_at>now() AND requested_by<>$3
		RETURNING event_id::text`, id, decision, actor).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkOrderConflict
	}
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"workOrderId": id, "decision": decision, "actor": actor})
	if _, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'work_order_decision',now(),$2)`, eventID, payload); err != nil {
		return nil, err
	}
	order, err := workOrderByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return order, tx.Commit(ctx)
}

// BeginWorkOrder atomically consumes one approval. Concurrent execution
// attempts cannot run a second collection through this endpoint.
func (s *Postgres) BeginWorkOrder(ctx context.Context, id string) (string, string, error) {
	if !evidenceUUID.MatchString(id) {
		return "", "", ErrInvalidWorkOrder
	}
	var eventID, assetID string
	err := s.pool.QueryRow(ctx, `UPDATE work_orders SET status='executing' WHERE id=$1
		AND status='approved' AND expires_at>now()
		AND EXISTS(SELECT 1 FROM events e WHERE e.id=work_orders.event_id AND e.status='open')
		RETURNING event_id::text,asset_id::text`, id).Scan(&eventID, &assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrWorkOrderConflict
	}
	return eventID, assetID, err
}

func (s *Postgres) FinishWorkOrder(ctx context.Context, id string, result CheckResult, operationErr error) (json.RawMessage, error) {
	if !evidenceUUID.MatchString(id) {
		return nil, ErrInvalidWorkOrder
	}
	status := "completed"
	if operationErr != nil || result.Status != "ok" {
		status = "failed"
	}
	payload, _ := json.Marshal(map[string]any{"status": status, "sampleId": result.SampleID,
		"sampleStatus": result.Status, "finishedAt": time.Now().UTC()})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var eventID string
	err = tx.QueryRow(ctx, `UPDATE work_orders SET status=$2,executed_at=now(),result_json=$3
		WHERE id=$1 AND status='executing' RETURNING event_id::text`, id, status, payload).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkOrderConflict
	}
	if err != nil {
		return nil, err
	}
	evidence, _ := json.Marshal(map[string]any{"workOrderId": id, "actor": accessActor(ctx, "admin"),
		"status": status, "sampleId": result.SampleID, "sampleStatus": result.Status})
	if _, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'work_order_executed',now(),$2)`, eventID, evidence); err != nil {
		return nil, err
	}
	order, err := workOrderByID(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return order, tx.Commit(ctx)
}
