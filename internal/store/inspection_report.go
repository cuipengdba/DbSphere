package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) InspectionReport(ctx context.Context, id, format string) (string, error) {
	var assetName, templateID, sampleID string
	var version int
	var completed time.Time
	var summaryJSON, findingsJSON, eventIDsJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT a.name,r.template_id,r.template_version,
		coalesce(r.sample_id::text,''),r.completed_at,r.summary_json,r.findings_json,r.event_ids_json
		FROM inspection_runs r JOIN assets a ON a.id=r.asset_id WHERE r.id=$1`, id).
		Scan(&assetName, &templateID, &version, &sampleID, &completed,
			&summaryJSON, &findingsJSON, &eventIDsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInspectionTemplateNotFound
	}
	if err != nil {
		return "", err
	}
	var summary inspectionSummary
	var findings []inspectionFinding
	var eventIDs []string
	if err := json.Unmarshal(summaryJSON, &summary); err != nil {
		return "", err
	}
	if err := json.Unmarshal(findingsJSON, &findings); err != nil {
		return "", err
	}
	if err := json.Unmarshal(eventIDsJSON, &eventIDs); err != nil {
		return "", err
	}
	if format == "md" {
		return markdownInspectionReport(id, assetName, templateID, version, sampleID,
			completed, summary, findings, eventIDs), nil
	}
	if format == "html" {
		return htmlInspectionReport(id, assetName, templateID, version, sampleID,
			completed, summary, findings, eventIDs), nil
	}
	return "", ErrInvalidInspection
}

func markdownCell(value string) string {
	return strings.NewReplacer("\\", "\\\\", "|", "\\|", "\r", " ", "\n", " ").Replace(value)
}

func markdownInspectionReport(id, asset, template string, version int, sample string,
	at time.Time, summary inspectionSummary, findings []inspectionFinding, events []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# DbSphere 巡检报告\n\n资产：%s  \n模板：%s v%d  \n巡检 ID：%s  \n样本 ID：%s  \n完成时间：%s\n\n",
		markdownCell(asset), markdownCell(template), version, id, sample, at.UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "风险 %d · 通过 %d · 未知 %d · 不适用 %d · 状态变化 %d\n\n",
		summary.Risk, summary.Pass, summary.Unknown, summary.NotApplicable, summary.Changed)
	if len(events) > 0 {
		fmt.Fprintf(&out, "关联事件 ID：%s\n\n", strings.Join(events, "、"))
	}
	out.WriteString("| 巡检项 | 状态 | 观测值 | 阈值 | 来源/原因 |\n|---|---|---:|---:|---|\n")
	for _, item := range findings {
		observed := "未采集"
		if item.Observed != nil {
			observed = fmt.Sprintf("%g", *item.Observed)
		}
		fmt.Fprintf(&out, "| %s | %s | %s | %g | %s |\n", markdownCell(item.Title),
			markdownCell(item.Status), observed, item.Threshold,
			markdownCell(strings.TrimSpace(item.Source+" "+item.Reason)))
	}
	return out.String()
}

func htmlInspectionReport(id, asset, template string, version int, sample string,
	at time.Time, summary inspectionSummary, findings []inspectionFinding, events []string) string {
	var out strings.Builder
	out.WriteString("<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><title>DbSphere 巡检报告</title>")
	out.WriteString("<style>body{font:15px/1.6 system-ui,sans-serif;max-width:1100px;margin:36px auto;padding:0 20px;color:#243b53}table{border-collapse:collapse;width:100%}th,td{border:1px solid #dce5ef;padding:9px;text-align:left}th{background:#edf4fb}small{color:#61788f}</style>")
	out.WriteString("<h1>DbSphere 巡检报告</h1>")
	fmt.Fprintf(&out, "<p><b>资产：</b>%s<br><b>模板：</b>%s v%d<br><b>巡检 ID：</b>%s<br><b>样本 ID：</b>%s<br><b>完成时间：</b>%s</p>",
		html.EscapeString(asset), html.EscapeString(template), version, id, sample, at.UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "<p>风险 %d · 通过 %d · 未知 %d · 不适用 %d · 状态变化 %d</p>",
		summary.Risk, summary.Pass, summary.Unknown, summary.NotApplicable, summary.Changed)
	if len(events) > 0 {
		fmt.Fprintf(&out, "<p><b>关联事件 ID：</b>%s</p>", html.EscapeString(strings.Join(events, "、")))
	}
	out.WriteString("<table><thead><tr><th>巡检项</th><th>状态</th><th>观测值</th><th>阈值</th><th>来源/原因</th></tr></thead><tbody>")
	for _, item := range findings {
		observed := "未采集"
		if item.Observed != nil {
			observed = fmt.Sprintf("%g", *item.Observed)
		}
		fmt.Fprintf(&out, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%g</td><td>%s</td></tr>",
			html.EscapeString(item.Title), html.EscapeString(item.Status), observed,
			item.Threshold, html.EscapeString(strings.TrimSpace(item.Source+" "+item.Reason)))
	}
	out.WriteString("</tbody></table></html>")
	return out.String()
}
