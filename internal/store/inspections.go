package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

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
