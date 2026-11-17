package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type alertRule struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Metric          string  `json:"metric"`
	Source          string  `json:"source"`
	Group           string  `json:"group"`
	Severity        string  `json:"severity"`
	Operator        string  `json:"operator"`
	Threshold       float64 `json:"threshold"`
	DurationSeconds int     `json:"durationSeconds"`
	Version         int     `json:"version"`
	Enabled         bool    `json:"enabled"`
}

var goDefaultRules = []alertRule{
	{"pg_connections_high", "PostgreSQL 连接使用率高", "pg.connection_utilization", "PostgreSQL", "capacity", "warning", ">=", 85, 120, 1, true},
	{"pg_lock_wait", "PostgreSQL 会话被锁阻塞", "pg.blocked_sessions", "PostgreSQL", "latency", "warning", ">=", 1, 60, 1, true},
	{"pg_long_transaction", "PostgreSQL 长事务", "pg.long_transactions", "PostgreSQL", "latency", "warning", ">=", 1, 300, 1, true},
	{"linux_memory_high", "Linux 内存使用率高", "linux.memory_used_percent", "node_exporter", "capacity", "warning", ">=", 90, 300, 1, true},
	{"linux_disk_high", "Linux 文件系统使用率高", "linux.disk_used_percent", "node_exporter", "storage", "warning", ">=", 90, 300, 1, true},
}

func seedGoRules(ctx context.Context, tx pgx.Tx) error {
	for _, rule := range goDefaultRules {
		body, err := json.Marshal(rule)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO alert_rules(id,definition_json)
			VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, rule.ID, body); err != nil {
			return err
		}
	}
	return nil
}

type alertState struct {
	Status         string     `json:"status"`
	PendingSince   time.Time  `json:"pendingSince"`
	FiredAt        *time.Time `json:"firedAt"`
	EventID        string     `json:"eventId"`
	MissingSince   *time.Time `json:"missingSince"`
	LastValue      float64    `json:"lastValue"`
	LastObservedAt time.Time  `json:"lastObservedAt"`
	MutedBy        *string    `json:"mutedBy"`
}

type alertSignal struct {
	Available bool
	Value     float64
	Source    string
	Reason    string
	Detail    any
}

func numeric(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func supportedAlertMetric(metric string) bool {
	switch metric {
	case "pg.connection_utilization", "pg.blocked_sessions", "pg.long_transactions",
		"linux.memory_used_percent", "linux.disk_used_percent":
		return true
	}
	return false
}

func readGoSignal(metric string, metrics map[string]any) alertSignal {
	signal := alertSignal{Source: "PostgreSQL", Reason: "signal unavailable"}
	switch metric {
	case "pg.connection_utilization":
		connections, hasConnections := numeric(metrics["connections"])
		maximum, hasMaximum := numeric(metrics["max_connections"])
		if hasConnections && hasMaximum && maximum > 0 {
			return alertSignal{Available: true, Value: connections / maximum * 100, Source: "PostgreSQL"}
		}
	case "pg.blocked_sessions", "pg.long_transactions":
		section, field := "locks", "blockedSessions"
		if metric == "pg.long_transactions" {
			section, field = "transactions", "longTransactions"
		}
		item := object(object(metrics["diagnostics"])[section])
		if stringValue(item["status"]) == "ok" {
			if value, ok := numeric(object(item["data"])[field]); ok {
				return alertSignal{Available: true, Value: value, Source: "PostgreSQL"}
			}
		}
	case "linux.memory_used_percent":
		signal.Source = "node_exporter"
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			if value, ok := numeric(object(linux["memory"])["usedPercent"]); ok {
				return alertSignal{Available: true, Value: value, Source: signal.Source}
			}
		}
	case "linux.disk_used_percent":
		signal.Source = "node_exporter"
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			best, mount := -1.0, ""
			for _, item := range list(linux["filesystems"]) {
				filesystem := object(item)
				if value, ok := numeric(filesystem["usedPercent"]); ok && value > best {
					best, mount = value, stringValue(filesystem["mountpoint"])
				}
			}
			if best >= 0 {
				return alertSignal{Available: true, Value: best, Source: signal.Source,
					Detail: map[string]any{"mountpoint": mount}}
			}
		}
	}
	return signal
}

func nextGoAlert(rule alertRule, signal alertSignal, old *alertState, now time.Time) (*alertState, string) {
	if !signal.Available {
		if old != nil && old.Status == "firing" {
			copy := *old
			if copy.MissingSince == nil {
				copy.MissingSince = &now
				return &copy, "signal_missing"
			}
			return &copy, ""
		}
		return nil, ""
	}
	breach := false
	switch rule.Operator {
	case ">=":
		breach = signal.Value >= rule.Threshold
	case ">":
		breach = signal.Value > rule.Threshold
	case "<=":
		breach = signal.Value <= rule.Threshold
	case "<":
		breach = signal.Value < rule.Threshold
	}
	if !breach {
		if old != nil && old.Status == "firing" {
			return nil, "recovered"
		}
		return nil, ""
	}
	pendingSince := now
	if old != nil && !old.PendingSince.IsZero() {
		pendingSince = old.PendingSince
	}
	firing := now.Sub(pendingSince) >= time.Duration(rule.DurationSeconds)*time.Second ||
		old != nil && old.Status == "firing"
	next := &alertState{Status: "pending", PendingSince: pendingSince,
		LastValue: signal.Value, LastObservedAt: now}
	if old != nil {
		next.EventID, next.FiredAt, next.MutedBy = old.EventID, old.FiredAt, old.MutedBy
	}
	if firing {
		next.Status = "firing"
		if next.FiredAt == nil {
			next.FiredAt = &now
		}
		if old == nil || old.Status != "firing" {
			return next, "firing"
		}
	}
	return next, ""
}

func (s *Postgres) applyGoAlerts(ctx context.Context, tx pgx.Tx, assetID, sampleID string,
	metrics map[string]any, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT definition_json FROM alert_rules ORDER BY id`)
	if err != nil {
		return err
	}
	rules := []alertRule{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var rule alertRule
		if json.Unmarshal(raw, &rule) == nil && supportedAlertMetric(rule.Metric) {
			rules = append(rules, rule)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	states := map[string]*alertState{}
	stateRows, err := tx.Query(ctx, `SELECT rule_id,state_json FROM alert_states WHERE asset_id=$1`, assetID)
	if err != nil {
		return err
	}
	for stateRows.Next() {
		var ruleID string
		var raw []byte
		if err := stateRows.Scan(&ruleID, &raw); err != nil {
			stateRows.Close()
			return err
		}
		var state alertState
		if json.Unmarshal(raw, &state) == nil {
			states[ruleID] = &state
		}
	}
	if err := stateRows.Err(); err != nil {
		stateRows.Close()
		return err
	}
	stateRows.Close()
	var genericSilence *string
	ruleSilences := map[string]string{}
	silences, err := tx.Query(ctx, `SELECT id::text,rule_id FROM silences
		WHERE revoked_at IS NULL AND starts_at<=$1 AND ends_at>$1
		AND (asset_id IS NULL OR asset_id=$2) ORDER BY created_at DESC`, now, assetID)
	if err != nil {
		return err
	}
	for silences.Next() {
		var id string
		var ruleID *string
		if err := silences.Scan(&id, &ruleID); err != nil {
			silences.Close()
			return err
		}
		if ruleID == nil && genericSilence == nil {
			genericSilence = &id
		} else if ruleID != nil {
			if _, exists := ruleSilences[*ruleID]; !exists {
				ruleSilences[*ruleID] = id
			}
		}
	}
	if err := silences.Err(); err != nil {
		silences.Close()
		return err
	}
	silences.Close()
	for _, rule := range rules {
		old := states[rule.ID]
		if !rule.Enabled {
			if old != nil {
				if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2`,
					assetID, rule.ID); err != nil {
					return err
				}
				if err := s.finishGoAlert(ctx, tx, assetID, rule, old, sampleID, now, "disabled"); err != nil {
					return err
				}
			}
			continue
		}
		signal := readGoSignal(rule.Metric, metrics)
		next, transition := nextGoAlert(rule, signal, old, now)
		if next != nil {
			next.MutedBy = genericSilence
			if id, exists := ruleSilences[rule.ID]; exists {
				next.MutedBy = &id
			}
		}
		if transition == "firing" {
			eventID, err := s.openGoAlertEvent(ctx, tx, assetID, rule, now)
			if err != nil {
				return err
			}
			next.EventID = eventID
		}
		if next != nil {
			body, err := json.Marshal(next)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO alert_states(asset_id,rule_id,state_json)
				VALUES($1,$2,$3) ON CONFLICT(asset_id,rule_id)
				DO UPDATE SET state_json=excluded.state_json`, assetID, rule.ID, body); err != nil {
				return err
			}
		} else if old != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2`, assetID, rule.ID); err != nil {
				return err
			}
		}
		if transition == "recovered" || transition == "disabled" {
			if err := s.finishGoAlert(ctx, tx, assetID, rule, old, sampleID, now, transition); err != nil {
				return err
			}
			continue
		}
		if transition == "firing" && next.MutedBy != nil {
			transition = "suppressed"
		} else if old != nil && old.Status == "firing" && next != nil &&
			old.MutedBy != nil && next.MutedBy == nil {
			transition = "unsuppressed"
		} else if old != nil && old.Status == "firing" && next != nil &&
			old.MutedBy == nil && next.MutedBy != nil {
			transition = "suppressed"
		}
		if transition != "" && next != nil && next.EventID != "" {
			payload, _ := json.Marshal(map[string]any{"ruleId": rule.ID, "ruleVersion": rule.Version,
				"metric": rule.Metric, "severity": rule.Severity, "source": signal.Source,
				"value": signal.Value, "threshold": rule.Threshold, "operator": rule.Operator,
				"sampleId": sampleID, "detail": signal.Detail, "reason": signal.Reason,
				"silenceId": next.MutedBy})
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,$2,$3,$4)`, next.EventID, "alert_"+transition, now, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Postgres) openGoAlertEvent(ctx context.Context, tx pgx.Tx, assetID string, rule alertRule, now time.Time) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND event_group=$2
		AND status='open' ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, assetID, rule.Group).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, assetID, now).Scan(&id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'alert_incident',$3,'open',$4,$5)`, id, assetID, rule.Title, now, rule.Group); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id)
		VALUES($1,$2,$3) ON CONFLICT(asset_id,event_group)
		DO UPDATE SET event_id=excluded.event_id`, assetID, rule.Group, id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Postgres) finishGoAlert(ctx context.Context, tx pgx.Tx, assetID string, rule alertRule,
	old *alertState, sampleID string, now time.Time, transition string) error {
	if old == nil || old.EventID == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"ruleId": rule.ID, "ruleVersion": rule.Version,
		"metric": rule.Metric, "sampleId": sampleID})
	if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4)`, old.EventID, "alert_"+transition, now, payload); err != nil {
		return err
	}
	var firing int
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM alert_states
		WHERE asset_id=$1 AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'`,
		assetID, old.EventID).Scan(&firing); err != nil {
		return err
	}
	if firing == 0 {
		if _, err := tx.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$1
			WHERE id=$2 AND kind='alert_incident'`, now, old.EventID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE event_id=$1`, old.EventID); err != nil {
			return err
		}
	}
	return nil
}

func validateGoRuleUpdate(rule alertRule) error {
	if rule.ID == "" || !supportedAlertMetric(rule.Metric) || rule.Title == "" ||
		rule.DurationSeconds < 0 || rule.DurationSeconds > 86400 ||
		rule.Threshold < 0 || rule.Threshold > 1e12 ||
		(rule.Severity != "info" && rule.Severity != "warning" && rule.Severity != "critical") ||
		(rule.Operator != ">=" && rule.Operator != ">" && rule.Operator != "<=" && rule.Operator != "<") {
		return fmt.Errorf("invalid Go alert rule")
	}
	return nil
}

var ErrAlertRuleNotFound = errors.New("alert rule not found")
var ErrAlertRuleUnsupported = errors.New("alert rule collector is not available in Go")
var ErrInvalidAlertRule = errors.New("invalid alert rule update")

type AlertRulePatch struct {
	Enabled         *bool    `json:"enabled"`
	Severity        *string  `json:"severity"`
	Operator        *string  `json:"operator"`
	Threshold       *float64 `json:"threshold"`
	DurationSeconds *int     `json:"durationSeconds"`
}

func (s *Postgres) UpdateAlertRule(ctx context.Context, id string, patch AlertRulePatch) (any, error) {
	if patch.Enabled == nil && patch.Severity == nil && patch.Operator == nil &&
		patch.Threshold == nil && patch.DurationSeconds == nil {
		return nil, ErrInvalidAlertRule
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT definition_json FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlertRuleNotFound
	}
	if err != nil {
		return nil, err
	}
	var rule alertRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, err
	}
	if !supportedAlertMetric(rule.Metric) {
		return nil, ErrAlertRuleUnsupported
	}
	if patch.Enabled != nil {
		rule.Enabled = *patch.Enabled
	}
	if patch.Severity != nil {
		rule.Severity = *patch.Severity
	}
	if patch.Operator != nil {
		rule.Operator = *patch.Operator
	}
	if patch.Threshold != nil {
		rule.Threshold = *patch.Threshold
	}
	if patch.DurationSeconds != nil {
		rule.DurationSeconds = *patch.DurationSeconds
	}
	if validateGoRuleUpdate(rule) != nil {
		return nil, ErrInvalidAlertRule
	}
	rule.Version++
	body, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE alert_rules SET definition_json=$1,updated_at=now()
		WHERE id=$2`, body, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"rule": rule}, nil
}
