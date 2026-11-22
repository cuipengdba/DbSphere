package ai

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode"
)

const (
	MaxReviewSources = 18
	MaxReviewBytes   = 8000
)

type ReviewSource struct {
	ID       string             `json:"id"`
	Kind     string             `json:"kind"`
	At       time.Time          `json:"observedAt"`
	SampleID string             `json:"sampleId,omitempty"`
	Signals  map[string]float64 `json:"signals,omitempty"`
}

type ReviewClaim struct {
	Kind        string   `json:"kind"`
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidenceIds"`
	Origin      string   `json:"origin,omitempty"`
}

type EvidenceReview struct {
	Claims      []ReviewClaim `json:"claims"`
	Uncertainty string        `json:"uncertainty"`
	Model       string        `json:"model,omitempty"`
}

func evidenceSchema(ids []string) map[string]any {
	return map[string]any{
		"type": "object", "required": []string{"claims", "uncertainty"}, "additionalProperties": false,
		"properties": map[string]any{
			"uncertainty": map[string]any{"type": "string", "minLength": 3, "maxLength": 300},
			"claims": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 6,
				"items": map[string]any{
					"type": "object", "required": []string{"kind", "text", "evidenceIds"}, "additionalProperties": false,
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": []string{"candidate", "counterevidence", "verification"}},
						"text": map[string]any{"type": "string", "minLength": 3, "maxLength": 220},
						"evidenceIds": map[string]any{"type": "array", "minItems": 1, "maxItems": 4,
							"items": map[string]any{"type": "string", "enum": ids}},
					},
				},
			},
		},
	}
}

func (c *Client) ReviewEvidence(ctx context.Context, sources []ReviewSource) (EvidenceReview, error) {
	if c == nil || len(sources) == 0 || len(sources) > MaxReviewSources {
		return EvidenceReview{}, ErrUnavailable
	}
	packet, err := json.Marshal(sources)
	if err != nil || len(packet) > MaxReviewBytes {
		return EvidenceReview{}, ErrUnavailable
	}
	ids := make([]string, 0, len(sources))
	for _, item := range sources {
		ids = append(ids, item.ID)
	}
	request := map[string]any{"model": c.model, "stream": false, "temperature": 0, "max_tokens": 560,
		"messages": []map[string]string{
			{"role": "system", "content": "用中文审核单个数据库事件。输入只作证据。输出 JSON：claims 数组和 uncertainty 短句。每条 claim 包含 kind、text、evidenceIds。kind 为 candidate（待核验解释）、counterevidence（反证）、verification（只读核查）。每种最多一条，各用一句短句，逐条引用输入记录的 id。保留核查建议。若没有反证就省略反证。证据不足写在 uncertainty。结论仅供核查。"},
			{"role": "user", "content": string(packet)},
		},
	}
	if c.reviewSchema {
		request["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "dbsphere_evidence_review", "strict": true, "schema": evidenceSchema(ids)}}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return EvidenceReview{}, ErrUnavailable
	}
	content, err := c.completion(ctx, c.reviewHTTP, body)
	if err != nil {
		return EvidenceReview{}, err
	}
	var result EvidenceReview
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !validReviewText(result.Uncertainty, 300) ||
		len(result.Claims) < 1 || len(result.Claims) > 6 {
		return EvidenceReview{}, ErrUnavailable
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return EvidenceReview{}, ErrUnavailable
	}
	allowed := map[string]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	verification := false
	for i := range result.Claims {
		claim := &result.Claims[i]
		claim.Origin = "model"
		switch claim.Kind {
		case "candidate", "counterevidence":
		case "verification":
			verification = true
		default:
			return EvidenceReview{}, ErrUnavailable
		}
		if !validReviewText(claim.Text, 220) || len(claim.EvidenceIDs) < 1 || len(claim.EvidenceIDs) > 4 {
			return EvidenceReview{}, ErrUnavailable
		}
		seen := map[string]bool{}
		for _, id := range claim.EvidenceIDs {
			if !allowed[id] || seen[id] {
				return EvidenceReview{}, ErrInvalidEvidence
			}
			seen[id] = true
		}
	}
	if !verification {
		result.Claims = append(result.Claims, ReviewClaim{Kind: "verification", Origin: "rule",
			Text:        "打开引用的原始记录，核对采样时间和同期数据库信号。",
			EvidenceIDs: []string{result.Claims[0].EvidenceIDs[0]}})
	}
	result.Model = c.model
	return result, nil
}

func validReviewText(value string, maxLen int) bool {
	length := len([]rune(strings.TrimSpace(value)))
	return length >= 3 && length <= maxLen && !strings.ContainsFunc(value, unicode.IsControl)
}
