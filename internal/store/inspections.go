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
