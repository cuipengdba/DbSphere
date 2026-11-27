package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

const DefaultOrganizationID = "00000000-0000-4000-8000-000000000001"

type organizationKey struct{}

func WithOrganization(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, organizationKey{}, id)
}

func OrganizationFromContext(ctx context.Context) string {
	id, _ := ctx.Value(organizationKey{}).(string)
	return id
}

func requestedOrganization(ctx context.Context, supplied string, fallback string) (string, error) {
	scoped := OrganizationFromContext(ctx)
	if supplied != "" && !evidenceUUID.MatchString(supplied) || scoped != "" && supplied != "" && scoped != supplied {
		return "", ErrAccessInput
	}
	if scoped != "" {
		return scoped, nil
	}
	if supplied != "" {
		return supplied, nil
	}
	return fallback, nil
}

type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func migrateOrganizations(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=47)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		CREATE TABLE organizations (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL UNIQUE,
			created_at timestamptz NOT NULL DEFAULT now()
		);
		INSERT INTO organizations(id,name) VALUES('00000000-0000-4000-8000-000000000001','默认组织');
		ALTER TABLE assets ADD COLUMN organization_id uuid NOT NULL
			DEFAULT '00000000-0000-4000-8000-000000000001' REFERENCES organizations(id);
		CREATE INDEX assets_organization ON assets(organization_id,id);
		ALTER TABLE access_accounts ADD COLUMN organization_id uuid REFERENCES organizations(id),
			ADD COLUMN oidc_issuer text, ADD COLUMN oidc_subject text,
			ADD CONSTRAINT access_oidc_identity_pair CHECK((oidc_issuer IS NULL)=(oidc_subject IS NULL));
		CREATE UNIQUE INDEX access_accounts_oidc_identity ON access_accounts(oidc_issuer,oidc_subject)
			WHERE oidc_subject IS NOT NULL;
		ALTER TABLE access_teams ADD COLUMN organization_id uuid NOT NULL
			DEFAULT '00000000-0000-4000-8000-000000000001' REFERENCES organizations(id);
		ALTER TABLE access_teams DROP CONSTRAINT IF EXISTS access_teams_project_name_key;
		ALTER TABLE access_teams ADD UNIQUE(organization_id,project,name);
		CREATE TABLE event_shares (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
			organization_id uuid NOT NULL REFERENCES organizations(id), created_by text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL, revoked_at timestamptz
		);
		CREATE INDEX event_shares_event ON event_shares(event_id,created_at DESC);
		ALTER TABLE admin_audit ADD COLUMN organization_id uuid REFERENCES organizations(id);
		CREATE TABLE organization_audit_entries (
			organization_id uuid NOT NULL REFERENCES organizations(id), sequence bigint NOT NULL,
			record bytea NOT NULL, previous_hash text NOT NULL, hash text NOT NULL, signature text NOT NULL,
			PRIMARY KEY(organization_id,sequence)
		);
		CREATE TABLE organization_audit_heads (
			organization_id uuid PRIMARY KEY REFERENCES organizations(id),
			sequence bigint NOT NULL DEFAULT 0, hash text NOT NULL DEFAULT ''
		);
		CREATE TABLE oidc_flows (digest text PRIMARY KEY, expires_at timestamptz NOT NULL);
		CREATE INDEX oidc_flows_expiry ON oidc_flows(expires_at);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(47)`)
	return err
}

func (s *Postgres) RecordOIDCFlow(ctx context.Context, digest string, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `WITH expired AS (DELETE FROM oidc_flows WHERE expires_at<=now())
		INSERT INTO oidc_flows(digest,expires_at) VALUES($1,$2)`, digest, expires)
	return err
}

func (s *Postgres) ConsumeOIDCFlow(ctx context.Context, digest string) (bool, error) {
	row, err := s.pool.Exec(ctx, `DELETE FROM oidc_flows WHERE digest=$1 AND expires_at>now()`, digest)
	return err == nil && row.RowsAffected() == 1, err
}

func (s *Postgres) Organizations(ctx context.Context) ([]Organization, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,name FROM organizations
		WHERE nullif($1,'')::uuid IS NULL OR id=nullif($1,'')::uuid ORDER BY name,id`, OrganizationFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Organization])
}

func (s *Postgres) CreateOrganization(ctx context.Context, name string) (Organization, error) {
	result := Organization{Name: strings.TrimSpace(name)}
	if OrganizationFromContext(ctx) != "" || len([]rune(result.Name)) < 2 || len([]rune(result.Name)) > 80 || strings.ContainsFunc(result.Name, unicode.IsControl) {
		return result, ErrAccessInput
	}
	err := s.pool.QueryRow(ctx, "INSERT INTO organizations(name) VALUES($1) RETURNING id::text", result.Name).Scan(&result.ID)
	return result, accessWriteError(err)
}

func (s *Postgres) LegacyOrganizationScope(ctx context.Context, explicit []string) ([]string, bool, error) {
	var restricted bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM organizations WHERE id<>$1)", DefaultOrganizationID).Scan(&restricted)
	if err != nil || !restricted {
		return nil, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM assets WHERE organization_id=$1
		AND ($2::uuid[] IS NULL OR id=ANY($2::uuid[])) ORDER BY id`, DefaultOrganizationID, explicit)
	if err != nil {
		return nil, true, err
	}
	result, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return append([]string{}, result...), true, err
}

func (s *Postgres) AssetOrganization(ctx context.Context, id string) (string, error) {
	var organization string
	err := s.pool.QueryRow(ctx, "SELECT organization_id::text FROM assets WHERE id=$1", id).Scan(&organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return organization, err
}

func (s *Postgres) EventOrganization(ctx context.Context, id string) (string, error) {
	var organization string
	err := s.pool.QueryRow(ctx, `SELECT a.organization_id::text FROM event_keys k JOIN assets a ON a.id=k.asset_id
		WHERE k.id=$1 AND ($2='' OR a.organization_id::text=$2)`, id, OrganizationFromContext(ctx)).Scan(&organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrEventNotFound
	}
	return organization, err
}

func (s *Postgres) CreateEventShare(ctx context.Context, eventID string, hours int) (any, error) {
	if hours < 1 || hours > 168 {
		return nil, ErrAccessInput
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `INSERT INTO event_shares(event_id,organization_id,created_by,expires_at)
		SELECT e.id,a.organization_id,$2,now()+make_interval(hours=>$3)
		FROM event_keys e JOIN assets a ON a.id=e.asset_id WHERE e.id=$1
		AND (nullif($4,'')::uuid IS NULL OR a.organization_id=nullif($4,'')::uuid)
		RETURNING jsonb_build_object('id',id,'eventId',event_id,'expiresAt',expires_at)`,
		eventID, accessActor(ctx, "admin"), hours, OrganizationFromContext(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	return json.RawMessage(raw), err
}

func (s *Postgres) ResolveEventShare(ctx context.Context, id string) (string, error) {
	var eventID string
	err := s.pool.QueryRow(ctx, `SELECT event_id::text FROM event_shares WHERE id=$1 AND revoked_at IS NULL AND expires_at>now()
		AND (nullif($2,'')::uuid IS NULL OR organization_id=nullif($2,'')::uuid)`, id, OrganizationFromContext(ctx)).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrEventNotFound
	}
	return eventID, err
}

func (s *Postgres) EventShares(ctx context.Context, eventID string) (any, error) {
	return s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'expiresAt',expires_at,'createdBy',created_by)
		FROM event_shares WHERE event_id=$1 AND revoked_at IS NULL AND expires_at>now()
		AND (nullif($2,'')::uuid IS NULL OR organization_id=nullif($2,'')::uuid)
		ORDER BY created_at DESC LIMIT 50`, eventID, OrganizationFromContext(ctx))
}

func (s *Postgres) RevokeEventShare(ctx context.Context, eventID, shareID string) error {
	row, err := s.pool.Exec(ctx, `UPDATE event_shares SET revoked_at=$3 WHERE id=$1 AND event_id=$2
		AND revoked_at IS NULL AND (nullif($4,'')::uuid IS NULL OR organization_id=nullif($4,'')::uuid)`,
		shareID, eventID, time.Now().UTC(), OrganizationFromContext(ctx))
	if err == nil && row.RowsAffected() != 1 {
		return ErrEventNotFound
	}
	return err
}
