package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cuipengdba/dbsphere/internal/ai"
)

var ErrAIBudgetExceeded = errors.New("daily AI invocation budget exceeded")
var ErrAIAlreadyAnalyzed = errors.New("automatic AI analysis already reserved for event")
var ErrAIEventNotEligible = errors.New("event is not eligible for automatic AI analysis")

type eventReportData struct {
	Event         eventRecord          `json:"event"`
	RelatedEvents []relatedEventRecord `json:"relatedEvents"`
	Analysis      struct {
		Status         string   `json:"status"`
		MissingSignals []string `json:"missingSignals"`
		TraceCoverage  struct {
			Spans         int                  `json:"spans"`
			DatabaseSpans int                  `json:"databaseSpans"`
			Services      []string             `json:"services"`
			ServiceImpact []serviceTraceImpact `json:"serviceImpact"`
			AtLimit       bool                 `json:"atLimit"`
		} `json:"traceCoverage"`
		Candidates []struct {
			Code              string   `json:"code"`
			Title             string   `json:"title"`
			Confidence        string   `json:"confidence"`
			Supports          []string `json:"supports"`
			Against           []string `json:"against"`
			Missing           []string `json:"missing"`
			Limitation        string   `json:"limitation"`
			MatchedConditions []struct {
				Metric    string `json:"metric"`
				Observed  any    `json:"observed"`
				Operator  string `json:"operator"`
				Threshold any    `json:"threshold"`
				SampleID  string `json:"sampleId"`
			} `json:"matchedConditions"`
			TimeOrder struct {
				FirstObservedAt time.Time `json:"firstObservedAt"`
				Relation        string    `json:"relation"`
			} `json:"timeOrder"`
			Review *struct {
				Decision string    `json:"decision"`
				Reason   string    `json:"reason"`
				At       time.Time `json:"at"`
			} `json:"review"`
		} `json:"candidates"`
	} `json:"analysis"`
}

// EventReport exports the same evidence and candidate set shown by EventByID.
// It includes identifiers and bounded summaries, never raw SQL or sample payloads.
func (s *Postgres) EventReport(ctx context.Context, id, format string) (string, error) {
	if format != "md" && format != "html" {
		return "", ErrInvalidInspection
	}
	data, err := s.loadEventReportData(ctx, id)
	if err != nil {
		return "", err
	}
	markdown := renderEventMarkdown(data)
	if format == "md" {
		return markdown, nil
	}
	return "<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>DbSphere 问题事件复盘报告</title><style>body{font:15px/1.6 system-ui,sans-serif;" +
		"max-width:1000px;margin:32px auto;padding:0 20px;color:#243b53}" +
		"pre{white-space:pre-wrap;overflow-wrap:anywhere;font:inherit}</style><pre>" +
		html.EscapeString(markdown) + "</pre></html>", nil
}

func (s *Postgres) loadEventReportData(ctx context.Context, id string) (eventReportData, error) {
	value, err := s.EventByID(ctx, id)
	if err != nil {
		return eventReportData{}, err
	}
	if value == nil {
		return eventReportData{}, ErrEventNotFound
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return eventReportData{}, err
	}
	var data eventReportData
	if err := json.Unmarshal(encoded, &data); err != nil {
		return eventReportData{}, err
	}
	return data, nil
}

// EventAIEvidence prepares an explicitly requested, redacted context for a model.
func (s *Postgres) EventAIEvidence(ctx context.Context, id string) (string, []string, error) {
	data, err := s.loadEventReportData(ctx, id)
	if err != nil {
		return "", nil, err
	}
	ids := make([]string, 0, len(data.Event.Evidence))
	for _, item := range data.Event.Evidence {
		if item.Kind != "ai_invocation" && item.Kind != "ai_advisory" &&
			item.Kind != "operator_response" && item.Kind != "rca_decision" {
			ids = append(ids, item.ID)
		}
	}
	data.Event.AssetName = "[资产名称已省略]"
	data.RelatedEvents = nil
	data.Analysis.TraceCoverage.Services = nil
	data.Analysis.TraceCoverage.ServiceImpact = nil
	for i := range data.Analysis.Candidates {
		if data.Analysis.Candidates[i].Review != nil {
			data.Analysis.Candidates[i].Review.Reason = "[人工备注已省略]"
		}
	}
	for i := range data.Event.Evidence {
		item := &data.Event.Evidence[i]
		if item.Kind != "operator_response" && item.Kind != "recovery_verified" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) == nil {
			payload["note"] = "[人工备注已省略]"
			item.Payload, _ = json.Marshal(payload)
		}
	}
	filtered := data.Event.Evidence[:0]
	for _, item := range data.Event.Evidence {
		if item.Kind != "ai_invocation" && item.Kind != "ai_advisory" {
			filtered = append(filtered, item)
		}
	}
	data.Event.Evidence = filtered
	prompt := renderEventMarkdown(data)
	if len(prompt) > 12000 {
		cut := 12000
		for !utf8.ValidString(prompt[:cut]) {
			cut--
		}
		prompt = prompt[:cut] + "\n[后续证据已省略]"
	}
	return prompt, ids, nil
}

// ReserveAIInvocation counts both successful and failed external calls. The
// PostgreSQL transaction lock makes the daily cap shared across Go replicas.
func (s *Postgres) ReserveAIInvocation(ctx context.Context, eventID, model string, dailyLimit int) (string, error) {
	return s.reserveAIInvocation(ctx, eventID, model, dailyLimit, "admin")
}

func (s *Postgres) ReserveAutomaticAIInvocation(ctx context.Context, eventID, model string, dailyLimit int) (string, error) {
	return s.reserveAIInvocation(ctx, eventID, model, dailyLimit, "auto")
}

func (s *Postgres) reserveAIInvocation(ctx context.Context, eventID, model string, dailyLimit int, actor string) (string, error) {
	if dailyLimit < 1 || dailyLimit > 1000 || model == "" {
		return "", fmt.Errorf("invalid AI daily limit or model")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742028)"); err != nil {
		return "", err
	}
	if actor == "auto" {
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events e WHERE e.id=$1
			AND e.status='open' AND e.started_at>=now()-interval '24 hours'
			AND EXISTS(SELECT 1 FROM evidence f WHERE f.event_id=e.id
				AND f.kind='alert_firing' AND f.payload_json->>'severity'='critical'))`, eventID).
			Scan(&eligible); err != nil {
			return "", err
		}
		if !eligible {
			return "", ErrAIEventNotEligible
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence WHERE event_id=$1
			AND kind='ai_invocation' AND payload_json->>'actor'='auto')`, eventID).Scan(&exists); err != nil {
			return "", err
		}
		if exists {
			return "", ErrAIAlreadyAnalyzed
		}
	}
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return "", err
	}
	day := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	var used int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE kind='ai_invocation'
		AND observed_at >= $1 AND observed_at < $2`, day, day.Add(24*time.Hour)).Scan(&used); err != nil {
		return "", err
	}
	if used >= dailyLimit {
		return "", ErrAIBudgetExceeded
	}
	payload, err := json.Marshal(map[string]any{"model": model, "status": "pending", "actor": actor,
		"evidenceIds": []string{}})
	if err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'ai_invocation',$2,$3) RETURNING id::text`, eventID, now, payload).Scan(&id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Postgres) CompleteAIInvocation(ctx context.Context, eventID, invocationID, status string, citedIDs []string) error {
	if status != "success" && status != "failed" {
		return ErrInvalidRCADecision
	}
	update, err := json.Marshal(map[string]any{"status": status, "evidenceIds": citedIDs})
	if err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `UPDATE evidence SET payload_json=payload_json || $3::jsonb
		WHERE id=$1 AND event_id=$2 AND kind='ai_invocation' AND payload_json->>'status'='pending'`,
		invocationID, eventID, update)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrEventNotFound
	}
	return nil
}

// AutoAIEventIDs only selects recent critical incidents which have a real
// firing alert and have not been submitted for automatic analysis.
func (s *Postgres) AutoAIEventIDs(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 20 {
		return nil, fmt.Errorf("invalid automatic AI batch size")
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id::text FROM events e
		WHERE e.status='open' AND e.started_at>=now()-interval '24 hours'
		AND EXISTS(SELECT 1 FROM evidence f WHERE f.event_id=e.id AND f.kind='alert_firing'
			AND f.payload_json->>'severity'='critical')
		AND NOT EXISTS(SELECT 1 FROM evidence a WHERE a.event_id=e.id AND a.kind='ai_invocation'
			AND a.payload_json->>'actor'='auto')
		ORDER BY e.started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CompleteAutomaticAIInvocation stores a bounded, validated suggestion in the
// same event as its audit record. It remains advice, never a root cause verdict.
func (s *Postgres) CompleteAutomaticAIInvocation(ctx context.Context, eventID, invocationID string,
	result *ai.Result) error {
	status := "failed"
	ids := []string{}
	if result != nil {
		if len([]rune(result.Summary)) < 3 || len([]rune(result.Summary)) > 800 ||
			len([]rune(result.Uncertainty)) < 3 || len([]rune(result.Uncertainty)) > 500 ||
			len(result.EvidenceIDs) == 0 || len(result.EvidenceIDs) > 12 ||
			len(result.SuggestedChecks) > 6 {
			return ai.ErrUnavailable
		}
		status, ids = "success", result.EvidenceIDs
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	update, _ := json.Marshal(map[string]any{"status": status, "evidenceIds": ids})
	changed, err := tx.Exec(ctx, `UPDATE evidence SET payload_json=payload_json || $3::jsonb
		WHERE id=$1 AND event_id=$2 AND kind='ai_invocation' AND payload_json->>'status'='pending'
		AND payload_json->>'actor'='auto'`, invocationID, eventID, update)
	if err != nil {
		return err
	}
	if changed.RowsAffected() != 1 {
		return ErrEventNotFound
	}
	if result != nil {
		for _, id := range ids {
			var belongs bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence WHERE event_id=$1 AND id=$2
				AND kind NOT IN ('ai_invocation','ai_advisory','operator_response','rca_decision'))`,
				eventID, id).Scan(&belongs); err != nil || !belongs {
				return ai.ErrInvalidEvidence
			}
		}
		for _, check := range result.SuggestedChecks {
			if len([]rune(check)) > 200 {
				return ai.ErrUnavailable
			}
		}
		payload, _ := json.Marshal(map[string]any{"summary": result.Summary,
			"uncertainty": result.Uncertainty, "suggestedChecks": result.SuggestedChecks,
			"evidenceIds": ids, "model": result.Model, "actor": "auto",
			"notice": "AI 建议仅供排查，须以原始证据和人工验证为准。"})
		if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'ai_advisory',now(),$2)`, eventID, payload); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func reportText(value string) string {
	return markdownCell(html.EscapeString(value))
}

func reportTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "未记录"
	}
	return value.UTC().Format(time.RFC3339)
}

func renderEventMarkdown(data eventReportData) string {
	event := data.Event
	var out strings.Builder
	fmt.Fprintf(&out, "# DbSphere 问题事件复盘报告\n\n问题：%s  \n事件 ID：%s  \n资产：%s（%s）  \n状态：%s  \n开始：%s  \n结束：%s  \n确认：%s\n\n",
		reportText(event.Title), event.ID, reportText(event.AssetName), event.AssetID,
		reportText(event.Status), reportTime(&event.StartedAt), reportTime(event.EndedAt),
		reportTime(event.AcknowledgedAt))
	if len(data.RelatedEvents) > 0 {
		out.WriteString("## 同一原始样本关联的其他事件\n\n")
		for _, related := range data.RelatedEvents {
			fmt.Fprintf(&out, "- [%s](/api/events/%s)：%s（%s，%s）；共享样本 %s。\n",
				reportText(related.ID), related.ID, reportText(related.Title),
				reportText(related.EventGroup), reportText(related.Status), reportText(related.SharedSampleID))
		}
		out.WriteString("共享原始样本仅证明两个事件记录了同一次采样，不证明相同根因。\n\n")
	}
	out.WriteString("## 应用链路覆盖\n\n")
	fmt.Fprintf(&out, "Span %d 条，其中 PostgreSQL 客户端 Span %d 条；服务：%s。采样率和时钟偏移尚未验证。\n\n",
		data.Analysis.TraceCoverage.Spans, data.Analysis.TraceCoverage.DatabaseSpans,
		reportText(strings.Join(data.Analysis.TraceCoverage.Services, "、")))
	for _, impact := range data.Analysis.TraceCoverage.ServiceImpact {
		fmt.Fprintf(&out, "- %s：已观测 Span %d 条，数据库调用 %d 条，错误 %d 条；时间 %s 至 %s；同期 Trace %d 条，故障信号关联 %d 条（数据库错误 %d，锁等待 PID 匹配 %d）。\n",
			reportText(impact.Service), impact.Spans, impact.DatabaseSpans, impact.ErrorSpans,
			reportTime(&impact.FirstAt), reportTime(&impact.LastAt), impact.ObservedTraces,
			impact.CorrelatedTraces, impact.ErrorTraces, impact.LockWaitTraces)
		for _, traceID := range impact.TraceIDs {
			fmt.Fprintf(&out, "  - 关联 Trace [%s](/api/assets/%s/traces/%s)\n",
				traceID, event.AssetID, traceID)
		}
	}
	out.WriteString("故障信号关联指数据库客户端错误或与锁样本精确匹配的后端 PID，并沿父 Span 关联请求；同窗出现与 PID 匹配都不能单独证明根因。\n")
	if data.Analysis.TraceCoverage.AtLimit {
		out.WriteString("Trace 已达到 100 条显示上限，实际影响可能更多。\n")
	}
	if len(data.Analysis.TraceCoverage.ServiceImpact) > 0 {
		out.WriteString("\n")
	}
	out.WriteString("## 根因候选与人工结论\n\n")
	if len(data.Analysis.Candidates) == 0 {
		out.WriteString("未定位：当前证据不足以支持根因候选。\n\n")
	}
	for _, candidate := range data.Analysis.Candidates {
		fmt.Fprintf(&out, "### %s（%s）\n\n置信级别：%s  \n支持证据 ID：%s  \n首次观测：%s（%s）  \n局限：%s\n\n",
			reportText(candidate.Title), reportText(candidate.Code), reportText(candidate.Confidence),
			reportText(strings.Join(candidate.Supports, "、")), reportTime(&candidate.TimeOrder.FirstObservedAt),
			reportText(candidate.TimeOrder.Relation), reportText(candidate.Limitation))
		if len(candidate.Against) > 0 {
			fmt.Fprintf(&out, "反证：%s\n\n", reportText(strings.Join(candidate.Against, "；")))
		}
		if len(candidate.Missing) > 0 {
			fmt.Fprintf(&out, "待验证：%s\n\n", reportText(strings.Join(candidate.Missing, "；")))
		}
		for _, condition := range candidate.MatchedConditions {
			fmt.Fprintf(&out, "匹配条件：%s；观测 %s %s %s",
				reportText(condition.Metric), reportText(fmt.Sprint(condition.Observed)),
				reportText(condition.Operator), reportText(fmt.Sprint(condition.Threshold)))
			if condition.SampleID != "" {
				fmt.Fprintf(&out, "；样本 %s", reportText(condition.SampleID))
			}
			out.WriteString("。\n\n")
		}
		if candidate.Review != nil {
			fmt.Fprintf(&out, "人工结论：%s（%s）；依据：%s\n\n",
				reportText(candidate.Review.Decision), reportTime(&candidate.Review.At),
				reportText(candidate.Review.Reason))
		}
	}
	autoAdvice := false
	for _, evidence := range event.Evidence {
		if evidence.Kind != "ai_advisory" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(evidence.Payload, &payload) != nil {
			continue
		}
		if !autoAdvice {
			out.WriteString("## 可选 AI 告警建议（待人工核验）\n\n")
			autoAdvice = true
		}
		cited := []string{}
		for _, raw := range list(payload["evidenceIds"]) {
			if id := stringValue(raw); id != "" {
				cited = append(cited, id)
			}
		}
		fmt.Fprintf(&out, "- %s · 证据 %s · 模型 %s：%s；不确定性：%s；引用：%s\n",
			reportTime(&evidence.ObservedAt), evidence.ID, reportText(stringValue(payload["model"])),
			reportText(stringValue(payload["summary"])), reportText(stringValue(payload["uncertainty"])),
			reportText(strings.Join(cited, "、")))
	}
	if autoAdvice {
		out.WriteString("\n")
	}
	out.WriteString("## 处置与恢复验证\n\n")
	responseCount := 0
	for _, evidence := range event.Evidence {
		if evidence.Kind != "operator_response" && evidence.Kind != "recovery_verified" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(evidence.Payload, &payload) != nil {
			continue
		}
		responseCount++
		fmt.Fprintf(&out, "- %s · %s · 证据 %s · 成功采样 %s：%s\n",
			reportTime(&evidence.ObservedAt), reportText(stringValue(payload["phase"])), evidence.ID,
			reportText(stringValue(payload["sampleId"])), reportText(stringValue(payload["note"])))
	}
	if responseCount == 0 {
		out.WriteString("暂无人工处置或恢复验证记录。\n")
	}
	out.WriteString("\n")
	out.WriteString("## 信号缺口\n\n")
	if len(data.Analysis.MissingSignals) == 0 {
		out.WriteString("无已标记的信号缺口。\n\n")
	} else {
		for _, missing := range data.Analysis.MissingSignals {
			fmt.Fprintf(&out, "- %s\n", reportText(missing))
		}
		out.WriteString("\n")
	}
	out.WriteString("## 证据时间线\n\n| 时间 | 证据 ID | 类型 | 样本 ID / 规则 ID |\n|---|---|---|---|\n")
	for _, evidence := range event.Evidence {
		var payload map[string]any
		_ = json.Unmarshal(evidence.Payload, &payload)
		ids := []string{}
		for _, key := range []string{"sampleId", "ruleId", "code"} {
			if id, ok := payload[key].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
		fmt.Fprintf(&out, "| %s | %s | %s | %s |\n", evidence.ObservedAt.UTC().Format(time.RFC3339),
			evidence.ID, reportText(evidence.Kind), reportText(strings.Join(ids, "、")))
	}
	if len(event.Evidence) >= 500 {
		out.WriteString("\n时间线已达到 500 条显示上限，可能仍有其他证据。\n")
	}
	return out.String()
}
