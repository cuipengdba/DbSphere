package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/jackc/pgx/v5"
)

type PostmortemInput struct {
	Revision int    `json:"revision"`
	Action   string `json:"action"`
	Body     string `json:"body"`
	ModelID  string `json:"modelInvocationId"`
}

const postmortemAccess = `SELECT e.asset_id::text FROM event_keys e JOIN assets a ON a.id=e.asset_id
	WHERE e.id=$1 AND ($2::uuid IS NULL OR a.organization_id=$2) AND ($3::uuid[] IS NULL OR e.asset_id=ANY($3))`

func (s *Postgres) ReadPostmortem(ctx context.Context, ids []string, eventID string) (any, error) {
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('assetId',visible.asset_id,'revisions',
		coalesce((SELECT jsonb_agg(record ORDER BY revision DESC) FROM
		(SELECT revision,`+postmortemJSON+` AS record FROM postmortem_revisions
		WHERE event_id=$1 ORDER BY revision DESC LIMIT 100) history),'[]'::jsonb))
		FROM (`+postmortemAccess+`) visible(asset_id)`, eventID, nullable(OrganizationFromContext(ctx)), ids).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	return result, err
}

const postmortemJSON = `jsonb_build_object('eventId',event_id,'revision',revision,'state',state,
	'body',body,'actor',actor,'aiEvidenceId',ai_evidence_id,'createdAt',created_at)`

func (s *Postgres) SavePostmortem(ctx context.Context, ids []string, eventID string, input PostmortemInput) (any, error) {
	if input.Revision < 0 || (input.ModelID != "" && (!evidenceUUID.MatchString(input.ModelID) || input.Action != "draft" || input.Body != "")) {
		return nil, ErrServiceReportInput
	}
	state := map[string]string{"draft": "draft", "reject": "rejected", "archive": "archived"}[input.Action]
	if state == "" {
		return nil, ErrServiceReportInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var asset string
	err = tx.QueryRow(ctx, postmortemAccess+` FOR UPDATE OF e FOR SHARE OF a`, eventID, nullable(OrganizationFromContext(ctx)), ids).Scan(&asset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	var revision int
	var previousState, previousBody string
	var evidenceID *string
	err = tx.QueryRow(ctx, `SELECT revision,state,body,ai_evidence_id::text FROM postmortem_revisions
		WHERE event_id=$1 ORDER BY revision DESC LIMIT 1`, eventID).Scan(&revision, &previousState, &previousBody, &evidenceID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if input.Revision != revision || previousState == "archived" || (state != "draft" && previousState != "draft") || revision >= 100 {
		return nil, ErrServiceReportConflict
	}
	body := strings.TrimSpace(input.Body)
	if state != "draft" && body == "" {
		body = previousBody
	}
	if input.ModelID != "" {
		var modelEvidenceID string
		var result []byte
		err = tx.QueryRow(ctx, `SELECT id::text,payload_json->'result' FROM evidence
			WHERE event_id=$1 AND kind='ai_evidence_review' AND payload_json->>'invocationId'=$2`, eventID, input.ModelID).Scan(&modelEvidenceID, &result)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrServiceReportConflict
		}
		if err != nil {
			return nil, err
		}
		var review ai.EvidenceReview
		if err := json.Unmarshal(result, &review); err != nil || len(review.Claims) == 0 {
			return nil, ErrServiceReportConflict
		}
		var started string
		var ended *string
		err = tx.QueryRow(ctx, `SELECT to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI:SS" UTC"'),
			to_char(ended_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI:SS" UTC"') FROM events WHERE id=$1`, eventID).Scan(&started, &ended)
		if err != nil {
			return nil, err
		}
		ending := "尚无恢复记录"
		if ended != nil {
			ending = *ended
		}
		var draft strings.Builder
		fmt.Fprintf(&draft, "已记录的事件窗口：%s 至 %s\n处置与证据摘要（待人工核对）\n", started, ending)
		kinds := map[string]string{"candidate": "候选解释", "counterevidence": "反证", "verification": "核查建议"}
		for _, claim := range review.Claims {
			fmt.Fprintf(&draft, "- %s：%s [%s]\n", kinds[claim.Kind], claim.Text, strings.Join(claim.EvidenceIDs, ", "))
		}
		fmt.Fprintf(&draft, "待核对：%s", review.Uncertainty)
		body, evidenceID = draft.String(), &modelEvidenceID
	}
	if body == "" || utf8.RuneCountInString(body) > 5000 || strings.ContainsFunc(body, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	}) {
		return nil, ErrServiceReportInput
	}
	var saved json.RawMessage
	err = tx.QueryRow(ctx, `INSERT INTO postmortem_revisions(event_id,revision,state,body,actor,ai_evidence_id)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING `+postmortemJSON, eventID, revision+1, state, body, accessActor(ctx, "admin"), evidenceID).Scan(&saved)
	if err != nil {
		return nil, err
	}
	if err := insertChangeEvidence(ctx, tx, eventID, "postmortem_revision", time.Now().UTC(), map[string]any{
		"revision": revision + 1, "state": state, "actor": accessActor(ctx, "admin"),
	}); err != nil {
		return nil, err
	}
	return saved, tx.Commit(ctx)
}
