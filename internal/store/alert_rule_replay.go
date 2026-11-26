package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const alertReplayLimit = 2880

var ErrAlertReplayWindow = errors.New("invalid alert replay window or sample limit exceeded")
var ErrAlertRevisionConflict = errors.New("rule revision or review state has changed")

type AlertReplayInput struct {
	From  time.Time      `json:"from"`
	To    time.Time      `json:"to"`
	Patch AlertRulePatch `json:"patch"`
}

type alertReplaySignal struct {
	ID    string
	At    time.Time
	Value alertSignal
	Muted bool
}

type alertReplayEpisode struct {
	BreachAt        time.Time  `json:"breachAt"`
	FiringAt        time.Time  `json:"firingAt"`
	FiringSample    string     `json:"firingSampleId"`
	FiringValue     float64    `json:"firingValue"`
	Suppressed      bool       `json:"suppressed"`
	NotifyAt        *time.Time `json:"notifyAt,omitempty"`
	RecoveredAt     *time.Time `json:"recoveredAt,omitempty"`
	RecoverySample  string     `json:"recoverySampleId,omitempty"`
	RecoverySeconds *float64   `json:"recoverySeconds,omitempty"`
}

type alertReplayComparison struct {
	State     string              `json:"state"`
	Current   *alertReplayEpisode `json:"current,omitempty"`
	Proposed  *alertReplayEpisode `json:"proposed,omitempty"`
	ShiftSecs float64             `json:"shiftSeconds,omitempty"`
}

type alertReplayResult struct {
	Current     alertRule               `json:"current"`
	Proposed    alertRule               `json:"proposed"`
	From        time.Time               `json:"from"`
	To          time.Time               `json:"to"`
	Samples     int                     `json:"samples"`
	Missing     int                     `json:"missingSignals"`
	Maintenance int                     `json:"silencedSamples"`
	Comparisons []alertReplayComparison `json:"comparisons"`
}

func loadAlertRule(ctx context.Context, tx pgx.Tx, ruleID string, lock bool) (alertRule, error) {
	query := `SELECT definition_json FROM alert_rules WHERE id=$1`
	if lock {
		query += " FOR UPDATE"
	}
	var raw []byte
	err := tx.QueryRow(ctx, query, ruleID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return alertRule{}, ErrAlertRuleNotFound
	}
	if err != nil {
		return alertRule{}, err
	}
	var rule alertRule
	err = json.Unmarshal(raw, &rule)
	return rule, err
}

func authorizeReplayAsset(ctx context.Context, tx pgx.Tx, assetID string) error {
	if !evidenceUUID.MatchString(assetID) {
		return ErrAssetNotFound
	}
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND runtime IN ('linux','managed','k8s')
		AND (nullif($2,'')::uuid IS NULL OR organization_id=nullif($2,'')::uuid))`,
		assetID, OrganizationFromContext(ctx)).Scan(&exists)
	if err == nil && !exists {
		return ErrAssetNotFound
	}
	return err
}

func (s *Postgres) ReplayAlertRule(ctx context.Context, assetID, ruleID string, input AlertReplayInput, save bool) (any, error) {
	if !input.To.After(input.From) || input.To.Sub(input.From) > 7*24*time.Hour || input.To.After(time.Now().Add(time.Minute)) || input.From.IsZero() {
		return nil, ErrAlertReplayWindow
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := authorizeReplayAsset(ctx, tx, assetID); err != nil {
		return nil, err
	}
	global, err := loadAlertRule(ctx, tx, ruleID, save)
	if err != nil {
		return nil, err
	}
	overrides, err := activeRuleOverrides(ctx, tx, assetID, ruleID)
	if err != nil {
		return nil, err
	}
	base := global
	if revision, exists := overrides[ruleID]; exists {
		base = revision.Rule
	}
	if base.Metric == "collector.missed_intervals" {
		return nil, ErrAlertRuleUnsupported
	}
	proposed, err := patchedAlertRule(base, input.Patch)
	if err != nil {
		return nil, err
	}
	signals, err := loadReplaySignals(ctx, tx, assetID, input, base)
	if err != nil {
		return nil, err
	}
	result := alertReplayResult{Current: base, Proposed: proposed, From: input.From.UTC(), To: input.To.UTC(), Samples: len(signals)}
	for _, signal := range signals {
		if !signal.Value.Available {
			result.Missing++
		}
		if signal.Muted {
			result.Maintenance++
		}
	}
	result.Comparisons = compareAlertReplay(replayAlertEpisodes(base, signals), replayAlertEpisodes(proposed, signals))
	var revisionID string
	if save {
		if len(signals) == result.Missing {
			return nil, ErrAlertReplayWindow
		}
		body, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO alert_rule_revisions(asset_id,rule_id,base_global_version,base_revision_id,replay_json,created_by)
			VALUES ($1,$2,$3,nullif($4,'')::uuid,$5,$6) RETURNING id::text`, assetID, ruleID, global.Version, overrides[ruleID].ID,
			body, accessActor(ctx, "admin")).Scan(&revisionID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"replay": result, "revisionId": revisionID}, nil
}

func loadReplaySignals(ctx context.Context, tx pgx.Tx, assetID string, input AlertReplayInput, rule alertRule) ([]alertReplaySignal, error) {
	rows, err := tx.Query(ctx, `WITH selected AS (
		SELECT id,collected_at,metrics_json FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		ORDER BY collected_at,id LIMIT $5), prior AS (
		SELECT id,collected_at,metrics_json FROM samples WHERE asset_id=$1 AND collected_at<$2 ORDER BY collected_at DESC,id DESC LIMIT 1)
		SELECT s.id::text,s.collected_at,s.metrics_json,s.collected_at>=$2,
		EXISTS (SELECT 1 FROM silences w WHERE (w.asset_id IS NULL OR w.asset_id=$1) AND (w.rule_id IS NULL OR w.rule_id=$4)
		AND w.starts_at<=s.collected_at AND w.ends_at>s.collected_at AND w.created_at<=s.collected_at
		AND (w.revoked_at IS NULL OR w.revoked_at>s.collected_at))
		FROM (SELECT * FROM selected UNION ALL SELECT * FROM prior) s ORDER BY s.collected_at,s.id`,
		assetID, input.From, input.To, rule.ID, alertReplayLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	signals := []alertReplaySignal{}
	var previous map[string]any
	for rows.Next() {
		var signal alertReplaySignal
		var raw []byte
		var inWindow bool
		if err := rows.Scan(&signal.ID, &signal.At, &raw, &inWindow, &signal.Muted); err != nil {
			return nil, err
		}
		var metrics map[string]any
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &metrics); err != nil {
				return nil, err
			}
		}
		if inWindow {
			if len(signals) == alertReplayLimit {
				return nil, ErrAlertReplayWindow
			}
			signal.Value = readGoSignalWithPrevious(rule.Metric, metrics, previous, signal.At)
			signals = append(signals, signal)
		}
		previous = metrics
	}
	return signals, rows.Err()
}

func replayAlertEpisodes(rule alertRule, signals []alertReplaySignal) []alertReplayEpisode {
	var episodes []alertReplayEpisode
	var state *alertState
	if !rule.Enabled {
		return episodes
	}
	for _, signal := range signals {
		next, change := nextGoAlert(rule, signal.Value, state, signal.At)
		if change == "firing" {
			episode := alertReplayEpisode{BreachAt: next.PendingSince, FiringAt: signal.At,
				FiringSample: signal.ID, FiringValue: signal.Value.Value, Suppressed: signal.Muted}
			episodes = append(episodes, episode)
		}
		if len(episodes) != 0 {
			episode := &episodes[len(episodes)-1]
			if next != nil && next.Status == "firing" && !signal.Muted && episode.NotifyAt == nil {
				episode.NotifyAt = &signal.At
			}
			if change == "recovered" {
				seconds := signal.At.Sub(episode.FiringAt).Seconds()
				episode.RecoveredAt, episode.RecoverySample, episode.RecoverySeconds = &signal.At, signal.ID, &seconds
			}
		}
		state = next
	}
	return episodes
}

func compareAlertReplay(current, proposed []alertReplayEpisode) []alertReplayComparison {
	comparisons := []alertReplayComparison{}
	for i, j := 0, 0; i < len(current) || j < len(proposed); {
		if i == len(current) {
			comparisons = append(comparisons, alertReplayComparison{State: "added", Proposed: &proposed[j]})
			j++
			continue
		}
		if j == len(proposed) {
			comparisons = append(comparisons, alertReplayComparison{State: "omitted", Current: &current[i]})
			i++
			continue
		}
		left, right := &current[i], &proposed[j]
		switch {
		case left.RecoveredAt != nil && !left.RecoveredAt.After(right.BreachAt):
			comparisons = append(comparisons, alertReplayComparison{State: "omitted", Current: left})
			i++
		case right.RecoveredAt != nil && !right.RecoveredAt.After(left.BreachAt):
			comparisons = append(comparisons, alertReplayComparison{State: "added", Proposed: right})
			j++
		default:
			shift := right.FiringAt.Sub(left.FiringAt).Seconds()
			state := "same_time"
			if shift < 0 {
				state = "earlier"
			} else if shift > 0 {
				state = "later"
			}
			comparisons = append(comparisons, alertReplayComparison{State: state, Current: left, Proposed: right, ShiftSecs: shift})
			i++
			j++
		}
	}
	return comparisons
}
