package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrRiskInput          = errors.New("invalid forecast review")
	ErrRiskReviewRequired = errors.New("review a shadow forecast before enabling notifications")
)

func migrateRiskBaseline(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=50)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		CREATE TABLE asset_risk_mode (
			asset_id uuid PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			enabled boolean NOT NULL DEFAULT false
		);
		CREATE TABLE risk_scans (
			asset_id uuid PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			through_hour timestamptz NOT NULL
		);
		CREATE TABLE risk_observations (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			evaluated_at timestamptz NOT NULL, metric text NOT NULL, status text NOT NULL,
			mode text NOT NULL, forecast jsonb NOT NULL,
			event_id uuid REFERENCES event_keys(id) ON DELETE SET NULL,
			review text,reviewer text,reviewed_at timestamptz,
			UNIQUE(asset_id,evaluated_at,metric)
		);
		CREATE INDEX risk_observations_asset_recent ON risk_observations(asset_id,evaluated_at DESC);
		CREATE INDEX risk_observations_expiry ON risk_observations(evaluated_at);
		CREATE TABLE risk_active (
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			metric text NOT NULL,event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
			PRIMARY KEY(asset_id,metric)
		);
		CREATE INDEX risk_active_event ON risk_active(event_id);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(50)`)
	return err
}

func (s *Postgres) RiskForecasts(ctx context.Context, asset string) (any, error) {
	var enabled, reviewed bool
	profile := RiskProfile{TimeZone: "UTC"}
	var profileJSON []byte
	if err := s.pool.QueryRow(ctx, `SELECT coalesce((SELECT enabled FROM asset_risk_mode WHERE asset_id=$1),false), EXISTS(
		SELECT 1 FROM risk_observations WHERE asset_id=$1 AND mode='shadow' AND review='confirmed'
			AND reviewed_at>=now()-interval '14 days' AND evaluated_at>=coalesce((SELECT profile_changed_at
			FROM asset_risk_mode WHERE asset_id=$1),'-infinity'::timestamptz)),
		coalesce((SELECT profile FROM asset_risk_mode WHERE asset_id=$1),'{}'::jsonb)`, asset).Scan(&enabled, &reviewed, &profileJSON); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(profileJSON, &profile); err != nil {
		return nil, err
	}
	items, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'evaluatedAt',evaluated_at,'mode',mode,
		'forecast',forecast,'eventId',event_id,'review',review,'reviewer',reviewer,'reviewedAt',reviewed_at,
		'profileCurrent',evaluated_at>=coalesce((SELECT profile_changed_at FROM asset_risk_mode WHERE asset_id=$1),'-infinity'::timestamptz))
		FROM risk_observations WHERE asset_id=$1 AND (evaluated_at=(SELECT max(evaluated_at) FROM risk_observations WHERE asset_id=$1)
		OR (status='risk' AND evaluated_at>=now()-interval '7 days'))
		ORDER BY evaluated_at DESC,metric LIMIT 32`, asset)
	return map[string]any{"enabled": enabled, "reviewed": reviewed, "assessments": items, "profile": profile}, err
}

func (s *Postgres) BacktestRiskForecast(ctx context.Context, asset string) (any, error) {
	hour := time.Now().UTC().Truncate(time.Hour)
	queryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(queryCtx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(queryCtx)
	signals, err := s.forecastRiskSignals(queryCtx, tx, asset, hour)
	if err != nil {
		return nil, err
	}
	metrics := make([]RiskQuality, 0, len(forecastSpecs))
	for _, spec := range nativeRiskSpecs(signals.Engine) {
		metrics = append(metrics, assessRiskQuality(spec, signals, hour, 7))
	}
	return map[string]any{"days": 7, "through": hour, "metrics": metrics}, nil
}

func (s *Postgres) ReviewRiskForecast(ctx context.Context, asset, id, verdict string) error {
	if !evidenceUUID.MatchString(id) || (verdict != "confirmed" && verdict != "false_positive") {
		return ErrRiskInput
	}
	actor := accessActor(ctx, "admin")
	result, err := s.pool.Exec(ctx, `UPDATE risk_observations SET review=$3,reviewer=$4,reviewed_at=now()
		WHERE id=$1 AND asset_id=$2 AND status='risk' AND mode='shadow' AND evaluated_at>=now()-interval '14 days'
		AND evaluated_at>=coalesce((SELECT profile_changed_at FROM asset_risk_mode WHERE asset_id=$2),'-infinity'::timestamptz)`,
		id, asset, verdict, actor)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRiskInput
	}
	return nil
}

func (s *Postgres) SetRiskMode(ctx context.Context, asset string, enabled bool) error {
	result, err := s.pool.Exec(ctx, `INSERT INTO asset_risk_mode(asset_id,enabled)
		SELECT id,$2 FROM assets WHERE id=$1 AND (NOT $2 OR EXISTS(
			SELECT 1 FROM risk_observations WHERE asset_id=$1 AND mode='shadow' AND review='confirmed'
			AND reviewed_at>=now()-interval '14 days' AND evaluated_at>=coalesce((SELECT profile_changed_at
			FROM asset_risk_mode WHERE asset_id=$1),'-infinity'::timestamptz)))
		ON CONFLICT(asset_id) DO UPDATE SET enabled=excluded.enabled`, asset, enabled)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRiskReviewRequired
	}
	return nil
}

func (s *Postgres) forecastRiskSignals(ctx context.Context, tx pgx.Tx, asset string, hour time.Time) (riskSignals, error) {
	signals := riskSignals{Profile: RiskProfile{TimeZone: "UTC"}}
	var profile []byte
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN a.runtime IN ('linux','k8s','managed') THEN 'postgres' ELSE a.runtime END,
		coalesce(m.profile,'{}'::jsonb) FROM assets a LEFT JOIN asset_risk_mode m ON m.asset_id=a.id WHERE a.id=$1`,
		asset).Scan(&signals.Engine, &profile); err != nil {
		return signals, err
	}
	if err := json.Unmarshal(profile, &signals.Profile); err != nil {
		return signals, err
	}
	location, err := time.LoadLocation(signals.Profile.TimeZone)
	if err != nil {
		return signals, err
	}
	signals.Location = location
	if isNativeRiskEngine(signals.Engine) {
		signals.Hours, err = readNativeRiskHours(ctx, tx, asset, signals.Engine, hour)
	} else if signals.Engine == "postgres" {
		var rows pgx.Rows
		rows, err = tx.Query(ctx, `SELECT b.bucket,coalesce(b.value->>'latestSampleId',''),
		(b.value->>'averageConnectionUsagePercent')::float8,(b.value->>'averageDatabaseBytes')::float8,
		(b.value->>'walBytesPerSecond')::float8,
		EXISTS(SELECT 1 FROM silences w WHERE w.kind='maintenance' AND w.revoked_at IS NULL
			AND (w.asset_id=$1 OR w.asset_id IS NULL) AND w.starts_at<b.bucket+interval '1 hour' AND w.ends_at>b.bucket)
		OR EXISTS(SELECT 1 FROM asset_changes c WHERE c.asset_id=$1 AND c.source='operator'
			AND c.kind IN ('maintenance','deployment','configuration')
			AND c.occurred_at>=b.bucket-interval '2 hours' AND c.occurred_at<b.bucket+interval '1 hour')
		FROM monitoring_hourly_buckets b WHERE b.asset_id=$1 AND b.bucket >= $2::timestamptz-interval '21 days'
			AND b.bucket<$2 ORDER BY b.bucket`, asset, hour)
		if err == nil {
			signals.Hours, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (riskHour, error) {
				var b riskHour
				err := row.Scan(&b.At, &b.SampleID, &b.Connections, &b.DatabaseBytes, &b.WALRate, &b.Planned)
				return b, err
			})
		}
	}
	if err != nil {
		return signals, err
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM silences w WHERE w.kind='maintenance' AND w.revoked_at IS NULL
		AND (w.asset_id=$1 OR w.asset_id IS NULL) AND w.starts_at<$2::timestamptz+interval '4 hours'
		AND w.ends_at>$2::timestamptz-interval '1 hour')
		OR EXISTS(SELECT 1 FROM asset_changes c WHERE c.asset_id=$1 AND c.source='operator'
			AND c.kind IN ('maintenance','deployment','configuration')
			AND c.occurred_at>=$2::timestamptz-interval '3 hours' AND c.occurred_at<$2),
		coalesce(repeated.total,0),coalesce(repeated.latest,'')
		FROM (SELECT 1) AS seed LEFT JOIN LATERAL (
			SELECT count(*)::integer AS total,(array_agg(e.id::text ORDER BY e.started_at DESC))[1] AS latest
			FROM events e WHERE e.asset_id=$1 AND e.started_at>=$2::timestamptz-interval '7 days'
			AND e.started_at<$2 AND e.event_group='availability' AND e.kind IN ('collection_failure','alert_incident')
			GROUP BY e.kind,e.title ORDER BY total DESC,max(e.started_at) DESC LIMIT 1
		) repeated ON true`,
		asset, hour).Scan(&signals.Planned, &signals.RecentFailures, &signals.FailureEvent)
	return signals, err
}

func (s *Postgres) AdvanceRiskBaseline(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 50 {
		return 0, ErrRiskInput
	}
	hour := now.UTC().Truncate(time.Hour)
	if err := s.ensureCollectionPartitions(ctx, hour); err != nil {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id::text
		FROM assets a LEFT JOIN risk_scans scan ON scan.asset_id=a.id
		WHERE a.last_sample_at IS NOT NULL AND (scan.through_hour IS NULL OR scan.through_hour<$1)
		ORDER BY scan.through_hour NULLS FIRST,a.id LIMIT $2`, hour, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if err := s.evaluateAssetRisk(ctx, id, hour); err != nil {
			return i, err
		}
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM risk_observations WHERE id IN (
		SELECT id FROM risk_observations WHERE evaluated_at<$1::timestamptz-interval '30 days'
		ORDER BY evaluated_at LIMIT 250)`, hour)
	return len(ids), err
}

func (s *Postgres) evaluateAssetRisk(ctx context.Context, asset string, hour time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, "SELECT id::text FROM assets WHERE id=$1 FOR UPDATE", asset).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO risk_scans(asset_id,through_hour)
		SELECT id,$2 FROM assets WHERE id=$1
		ON CONFLICT(asset_id) DO UPDATE SET through_hour=excluded.through_hour
			WHERE risk_scans.through_hour<excluded.through_hour RETURNING asset_id::text`, asset, hour).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	signals, err := s.forecastRiskSignals(ctx, tx, asset, hour)
	if err != nil {
		return err
	}
	var enabled bool
	if err := tx.QueryRow(ctx, "SELECT coalesce((SELECT enabled FROM asset_risk_mode WHERE asset_id=$1),false)", asset).Scan(&enabled); err != nil {
		return err
	}
	mode := "shadow"
	if enabled {
		mode = "active"
	}
	for _, forecast := range evaluateRisks(hour, signals) {
		raw, err := json.Marshal(forecast)
		if err != nil {
			return err
		}
		var eventID string
		if enabled && forecast.Status == "risk" {
			eventID, err = s.publishRiskForecast(ctx, tx, asset, hour, forecast)
		} else if forecast.Status == "clear" || !enabled {
			err = s.closeRiskForecast(ctx, tx, asset, forecast, hour)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO risk_observations(asset_id,evaluated_at,metric,status,mode,forecast,event_id)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, asset, hour, forecast.Metric, forecast.Status, mode, raw, nullable(eventID)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Postgres) publishRiskForecast(ctx context.Context, tx pgx.Tx, asset string, hour time.Time, forecast RiskForecast) (string, error) {
	var eventID string
	err := tx.QueryRow(ctx, `SELECT r.event_id::text FROM risk_active r JOIN event_keys k ON k.id=r.event_id
		JOIN events e ON e.id=k.id AND e.started_at=k.started_at WHERE r.asset_id=$1 AND r.metric=$2
		AND (e.status='open' OR r.metric='recurrence')`, asset, forecast.Metric).Scan(&eventID)
	if err == nil {
		return eventID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	title := "近七天同类数据库故障重复出现"
	if forecast.Metric == "recurrence" {
		eventID = forecast.SourceEventID
	} else {
		for _, spec := range nativeRiskSpecs(forecast.Engine) {
			if spec.name != forecast.Metric {
				continue
			}
			title = spec.title
			err := tx.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND event_group=$2 AND status='open'
				ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, asset, spec.group).Scan(&eventID)
			if errors.Is(err, pgx.ErrNoRows) {
				if err = tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
					VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset, hour).Scan(&eventID); err != nil {
					return "", err
				}
				if _, err = tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
					VALUES($1,$2,'risk_forecast',$3,'open',$4,$5)`, eventID, asset, title, hour, spec.group); err != nil {
					return "", err
				}
				if _, err = tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id) VALUES($1,$2,$3)
					ON CONFLICT(asset_id,event_group) DO UPDATE SET event_id=excluded.event_id`, asset, spec.group, eventID); err != nil {
					return "", err
				}
			} else if err != nil {
				return "", err
			}
			break
		}
	}
	if eventID == "" {
		return "", nil
	}
	var evidenceID string
	payload, err := json.Marshal(map[string]any{"source": "seasonal_baseline", "ruleId": "risk_" + forecast.Metric, "metric": forecast.Metric, "severity": "warning",
		"title": title, "value": forecast.Observed, "sampleId": forecast.SampleID, "leadMinutes": forecast.LeadMinutes, "forecast": forecast})
	if err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'risk_forecast',$2,$3) RETURNING id::text`, eventID, hour, payload).Scan(&evidenceID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO risk_active(asset_id,metric,event_id) VALUES($1,$2,$3)
		ON CONFLICT(asset_id,metric) DO UPDATE SET event_id=excluded.event_id`, asset, forecast.Metric, eventID); err != nil {
		return "", err
	}
	if err := s.queueGoNotifications(ctx, tx, evidenceID, hour); err != nil {
		return "", err
	}
	return eventID, nil
}

func (s *Postgres) closeRiskForecast(ctx context.Context, tx pgx.Tx, asset string, forecast RiskForecast, hour time.Time) error {
	var eventID string
	err := tx.QueryRow(ctx, `DELETE FROM risk_active WHERE asset_id=$1 AND metric=$2 RETURNING event_id::text`, asset, forecast.Metric).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	payload := map[string]any{"metric": forecast.Metric, "source": "seasonal_baseline", "forecast": forecast}
	if forecast.Status == "clear" && forecast.SampleID != "" {
		payload["sampleId"], payload["value"] = forecast.SampleID, forecast.Observed
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'risk_forecast_cleared',$2,$3)`, eventID, hour, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH closed AS (UPDATE events e SET status='resolved',ended_at=$2 FROM event_keys k
		WHERE k.id=$1 AND e.id=k.id AND e.started_at=k.started_at AND e.status='open' AND e.kind='risk_forecast'
		  AND NOT EXISTS(SELECT 1 FROM risk_active WHERE event_id=k.id)
		  AND NOT EXISTS(SELECT 1 FROM alert_states WHERE asset_id=e.asset_id AND state_json->>'eventId'=k.id::text
			AND state_json->>'status' IN ('firing','recovering','unknown'))
		RETURNING e.id)
		DELETE FROM open_event_groups g USING closed WHERE g.event_id=closed.id`, eventID, hour)
	return err
}
