package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed default_inspections.json
var defaultInspectionsJSON []byte

type inspectionCheck struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Metric    string  `json:"metric"`
	Operator  string  `json:"operator"`
	Threshold float64 `json:"threshold"`
	Severity  string  `json:"severity"`
}

type inspectionTemplate struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Scope          string            `json:"scope"`
	Version        int               `json:"version"`
	CatalogVersion int               `json:"catalogVersion,omitempty"`
	Checks         []inspectionCheck `json:"checks"`
}

var ErrInspectionTemplateNotFound = errors.New("inspection template not found")
var ErrInspectionScheduleReference = errors.New("inspection schedule asset or template not found")
var ErrInvalidInspection = errors.New("invalid inspection input")

type InspectionTemplatePatch struct {
	Thresholds map[string]float64 `json:"thresholds"`
}

type InspectionScheduleInput struct {
	AssetID         string `json:"assetId"`
	TemplateID      string `json:"templateId"`
	IntervalMinutes int    `json:"intervalMinutes"`
	Enabled         *bool  `json:"enabled"`
}

type DueInspection struct {
	AssetID    string
	TemplateID string
}

func (s *Postgres) UpdateInspectionTemplate(ctx context.Context, id string, patch InspectionTemplatePatch) (any, error) {
	if len(patch.Thresholds) == 0 {
		return nil, ErrInvalidInspection
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT definition_json FROM inspection_templates WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInspectionTemplateNotFound
		}
		return nil, err
	}
	var template inspectionTemplate
	if err := json.Unmarshal(raw, &template); err != nil {
		return nil, err
	}
	known := map[string]int{}
	for i, check := range template.Checks {
		known[check.ID] = i
	}
	for checkID, threshold := range patch.Thresholds {
		i, ok := known[checkID]
		if !ok || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1e12 {
			return nil, ErrInvalidInspection
		}
		template.Checks[i].Threshold = threshold
	}
	template.Version++
	body, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE inspection_templates SET definition_json=$1,updated_at=now() WHERE id=$2`, body, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"template": template}, nil
}

func (s *Postgres) UpsertInspectionSchedule(ctx context.Context, input InspectionScheduleInput) (any, error) {
	if input.AssetID == "" || input.TemplateID == "" || input.Enabled == nil ||
		input.IntervalMinutes < 15 || input.IntervalMinutes > 10080 {
		return nil, ErrInvalidInspection
	}
	var runtime, scope string
	err := s.pool.QueryRow(ctx, `SELECT a.runtime,t.definition_json->>'scope' FROM assets a
		JOIN inspection_templates t ON t.id=$2 WHERE a.id=$1`, input.AssetID, input.TemplateID).
		Scan(&runtime, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInspectionScheduleReference
	}
	if err != nil {
		return nil, err
	}
	if scope != "all" && scope != runtime {
		return nil, ErrInvalidInspection
	}
	next := time.Now().UTC().Add(time.Duration(input.IntervalMinutes) * time.Minute)
	_, err = s.pool.Exec(ctx, `INSERT INTO inspection_schedules(asset_id,template_id,interval_minutes,next_due_at,enabled)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(asset_id,template_id) DO UPDATE SET
		interval_minutes=excluded.interval_minutes,next_due_at=excluded.next_due_at,enabled=excluded.enabled`,
		input.AssetID, input.TemplateID, input.IntervalMinutes, next, *input.Enabled)
	if err != nil {
		return nil, err
	}
	return map[string]any{"schedule": map[string]any{"assetId": input.AssetID,
		"templateId": input.TemplateID, "intervalMinutes": input.IntervalMinutes,
		"nextDueAt": next, "enabled": *input.Enabled}}, nil
}

func (s *Postgres) RunInspection(ctx context.Context, assetID, templateID string, key []byte, retryOf string) (value any, runErr error) {
	var runtime string
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT a.runtime,t.definition_json FROM assets a
		JOIN inspection_templates t ON t.id=$2 WHERE a.id=$1`, assetID, templateID).Scan(&runtime, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInspectionScheduleReference
		}
		return nil, err
	}
	var template inspectionTemplate
	if err := json.Unmarshal(raw, &template); err != nil {
		return nil, err
	}
	if template.Scope != "all" && template.Scope != runtime {
		return nil, ErrInvalidInspection
	}
	started := time.Now().UTC()
	defer func() {
		if runErr == nil || ctx.Err() != nil {
			return
		}
		failure, err := s.saveFailedInspection(ctx, assetID, templateID, template, retryOf, started)
		if err == nil {
			value, runErr = failure, nil
		}
	}()
	result, err := s.CheckAsset(ctx, assetID, key)
	if err != nil {
		return nil, err
	}
	var metricsJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE id=$1 AND asset_id=$2
		AND collected_at=$3`, result.SampleID, assetID, result.CollectedAt).Scan(&metricsJSON); err != nil {
		return nil, err
	}
	var metrics map[string]any
	if err := json.Unmarshal(metricsJSON, &metrics); err != nil {
		return nil, err
	}
	var previous map[string]any
	var previousJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE asset_id=$1 AND collected_at<$2
		ORDER BY collected_at DESC LIMIT 1`, assetID, result.CollectedAt).Scan(&previousJSON); err == nil {
		_ = json.Unmarshal(previousJSON, &previous)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	prior := map[string]string{}
	var priorJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT findings_json FROM inspection_runs WHERE asset_id=$1
		AND template_id=$2 AND status='completed' ORDER BY completed_at DESC LIMIT 1`, assetID, templateID).
		Scan(&priorJSON); err == nil {
		var earlier []inspectionFinding
		if json.Unmarshal(priorJSON, &earlier) == nil {
			for _, finding := range earlier {
				prior[finding.ID] = finding.Status
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	findings, summary := evaluateGoInspection(template, metrics, previous, result.CollectedAt, prior)
	var eventIDsJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(id::text),'[]'::jsonb) FROM events
		WHERE asset_id=$1 AND status='open'`, assetID).Scan(&eventIDsJSON); err != nil {
		return nil, err
	}
	var eventIDs []string
	if err := json.Unmarshal(eventIDsJSON, &eventIDs); err != nil {
		return nil, err
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return nil, err
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	var retry any
	if retryOf != "" {
		retry = retryOf
	}
	completed := time.Now().UTC()
	var id string
	if err := s.pool.QueryRow(ctx, `INSERT INTO inspection_runs(id,asset_id,template_id,template_version,
		sample_id,retry_of,started_at,completed_at,summary_json,findings_json,status,error,event_ids_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,$9,'completed',NULL,$10)
		RETURNING id::text`, assetID, templateID, template.Version, result.SampleID,
		retry, started, completed, summaryJSON, findingsJSON, eventIDsJSON).Scan(&id); err != nil {
		return nil, err
	}
	return map[string]any{"run": map[string]any{"id": id, "assetId": assetID, "templateId": templateID,
		"templateVersion": template.Version, "sampleId": result.SampleID, "retryOf": retry,
		"startedAt": started, "completedAt": completed, "status": "completed",
		"error": nil, "summary": summary, "findings": findings, "eventIds": eventIDs}}, nil
}

func (s *Postgres) saveFailedInspection(ctx context.Context, assetID, templateID string,
	template inspectionTemplate, retryOf string, started time.Time) (any, error) {
	completed := time.Now().UTC()
	summary := inspectionSummary{Unknown: len(template.Checks)}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	var retry any
	if retryOf != "" {
		retry = retryOf
	}
	const reason = "巡检执行失败，请检查采集配置后重试"
	var id string
	if err := s.pool.QueryRow(ctx, `INSERT INTO inspection_runs(id,asset_id,template_id,template_version,
		sample_id,retry_of,started_at,completed_at,summary_json,findings_json,status,error,event_ids_json)
		VALUES(gen_random_uuid(),$1,$2,$3,NULL,$4,$5,$6,$7,'[]'::jsonb,'failed',$8,'[]'::jsonb)
		RETURNING id::text`, assetID, templateID, template.Version, retry, started,
		completed, summaryJSON, reason).Scan(&id); err != nil {
		return nil, err
	}
	return map[string]any{"run": map[string]any{"id": id, "assetId": assetID,
		"templateId": templateID, "templateVersion": template.Version, "sampleId": nil,
		"retryOf": retry, "startedAt": started, "completedAt": completed,
		"status": "failed", "error": reason, "summary": summary,
		"findings": []inspectionFinding{}, "eventIds": []string{}}}, nil
}

func (s *Postgres) RetryInspection(ctx context.Context, id string, key []byte) (any, error) {
	var assetID, templateID string
	if err := s.pool.QueryRow(ctx, `SELECT asset_id::text,template_id FROM inspection_runs WHERE id=$1`, id).
		Scan(&assetID, &templateID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInspectionTemplateNotFound
		}
		return nil, err
	}
	return s.RunInspection(ctx, assetID, templateID, key, id)
}

func (s *Postgres) ClaimDueInspections(ctx context.Context, now time.Time, limit int) ([]DueInspection, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidInspection
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT asset_id::text,template_id FROM inspection_schedules
		WHERE enabled=true AND next_due_at<=$1 ORDER BY next_due_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	due := []DueInspection{}
	for rows.Next() {
		var item DueInspection
		if err := rows.Scan(&item.AssetID, &item.TemplateID); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, item := range due {
		if _, err := tx.Exec(ctx, `UPDATE inspection_schedules SET
			next_due_at=$1::timestamptz + interval_minutes * interval '1 minute'
			WHERE asset_id=$2 AND template_id=$3`, now, item.AssetID, item.TemplateID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return due, nil
}

// seedGoInspections adds built-in templates without overwriting operator thresholds.
// Catalog upgrades copy thresholds by stable check ID, as the Node implementation does.
func seedGoInspections(ctx context.Context, tx pgx.Tx) error {
	var builtins []inspectionTemplate
	if err := json.Unmarshal(defaultInspectionsJSON, &builtins); err != nil {
		return fmt.Errorf("decode built-in inspection templates: %w", err)
	}
	for _, builtin := range builtins {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT definition_json FROM inspection_templates WHERE id=$1 FOR UPDATE`, builtin.ID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			body, err := json.Marshal(builtin)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO inspection_templates(id,definition_json,updated_at)
				VALUES($1,$2,now())`, builtin.ID, body); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var existing inspectionTemplate
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("decode inspection template %s: %w", builtin.ID, err)
		}
		if existing.CatalogVersion >= builtin.CatalogVersion {
			continue
		}
		thresholds := make(map[string]float64, len(existing.Checks))
		for _, check := range existing.Checks {
			thresholds[check.ID] = check.Threshold
		}
		for i := range builtin.Checks {
			if threshold, ok := thresholds[builtin.Checks[i].ID]; ok {
				builtin.Checks[i].Threshold = threshold
			}
		}
		builtin.Version = existing.Version + 1
		body, err := json.Marshal(builtin)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE inspection_templates SET definition_json=$1,
			updated_at=now() WHERE id=$2`, body, builtin.ID); err != nil {
			return err
		}
	}
	return nil
}
