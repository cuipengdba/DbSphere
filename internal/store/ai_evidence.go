package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/jackc/pgx/v5"
)

type EvidenceReviewContext struct {
	EventID  string             `json:"eventId"`
	AssetID  string             `json:"assetId"`
	Sources  []ai.ReviewSource  `json:"sources"`
	Fallback ai.EvidenceReview  `json:"fallback"`
	Latest   *ai.EvidenceReview `json:"latest,omitempty"`
}

var reviewSourceKinds = []string{
	"collection_error", "collection_recovered", "alert_firing", "alert_recovered",
	"trace_span", "linux_state", "k8s_state", "pgbouncer_state", "patroni_state",
	"routing_observed", "recovery_verified", "risk_forecast",
	"dm8_lock_wait",
	"kingbase_lock_wait",
	"redis_eviction",
	"redis_sentinel_failover",
	"redis_slots_unhealthy",
}

var reviewNumericPaths = []struct {
	name string
	path []string
}{
	{"value", []string{"value"}},
	{"durationMs", []string{"durationMs"}},
	{"blockedSessions", []string{"blockedSessions"}},
	{"replicationLagBytes", []string{"replicationLagBytes"}},
	{"replicaCount", []string{"replicaCount"}},
	{"leaderCount", []string{"leaderCount"}},
	{"leadMinutes", []string{"leadMinutes"}},
	{"cpuUsedPercent", []string{"cpu", "usedPercent"}},
	{"memoryUsedPercent", []string{"memory", "usedPercent"}},
}

func reviewSignals(payload map[string]any) map[string]float64 {
	var signals map[string]float64
	for _, field := range reviewNumericPaths {
		var value any = payload
		for _, key := range field.path {
			value = object(value)[key]
		}
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			continue
		}
		if signals == nil {
			signals = map[string]float64{}
		}
		signals[field.name] = number
	}
	return signals
}

func fallbackEvidenceReview(sources []ai.ReviewSource) ai.EvidenceReview {
	result := ai.EvidenceReview{Claims: []ai.ReviewClaim{}, Uncertainty: "当前证据需要结合原始采样和时间线复核。"}
	if len(sources) == 0 {
		result.Uncertainty = "该事件尚无可用于证据审核的原始观测。"
		return result
	}
	first := sources[0]
	for _, item := range sources {
		switch item.Kind {
		case "collection_error":
			result.Claims = append(result.Claims, ai.ReviewClaim{Kind: "candidate", Text: "采集异常可能来自目标不可达、认证或采集链路，需要结合连接和权限状态区分。", EvidenceIDs: []string{item.ID}})
		case "alert_firing":
			result.Claims = append(result.Claims, ai.ReviewClaim{Kind: "candidate", Text: "监控规则已触发。按触发时间比较同窗采样，核对异常是否集中在关联层。", EvidenceIDs: []string{item.ID}})
		}
		if len(result.Claims) != 0 {
			first = item
			break
		}
	}
	for _, item := range sources {
		if item.Kind == "collection_recovered" || item.Kind == "alert_recovered" || item.Kind == "recovery_verified" {
			result.Claims = append(result.Claims, ai.ReviewClaim{Kind: "counterevidence", Text: "事件内存在恢复观测，可核对首次恢复时间并缩小持续影响范围。", EvidenceIDs: []string{item.ID}})
			break
		}
	}
	result.Claims = append(result.Claims, ai.ReviewClaim{Kind: "verification", Text: "打开引用的原始观测，核对观测时间、资产身份和同期信号。", EvidenceIDs: []string{first.ID}})
	return result
}

func (s *Postgres) PrepareAIEvidenceReview(ctx context.Context, event string) (EvidenceReviewContext, error) {
	result := EvidenceReviewContext{EventID: event, Sources: []ai.ReviewSource{}}
	err := s.pool.QueryRow(ctx, `SELECT a.id::text FROM event_keys k JOIN assets a ON a.id=k.asset_id
		WHERE k.id=$1 AND (nullif($2,'') IS NULL OR a.organization_id=nullif($2,'')::uuid)`,
		event, OrganizationFromContext(ctx)).Scan(&result.AssetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrEventNotFound
	}
	if err != nil {
		return result, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,kind,observed_at,payload_json FROM evidence
		WHERE event_id=$1 AND kind=ANY($2::text[])
		ORDER BY row_number() OVER (PARTITION BY kind ORDER BY observed_at,id),
			CASE WHEN kind IN ('collection_error','alert_firing') THEN 0
			WHEN kind IN ('collection_recovered','alert_recovered','recovery_verified') THEN 1 ELSE 2 END,
			observed_at ASC,id LIMIT $3`, event, reviewSourceKinds, ai.MaxReviewSources)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ai.ReviewSource
		var raw json.RawMessage
		if err := rows.Scan(&item.ID, &item.Kind, &item.At, &raw); err != nil {
			return result, err
		}
		var payload map[string]any
		if json.Unmarshal(raw, &payload) == nil {
			if id, ok := payload["sampleId"].(string); ok && evidenceUUID.MatchString(id) {
				item.SampleID = id
			}
			item.Signals = reviewSignals(payload)
		}
		result.Sources = append(result.Sources, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	rows.Close()
	var previous json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT payload_json->'result' FROM evidence WHERE event_id=$1 AND kind='ai_evidence_review'
		ORDER BY observed_at DESC,id DESC LIMIT 1`, event).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		var review ai.EvidenceReview
		if json.Unmarshal(previous, &review) == nil {
			sources := map[string]bool{}
			for _, source := range result.Sources {
				sources[source.ID] = true
			}
			valid := len(review.Claims) > 0
			for _, claim := range review.Claims {
				for _, id := range claim.EvidenceIDs {
					valid = valid && sources[id]
				}
			}
			if valid {
				result.Latest = &review
			}
		}
	}
	result.Fallback = fallbackEvidenceReview(result.Sources)
	return result, nil
}

func (s *Postgres) ReserveAIEvidenceReview(ctx context.Context, event, model string, limit int) (string, error) {
	return s.reserveAIInvocation(ctx, event, model, limit, accessActor(ctx, "admin"), "evidence_review")
}

func (s *Postgres) CompleteAIEvidenceReview(ctx context.Context, event, invocation, reason string, sourceCount, bytes int,
	review *ai.EvidenceReview) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	cited := []string{}
	state := "failed"
	if review != nil {
		state = "success"
		for _, claim := range review.Claims {
			for _, id := range claim.EvidenceIDs {
				if !slices.Contains(cited, id) {
					cited = append(cited, id)
				}
			}
		}
		var authorized int
		if err := tx.QueryRow(ctx, `SELECT count(*)::integer FROM evidence
			WHERE event_id=$1 AND id=ANY($2::uuid[]) AND kind=ANY($3::text[])`, event, cited, reviewSourceKinds).Scan(&authorized); err != nil {
			return err
		}
		if authorized != len(cited) {
			return ai.ErrInvalidEvidence
		}
	}
	payload, err := json.Marshal(map[string]any{"status": state, "reason": reason, "evidenceIds": cited,
		"inputBytes": bytes, "sourceCount": sourceCount})
	if err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE evidence SET payload_json=payload_json||$3::jsonb
		WHERE id=$1 AND event_id=$2 AND kind='ai_invocation' AND payload_json->>'purpose'='evidence_review'
			AND payload_json->>'status'='pending'`, invocation, event, payload)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrEventNotFound
	}
	if review != nil {
		body, err := json.Marshal(map[string]any{"invocationId": invocation, "result": review})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'ai_evidence_review',now(),$2)`, event, body); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
