package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateAlertRuleRevisions(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=67)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE alert_rule_revisions (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), asset_id uuid REFERENCES assets(id) ON DELETE SET NULL,
		rule_id text NOT NULL REFERENCES alert_rules(id), base_global_version integer NOT NULL, base_revision_id uuid,
		replay_json jsonb NOT NULL, created_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
		review_note text, reviewed_by text, reviewed_at timestamptz, applied_by text, applied_at timestamptz,
		rollout_scope text CHECK(rollout_scope IN ('asset','organization')), shadow_state jsonb, shadow_sample uuid,
		shadow_transition text, shadow_at timestamptz, shadow_valid_at timestamptz, shadow_count integer NOT NULL DEFAULT 0
	);
	CREATE INDEX alert_rule_revision_asset ON alert_rule_revisions(asset_id,rule_id,created_at DESC);
	CREATE TABLE alert_rule_rollouts (
		scope_kind text NOT NULL CHECK(scope_kind IN ('asset','organization')), scope_id uuid NOT NULL,
		rule_id text NOT NULL REFERENCES alert_rules(id), revision_id uuid NOT NULL REFERENCES alert_rule_revisions(id),
		PRIMARY KEY(scope_kind,scope_id,rule_id)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(67)`)
	return err
}

type ruleOverride struct {
	ID   string
	Rule alertRule
}

func activeRuleOverrides(ctx context.Context, tx pgx.Tx, assetID, ruleID string) (map[string]ruleOverride, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (r.rule_id) r.rule_id,r.revision_id::text,v.replay_json->'proposed'
		FROM assets a JOIN alert_rule_rollouts r ON (r.scope_kind='asset' AND r.scope_id=a.id)
		OR (r.scope_kind='organization' AND r.scope_id=a.organization_id)
		JOIN alert_rule_revisions v ON v.id=r.revision_id WHERE a.id=$1 AND ($2='' OR r.rule_id=$2)
		ORDER BY r.rule_id,r.scope_kind`, assetID, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]ruleOverride{}
	for rows.Next() {
		var id string
		var raw []byte
		var item ruleOverride
		if err := rows.Scan(&id, &item.ID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &item.Rule); err != nil {
			return nil, err
		}
		result[id] = item
	}
	return result, rows.Err()
}

type AlertRevisionAction struct {
	Action string `json:"action"`
	Note   string `json:"note"`
	Scope  string `json:"scope"`
}

func (s *Postgres) AlertRuleRevisions(ctx context.Context, assetID, ruleID string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeReplayAsset(ctx, tx, assetID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT jsonb_build_object('id',v.id,'createdBy',v.created_by,'createdAt',v.created_at,
		'reviewNote',v.review_note,'reviewedBy',v.reviewed_by,'reviewedAt',v.reviewed_at,'appliedAt',v.applied_at,
		'rolloutScope',v.rollout_scope,'active',r.revision_id IS NOT NULL,'reviewed',v.reviewed_at IS NOT NULL,
		'shadowCount',v.shadow_count,'shadowSampleId',v.shadow_sample,'shadowAt',v.shadow_at,'shadowTransition',v.shadow_transition,
		'replay',v.replay_json)
		FROM alert_rule_revisions v LEFT JOIN alert_rule_rollouts r ON r.revision_id=v.id
		WHERE v.asset_id=$1 AND v.rule_id=$2 ORDER BY v.created_at DESC,v.id LIMIT 25`, assetID, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		results = append(results, raw)
	}
	return map[string]any{"revisions": results}, rows.Err()
}

func (s *Postgres) ChangeAlertRevision(ctx context.Context, assetID, ruleID, id string, input AlertRevisionAction) (any, error) {
	if !evidenceUUID.MatchString(id) {
		return nil, ErrAlertRuleNotFound
	}
	note := strings.TrimSpace(input.Note)
	if input.Action == "review" && (note == "" || len([]rune(note)) > 300) ||
		input.Action == "enable" && input.Scope != "asset" && input.Scope != "organization" ||
		input.Action != "review" && input.Action != "enable" && input.Action != "revert" {
		return nil, ErrInvalidAlertRule
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeReplayAsset(ctx, tx, assetID); err != nil {
		return nil, err
	}
	global, err := loadAlertRule(ctx, tx, ruleID, true)
	if err != nil {
		return nil, err
	}
	var baseVersion int
	var baseRevision, organization string
	var reviewed, applied bool
	var observed int
	err = tx.QueryRow(ctx, `SELECT v.base_global_version,coalesce(v.base_revision_id::text,''),v.reviewed_at IS NOT NULL,
		v.applied_at IS NOT NULL,CASE WHEN v.shadow_valid_at>=now()-interval '14 days' THEN v.shadow_count ELSE 0 END,
		a.organization_id::text FROM alert_rule_revisions v JOIN assets a ON a.id=v.asset_id
		WHERE v.id=$1 AND v.asset_id=$2 AND v.rule_id=$3 FOR UPDATE OF v`, id, assetID, ruleID).
		Scan(&baseVersion, &baseRevision, &reviewed, &applied, &observed, &organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlertRuleNotFound
	}
	if err != nil {
		return nil, err
	}
	actor := accessActor(ctx, "admin")
	switch input.Action {
	case "revert":
		if !applied {
			return nil, ErrAlertRevisionConflict
		}
		result, err := tx.Exec(ctx, `DELETE FROM alert_rule_rollouts WHERE revision_id=$1`, id)
		if err != nil {
			return nil, err
		}
		if result.RowsAffected() != 1 {
			return nil, ErrAlertRevisionConflict
		}
	case "review":
		if applied || reviewed {
			return nil, ErrAlertRevisionConflict
		}
		_, err = tx.Exec(ctx, `UPDATE alert_rule_revisions SET review_note=$2,reviewed_by=$3,reviewed_at=now() WHERE id=$1`, id, note, actor)
	case "enable":
		if applied || !reviewed || observed == 0 {
			return nil, ErrAlertRevisionConflict
		}
		active, lookupErr := activeRuleOverrides(ctx, tx, assetID, ruleID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if global.Version != baseVersion || active[ruleID].ID != baseRevision {
			return nil, ErrAlertRevisionConflict
		}
		scopeID := assetID
		if input.Scope == "organization" {
			scopeID = organization
			var baselineIsAsset bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM alert_rule_rollouts WHERE scope_kind='asset' AND scope_id=$1 AND rule_id=$2)`, assetID, ruleID).Scan(&baselineIsAsset); err != nil {
				return nil, err
			}
			if baselineIsAsset {
				return nil, ErrAlertRevisionConflict
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO alert_rule_rollouts(scope_kind,scope_id,rule_id,revision_id) VALUES($1,$2,$3,$4)
			ON CONFLICT(scope_kind,scope_id,rule_id) DO UPDATE SET revision_id=excluded.revision_id`, input.Scope, scopeID, ruleID, id); err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, `UPDATE alert_rule_revisions SET rollout_scope=$2,applied_by=$3,applied_at=now() WHERE id=$1`, id, input.Scope, actor)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "action": input.Action}, tx.Commit(ctx)
}

func observeAlertRevisions(ctx context.Context, tx pgx.Tx, assetID, sampleID string, now time.Time, metrics, previous map[string]any) error {
	rows, err := tx.Query(ctx, `SELECT id::text,replay_json->'proposed',shadow_state
		FROM alert_rule_revisions WHERE asset_id=$1 AND applied_at IS NULL AND created_at>=now()-interval '14 days'
		ORDER BY created_at DESC,id LIMIT 16 FOR UPDATE`, assetID)
	if err != nil {
		return err
	}
	type shadow struct {
		ID    string
		Rule  alertRule
		State *alertState
	}
	var revisions []shadow
	for rows.Next() {
		var candidate shadow
		var rule, state []byte
		if err := rows.Scan(&candidate.ID, &rule, &state); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(rule, &candidate.Rule); err != nil {
			rows.Close()
			return err
		}
		if len(state) > 0 {
			if err := json.Unmarshal(state, &candidate.State); err != nil {
				rows.Close()
				return err
			}
		}
		revisions = append(revisions, candidate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, candidate := range revisions {
		signal := readGoSignalWithPrevious(candidate.Rule.Metric, metrics, previous, now)
		var next *alertState
		var change string
		if candidate.Rule.Enabled {
			next, change = nextGoAlert(candidate.Rule, signal, candidate.State, now)
		}
		increment := 0
		if signal.Available {
			increment = 1
		}
		body, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_rule_revisions SET shadow_state=$2,
			shadow_transition=CASE WHEN $3='' THEN shadow_transition ELSE $3 END,
			shadow_sample=CASE WHEN $3<>'' OR shadow_transition IS NULL THEN $4::uuid ELSE shadow_sample END,
			shadow_at=CASE WHEN $3<>'' OR shadow_transition IS NULL THEN $5::timestamptz ELSE shadow_at END,
			shadow_valid_at=CASE WHEN $6>0 THEN $5::timestamptz ELSE shadow_valid_at END,
			shadow_count=shadow_count+$6 WHERE id=$1`, candidate.ID, body, change, sampleID, now, increment); err != nil {
			return err
		}
	}
	return nil
}
