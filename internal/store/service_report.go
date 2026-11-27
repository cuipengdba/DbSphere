package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrServiceReportInput = errors.New("invalid service report filters")
var ErrServiceReportLimit = errors.New("service report asset limit exceeded")
var ErrServiceReportConflict = errors.New("report revision changed or report is unavailable for the requested action")

const serviceReportAssetLimit = 50

type ServiceReportQuery struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Organization string    `json:"organization"`
	Service      string    `json:"service"`
	AssetGroup   string    `json:"assetGroup"`
	Tag          string    `json:"tag"`
}

func validateServiceReportQuery(q ServiceReportQuery, now time.Time) bool {
	return !q.From.IsZero() && q.To.After(q.From) && q.To.Sub(q.From) <= 31*24*time.Hour && !q.To.After(now.Add(time.Second)) &&
		(q.Organization == "" || evidenceUUID.MatchString(q.Organization)) &&
		(q.Service == "" || validChangeText(q.Service, 56)) && (q.AssetGroup == "" || validChangeText(q.AssetGroup, 150)) &&
		(q.Tag == "" || validChangeText(q.Tag, 64))
}

func migrateServiceReports(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS service_reports (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL REFERENCES organizations(id),
		asset_ids uuid[] NOT NULL CHECK(cardinality(asset_ids) BETWEEN 1 AND 50),
		query jsonb NOT NULL, report jsonb NOT NULL, creator text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
		signed_by text, signed_at timestamptz, signoff_note text
	);
	CREATE INDEX IF NOT EXISTS service_reports_org_time ON service_reports(organization_id,created_at DESC,id);
	CREATE TABLE IF NOT EXISTS postmortem_revisions (
		event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
		revision int NOT NULL CHECK(revision > 0), state text NOT NULL CHECK(state IN ('draft','rejected','archived')),
		body text NOT NULL, actor text NOT NULL, ai_evidence_id uuid REFERENCES evidence(id) ON DELETE SET NULL,
		created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(event_id,revision)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES (75) ON CONFLICT DO NOTHING`)
	return err
}

func (s *Postgres) ServiceReport(ctx context.Context, ids []string, q ServiceReportQuery, interval int, save bool) (json.RawMessage, error) {
	if !validateServiceReportQuery(q, time.Now()) || interval < 10 || interval > 3600 {
		return nil, ErrServiceReportInput
	}
	org, err := requestedOrganization(ctx, q.Organization, "")
	if err != nil || save && org == "" {
		return nil, ErrServiceReportInput
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var assetIDs []string
	err = tx.QueryRow(ctx, `SELECT ARRAY(SELECT a.id::text FROM assets a
		WHERE ($1::uuid[] IS NULL OR a.id=ANY($1)) AND ($2::uuid IS NULL OR a.organization_id=$2)
		AND ($3='' OR ('service:'||$3)=ANY(a.tags)) AND ($4='' OR a.cluster_name=$4)
		AND ($5='' OR $5=ANY(a.tags)) ORDER BY a.id LIMIT $6)`, ids, nullable(org), q.Service, q.AssetGroup, q.Tag, serviceReportAssetLimit+1).Scan(&assetIDs)
	if err != nil {
		return nil, err
	}
	if len(assetIDs) > serviceReportAssetLimit {
		return nil, ErrServiceReportLimit
	}
	var report json.RawMessage
	err = tx.QueryRow(ctx, serviceReportSQL, assetIDs, q.From, q.To, interval).Scan(&report)
	if err != nil {
		return nil, err
	}
	if save {
		if len(assetIDs) == 0 {
			return nil, ErrServiceReportInput
		}
		encoded, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO service_reports(organization_id,asset_ids,query,report,creator)
			VALUES($1,$2,$3,$4,$5) RETURNING `+serviceReportRecordSQL,
			org, assetIDs, encoded, report, accessActor(ctx, "admin")).Scan(&report)
		if err != nil {
			return nil, err
		}
	}
	return report, tx.Commit(ctx)
}

const serviceReportRecordSQL = `jsonb_build_object('id',id,'organizationId',organization_id,'query',query,'report',report,
	'creator',creator,'createdAt',created_at,'signedBy',signed_by,'signedAt',signed_at,'signoffNote',signoff_note)`

// Stored reports are snapshots. Every contributing asset must still be in the
// reader's scope, including when an asset moves to another organization.

const serviceReportAssetsSQL = `NOT EXISTS (SELECT FROM unnest(asset_ids) ref(asset_id) LEFT JOIN assets a ON a.id=ref.asset_id
		WHERE a.organization_id IS DISTINCT FROM service_reports.organization_id)`

const serviceReportScopeSQL = `($2::uuid IS NULL OR organization_id=$2) AND ($3::uuid[] IS NULL OR asset_ids <@ $3) AND ` + serviceReportAssetsSQL

func (s *Postgres) ListServiceReports(ctx context.Context, ids []string, org string) (any, error) {
	organization, err := requestedOrganization(ctx, org, "")
	if err != nil {
		return nil, ErrServiceReportInput
	}
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'organizationId',organization_id,'query',query,
		'creator',creator,'createdAt',created_at,'signedBy',signed_by,'signedAt',signed_at)
		FROM service_reports WHERE ($1::uuid IS NULL OR organization_id=$1) AND ($2::uuid[] IS NULL OR asset_ids <@ $2)
		AND `+serviceReportAssetsSQL+` ORDER BY created_at DESC,id LIMIT 50`, nullable(organization), ids)
	return map[string]any{"reports": rows}, err
}

func (s *Postgres) SavedServiceReport(ctx context.Context, ids []string, id string, signoff *string) (json.RawMessage, error) {
	var raw json.RawMessage
	q := `SELECT ` + serviceReportRecordSQL + ` FROM service_reports WHERE id=$1 AND ` + serviceReportScopeSQL
	args := []any{id, nullable(OrganizationFromContext(ctx)), ids}
	if signoff != nil {
		note := strings.TrimSpace(*signoff)
		if !validChangeText(note, 500) {
			return nil, ErrServiceReportInput
		}
		q = `UPDATE service_reports SET signed_by=$4,signed_at=now(),signoff_note=$5 WHERE id=$1 AND ` +
			serviceReportScopeSQL + ` AND signed_at IS NULL RETURNING ` + serviceReportRecordSQL
		args = append(args, accessActor(ctx, "admin"), note)
	}
	err := s.pool.QueryRow(ctx, q, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceReportConflict
	}
	return raw, err
}
