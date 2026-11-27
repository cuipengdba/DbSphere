package store

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

var ErrStatementTextUnavailable = errors.New("SQL text unavailable")

const statementTextLimit = 8192

type StatementTextPolicy struct {
	OrganizationID string `json:"organizationId"`
	Days           int    `json:"days"`
}

func migrateStatementText(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=72)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE statement_text_policy (
		organization_id uuid PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
		days integer NOT NULL CHECK(days BETWEEN 0 AND 30)
	);
	CREATE TABLE statement_retest_text (
		evidence_id uuid PRIMARY KEY REFERENCES evidence(id) ON DELETE CASCADE,
		organization_id uuid NOT NULL REFERENCES statement_text_policy(organization_id) ON DELETE CASCADE,
		ciphertext text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL
	);
	CREATE INDEX statement_text_expiry ON statement_retest_text(expires_at,evidence_id);
	CREATE INDEX statement_text_org ON statement_retest_text(organization_id);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(72)`)
	return err
}

func (s *Postgres) StatementTextPolicy(ctx context.Context, organization string) (StatementTextPolicy, error) {
	org, err := requestedOrganization(ctx, organization, DefaultOrganizationID)
	if err != nil {
		return StatementTextPolicy{}, ErrStatementComparison
	}
	value := StatementTextPolicy{OrganizationID: org}
	err = s.pool.QueryRow(ctx, `SELECT coalesce(p.days,0) FROM organizations o
		LEFT JOIN statement_text_policy p ON p.organization_id=o.id WHERE o.id=$1`, org).Scan(&value.Days)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrStatementComparison
	}
	return value, err
}

func (s *Postgres) SetStatementTextPolicy(ctx context.Context, input StatementTextPolicy) (StatementTextPolicy, error) {
	org, err := requestedOrganization(ctx, input.OrganizationID, DefaultOrganizationID)
	if err != nil || input.Days < 0 || input.Days > 30 {
		return StatementTextPolicy{}, ErrStatementComparison
	}
	input.OrganizationID = org
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return input, err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `INSERT INTO statement_text_policy(organization_id,days)
		SELECT id,$2 FROM organizations WHERE id=$1
		ON CONFLICT(organization_id) DO UPDATE SET days=excluded.days`, org, input.Days)
	if err != nil {
		return input, err
	}
	if result.RowsAffected() != 1 {
		return input, ErrStatementComparison
	}
	if _, err := tx.Exec(ctx, `UPDATE statement_retest_text SET expires_at=least(expires_at,created_at+make_interval(days=>$2))
		WHERE organization_id=$1 AND expires_at>created_at+make_interval(days=>$2)`, org, input.Days); err != nil {
		return input, err
	}
	return input, tx.Commit(ctx)
}

func retainStatementText(ctx context.Context, tx pgx.Tx, organization, evidenceID, text string, key []byte) error {
	if text == "" {
		return nil
	}
	if len(text) > statementTextLimit || !utf8.ValidString(text) {
		return ErrStatementComparison
	}
	var days int
	err := tx.QueryRow(ctx, `SELECT days FROM statement_text_policy WHERE organization_id=$1 FOR SHARE`, organization).Scan(&days)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && days == 0 {
		return ErrStatementTextUnavailable
	}
	if err != nil {
		return err
	}
	ciphertext, err := security.Encrypt(key, text)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO statement_retest_text(evidence_id,organization_id,ciphertext,expires_at)
		VALUES($1,$2,$3,now()+make_interval(days=>$4))`, evidenceID, organization, ciphertext, days)
	return err
}

func (s *Postgres) ReadStatementText(ctx context.Context, eventID, evidenceID string, key []byte) (map[string]any, error) {
	target, err := findStatementEvent(ctx, s.pool, eventID)
	if err != nil {
		return nil, err
	}
	var ciphertext string
	var expires time.Time
	err = s.pool.QueryRow(ctx, `SELECT t.ciphertext,t.expires_at FROM statement_retest_text t
		JOIN evidence e ON e.id=t.evidence_id JOIN statement_text_policy p ON p.organization_id=t.organization_id
		WHERE e.event_id=$1 AND e.kind='statement_retest' AND t.evidence_id=$2 AND t.organization_id=$3
		AND p.days>0 AND t.expires_at>now()`, eventID, evidenceID, target.Organization).Scan(&ciphertext, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStatementTextUnavailable
	}
	if err != nil {
		return nil, err
	}
	text, err := security.Decrypt(key, ciphertext)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": text, "expiresAt": expires, "evidenceId": evidenceID, "eventId": eventID}, nil
}

func (s *Postgres) PruneStatementText(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrStatementComparison
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT evidence_id FROM statement_retest_text WHERE expires_at<=$1
		ORDER BY expires_at,evidence_id LIMIT $2 FOR UPDATE SKIP LOCKED
	) DELETE FROM statement_retest_text t USING expired e WHERE t.evidence_id=e.evidence_id`, now, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}
