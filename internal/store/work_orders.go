package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidWorkOrder = errors.New("invalid work order")
var ErrWorkOrderConflict = errors.New("work order is not in the required state or has expired")

const workOrderFields = `jsonb_build_object('id',id,'eventId',event_id,'assetId',asset_id,
	'action',action,'reason',reason,'status',CASE WHEN status IN ('requested','approved','job_requested','job_approved')
	AND expires_at<=now() THEN 'expired' WHEN status='executing' AND executing_deadline<=now()-interval '5 seconds'
	THEN 'interrupted' WHEN status='job_requested' THEN 'requested' WHEN status='job_approved' THEN 'approved' ELSE status END,'requestedBy',requested_by,
	'approvedBy',approved_by,'requestedAt',requested_at,'expiresAt',expires_at,
	'approvedAt',approved_at,'executedAt',executed_at,'result',result_json,
	'plan',plan_json - 'targetRevision' - 'profileRevision','timeoutSeconds',timeout_seconds)`

const workOrderJSON = "SELECT " + workOrderFields + " FROM work_orders WHERE id=$1"

func workOrderByID(ctx context.Context, tx pgx.Tx, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, workOrderJSON, id).Scan(&raw)
	return raw, err
}

// CreateWorkOrder only plans a read-only collection for an open incident. The
// request itself never connects to the monitored PostgreSQL target.
func (s *Postgres) CreateWorkOrder(ctx context.Context, eventID, reason string) (json.RawMessage, error) {
	return s.CreatePlannedWorkOrder(ctx, eventID, WorkOrderInput{Reason: reason}, nil)
}

const workOrderRequestLookup = `SELECT ` + workOrderFields + `,coalesce(submission_json=$4::jsonb,false)
	FROM work_orders WHERE event_id=$1 AND requested_by=$2 AND idempotency_key=$3`

func readWorkOrderRequest(row pgx.Row) (json.RawMessage, error) {
	var raw json.RawMessage
	var same bool
	err := row.Scan(&raw, &same)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err == nil && !same {
		return nil, ErrWorkOrderConflict
	}
	return raw, err
}

func (s *Postgres) ExistingWorkOrder(ctx context.Context, eventID string, in WorkOrderInput) (json.RawMessage, error) {
	if in.IdempotencyKey == "" {
		return nil, nil
	}
	submitted, _ := json.Marshal(in)
	return readWorkOrderRequest(s.pool.QueryRow(ctx, workOrderRequestLookup, eventID,
		accessActor(ctx, "admin"), in.IdempotencyKey, submitted))
}

func (s *Postgres) CreatePlannedWorkOrder(ctx context.Context, eventID string, in WorkOrderInput, plan *WorkOrderPlan) (json.RawMessage, error) {
	in, err := NormalizeWorkOrder(in)
	if err != nil || !evidenceUUID.MatchString(eventID) || in.Reason == "" ||
		plan != nil && plan.Action != in.Action || plan == nil && in.Action != WorkCollect {
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
	actor := accessActor(ctx, "admin")
	submitted, _ := json.Marshal(in)
	if in.IdempotencyKey != "" {
		found, err := readWorkOrderRequest(tx.QueryRow(ctx, workOrderRequestLookup, eventID, actor, in.IdempotencyKey, submitted))
		if err != nil || found != nil {
			return found, err
		}
	}
	if eventStatus != "open" || plan != nil && plan.AssetID != assetID {
		return nil, ErrWorkOrderConflict
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders WHERE event_id=$1
		AND ((status IN ('requested','approved','job_requested','job_approved') AND expires_at>now()) OR
		(status='executing' AND coalesce(executing_deadline,expires_at)>now()-interval '5 seconds'))`, eventID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending >= 3 {
		return nil, ErrWorkOrderConflict
	}
	var id string
	var rawPlan []byte
	status := "requested"
	if plan != nil {
		rawPlan, _ = json.Marshal(plan)
		// A rolled-back server only recognizes legacy requested and approved rows.
		status = "job_requested"
	}
	err = tx.QueryRow(ctx, `INSERT INTO work_orders(id,event_id,asset_id,action,reason,status,
		requested_by,expires_at,plan_json,submission_json,idempotency_key,timeout_seconds)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,now()+make_interval(mins=>$7),
		$8,$9,nullif($10,''),$11) RETURNING id::text`, eventID, assetID, in.Action, in.Reason, status, actor,
		in.ExpiresMinutes, rawPlan, submitted, in.IdempotencyKey, in.TimeoutSeconds).Scan(&id)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"workOrderId": id, "action": in.Action,
		"reason": in.Reason, "actor": actor, "assetId": assetID})
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
	rows, err := s.pool.Query(ctx, `SELECT `+workOrderFields+`
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
	err = tx.QueryRow(ctx, `UPDATE work_orders SET status=CASE WHEN status='job_requested' AND $2::text='approved'
		THEN 'job_approved' ELSE $2 END,approved_by=$3,approved_at=now()
		WHERE id=$1 AND status IN ('requested','job_requested') AND expires_at>now() AND requested_by<>$3
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
	task, err := s.StartWorkPlan(ctx, id)
	return task.EventID, task.AssetID, err
}

func (s *Postgres) FinishWorkOrder(ctx context.Context, id string, result CheckResult, operationErr error) (json.RawMessage, error) {
	if !evidenceUUID.MatchString(id) {
		return nil, ErrInvalidWorkOrder
	}
	status := "completed"
	if operationErr != nil || result.Status != "ok" {
		status = "failed"
	}
	return s.FinishWorkPlan(ctx, id, status, map[string]any{"sampleId": result.SampleID, "sampleStatus": result.Status})
}

func (s *Postgres) FinishWorkPlan(ctx context.Context, id, status string, result map[string]any) (json.RawMessage, error) {
	result["status"], result["finishedAt"] = status, time.Now().UTC()
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var eventID, action string
	err = tx.QueryRow(ctx, `UPDATE work_orders SET status=$2,executed_at=now(),result_json=$3
		WHERE id=$1 AND status='executing' RETURNING event_id::text,action`, id, status, payload).Scan(&eventID, &action)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkOrderConflict
	}
	if err != nil {
		return nil, err
	}
	result["workOrderId"], result["actor"], result["action"] = id, accessActor(ctx, "admin"), action
	evidence, _ := json.Marshal(result)
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
