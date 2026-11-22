package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	WorkCollect       = "collect_once"
	WorkInspect       = "inspect_once"
	WorkVerifyRestore = "verify_restore"
	WorkAnalyze       = "analyze_relation"
)

var jobNamePattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$")

type WorkOrderInput struct {
	Action         string
	Reason         string
	TemplateID     string
	Profile        string
	IdempotencyKey string
	ExpiresMinutes int
	TimeoutSeconds int
}

type WorkOrderPlan struct {
	Action          string `json:"action"`
	AssetID         string `json:"assetId"`
	TemplateID      string `json:"templateId,omitempty"`
	Profile         string `json:"profile,omitempty"`
	Description     string `json:"description"`
	TargetRevision  string `json:"targetRevision,omitempty"`
	ProfileRevision string `json:"profileRevision,omitempty"`
}

type WorkOrderTarget struct {
	ID       string
	Host     string
	Port     int
	Database string
	Username string
	Revision string
}

type WorkOrderExecution struct {
	EventID  string
	AssetID  string
	Plan     WorkOrderPlan
	Deadline time.Time
}

const workOrderTargetRevision = `encode(sha256(convert_to(jsonb_build_array(a.host,a.port,a.database_name,a.username,a.ssl_mode,a.password_enc)::text,'UTF8')),'hex')`

func migrateWorkPlans(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=45)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		ALTER TABLE work_orders DROP CONSTRAINT IF EXISTS work_orders_action_check;
		ALTER TABLE work_orders ADD CONSTRAINT work_orders_action_check
			CHECK(action IN ('collect_once','inspect_once','verify_restore','analyze_relation'));
		ALTER TABLE work_orders DROP CONSTRAINT IF EXISTS work_orders_status_check;
		ALTER TABLE work_orders ADD CONSTRAINT work_orders_status_check
			CHECK(status IN ('requested','approved','rejected','executing','completed','failed','job_requested','job_approved'));
		ALTER TABLE work_orders ADD COLUMN IF NOT EXISTS plan_json jsonb,
			ADD COLUMN IF NOT EXISTS submission_json jsonb,
			ADD COLUMN IF NOT EXISTS idempotency_key text,
			ADD COLUMN IF NOT EXISTS timeout_seconds integer NOT NULL DEFAULT 35,
			ADD COLUMN IF NOT EXISTS executing_deadline timestamptz;
		CREATE UNIQUE INDEX IF NOT EXISTS work_orders_request_key
			ON work_orders(event_id,requested_by,idempotency_key) WHERE idempotency_key IS NOT NULL;
		INSERT INTO dbsphere_schema_migrations(version) VALUES(45)`)
	return err
}

func NormalizeWorkOrder(in WorkOrderInput) (WorkOrderInput, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Action == "" {
		in.Action = WorkCollect
	}
	if in.ExpiresMinutes == 0 {
		in.ExpiresMinutes = 15
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 35
	}
	if (in.Reason != "" && (len([]rune(in.Reason)) < 5 || len([]rune(in.Reason)) > 300)) ||
		(in.IdempotencyKey != "" && !jobNamePattern.MatchString(in.IdempotencyKey)) ||
		in.ExpiresMinutes < 1 || in.ExpiresMinutes > 60 || in.TimeoutSeconds < 1 || in.TimeoutSeconds > 35 {
		return in, ErrInvalidWorkOrder
	}
	switch in.Action {
	case WorkCollect:
		if in.TemplateID != "" || in.Profile != "" {
			return in, ErrInvalidWorkOrder
		}
	case WorkInspect:
		if !jobNamePattern.MatchString(in.TemplateID) || in.Profile != "" {
			return in, ErrInvalidWorkOrder
		}
	case WorkVerifyRestore, WorkAnalyze:
		if !jobNamePattern.MatchString(in.Profile) || in.TemplateID != "" {
			return in, ErrInvalidWorkOrder
		}
	default:
		return in, ErrInvalidWorkOrder
	}
	return in, nil
}

func (s *Postgres) WorkOrderTarget(ctx context.Context, eventID string, in WorkOrderInput) (WorkOrderTarget, error) {
	var target WorkOrderTarget
	var status, runtime string
	err := s.pool.QueryRow(ctx, `SELECT a.id::text,a.host,a.port,a.database_name,a.username,`+workOrderTargetRevision+`,e.status,a.runtime
		FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.id=$1 ORDER BY e.started_at DESC LIMIT 1`,
		eventID).Scan(&target.ID, &target.Host, &target.Port, &target.Database, &target.Username, &target.Revision, &status, &runtime)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, ErrEventNotFound
	}
	if err != nil {
		return target, err
	}
	if status != "open" {
		return target, ErrWorkOrderConflict
	}
	if in.Action == WorkInspect {
		var enabled bool
		err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM inspection_templates WHERE id=$1
			AND definition_json->>'scope' IN ($2,'all'))`, in.TemplateID, runtime).Scan(&enabled)
		if err == nil && !enabled {
			return target, ErrInvalidWorkOrder
		}
	}
	return target, err
}

func (s *Postgres) StartWorkPlan(ctx context.Context, id string) (WorkOrderExecution, error) {
	var task WorkOrderExecution
	var raw json.RawMessage
	err := s.pool.QueryRow(ctx, `UPDATE work_orders w SET status='executing',
		executing_deadline=LEAST(w.expires_at,now()+make_interval(secs=>w.timeout_seconds))
		WHERE w.id=$1 AND w.status IN ('approved','job_approved') AND w.expires_at>now()
		AND EXISTS(SELECT FROM events e WHERE e.id=w.event_id AND e.status='open')
		AND (w.plan_json IS NULL OR w.plan_json->>'targetRevision' = (
			SELECT `+workOrderTargetRevision+` FROM assets a WHERE a.id=w.asset_id))
		RETURNING w.event_id::text,w.asset_id::text,w.executing_deadline,
			coalesce(w.plan_json,jsonb_build_object('action',w.action,'assetId',w.asset_id))`, id).Scan(&task.EventID, &task.AssetID, &task.Deadline, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, ErrWorkOrderConflict
	}
	if err == nil {
		err = json.Unmarshal(raw, &task.Plan)
	}
	return task, err
}
