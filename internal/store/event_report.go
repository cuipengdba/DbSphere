package store

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"
)

type eventReportData struct {
	Event    eventRecord `json:"event"`
	Analysis struct {
		Status         string   `json:"status"`
		MissingSignals []string `json:"missingSignals"`
		TraceCoverage  struct {
			Spans         int      `json:"spans"`
			DatabaseSpans int      `json:"databaseSpans"`
			Services      []string `json:"services"`
		} `json:"traceCoverage"`
		Candidates []struct {
			Code       string   `json:"code"`
			Title      string   `json:"title"`
			Confidence string   `json:"confidence"`
			Supports   []string `json:"supports"`
			Against    []string `json:"against"`
			Missing    []string `json:"missing"`
			Limitation string   `json:"limitation"`
			TimeOrder  struct {
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
	value, err := s.EventByID(ctx, id)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", ErrEventNotFound
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var data eventReportData
	if err := json.Unmarshal(encoded, &data); err != nil {
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
	out.WriteString("## 应用链路覆盖\n\n")
	fmt.Fprintf(&out, "Span %d 条，其中 PostgreSQL 客户端 Span %d 条；服务：%s。采样率和时钟偏移尚未验证。\n\n",
		data.Analysis.TraceCoverage.Spans, data.Analysis.TraceCoverage.DatabaseSpans,
		reportText(strings.Join(data.Analysis.TraceCoverage.Services, "、")))
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
		if candidate.Review != nil {
			fmt.Fprintf(&out, "人工结论：%s（%s）；依据：%s\n\n",
				reportText(candidate.Review.Decision), reportTime(&candidate.Review.At),
				reportText(candidate.Review.Reason))
		}
	}
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
