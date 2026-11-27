package store

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type OIDCGroupGrant struct {
	OrganizationID string   `json:"organizationId"`
	Role           string   `json:"role"`
	TeamIDs        []string `json:"teamIds"`
}

func (grant OIDCGroupGrant) Valid() bool {
	return evidenceUUID.MatchString(grant.OrganizationID) && validAccessRole(grant.Role) &&
		len(grant.TeamIDs) <= 100 && validAccessIDs(grant.TeamIDs)
}

func migrateOIDCGroupGrants(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=69)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE access_accounts ADD COLUMN oidc_last_claim_at timestamptz,
		ADD COLUMN oidc_group_allowed boolean NOT NULL DEFAULT false;
		INSERT INTO dbsphere_schema_migrations(version) VALUES(69)`)
	return err
}

// ApplyOIDCGroupGrant uses an existing subject binding. A nil grant revokes
// active sessions when the identity provider confirms loss of authorization.
func (s *Postgres) ApplyOIDCGroupGrant(ctx context.Context, issuer, subject string, issuedAt time.Time, grant *OIDCGroupGrant) (AccessAccount, error) {
	if issuedAt.IsZero() || grant != nil && !grant.Valid() {
		return AccessAccount{}, ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccessAccount{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	var lastClaim *time.Time
	var previouslyAllowed bool
	err = tx.QueryRow(ctx, `SELECT id::text,oidc_last_claim_at,oidc_group_allowed FROM access_accounts
		WHERE oidc_issuer=$1 AND oidc_subject=$2 AND enabled AND organization_id IS NOT NULL FOR UPDATE`, issuer, subject).
		Scan(&id, &lastClaim, &previouslyAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessAccount{}, ErrAccessMissing
	}
	if err != nil {
		return AccessAccount{}, err
	}
	if lastClaim != nil && issuedAt.Before(*lastClaim) {
		return AccessAccount{}, ErrAccessMissing
	}
	account, err := readAccessAccount(tx.QueryRow(ctx, "SELECT "+accountColumns+" FROM access_accounts a WHERE a.id=$1", id))
	if err != nil {
		return AccessAccount{}, err
	}
	if grant != nil {
		var validTarget bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM organizations WHERE id=$1) AND
			(SELECT count(*) FROM access_teams WHERE organization_id=$1 AND id=ANY($2::uuid[]))=$3`,
			grant.OrganizationID, grant.TeamIDs, len(grant.TeamIDs)).Scan(&validTarget)
		if err != nil {
			return AccessAccount{}, err
		}
		if !validTarget {
			grant = nil
		}
	}
	changed := grant == nil
	var expectedTeams []string
	if grant != nil {
		expectedTeams = slices.Clone(grant.TeamIDs)
		slices.Sort(expectedTeams)
		changed = account.OrganizationID != grant.OrganizationID || account.Role != grant.Role || !slices.Equal(account.TeamIDs, expectedTeams)
	}
	// OIDC NumericDate typically has second precision. For conflicting claims
	// from the same second, revoke access and require a fresh authorization.
	if grant != nil && (changed || !previouslyAllowed) && lastClaim != nil && issuedAt.Equal(*lastClaim) {
		grant = nil
		changed = true
	}
	if changed {
		if _, err := tx.Exec(ctx, `DELETE FROM access_sessions WHERE account_id=$1`, id); err != nil {
			return AccessAccount{}, err
		}
	}
	if grant == nil {
		if _, err := tx.Exec(ctx, `UPDATE access_accounts SET oidc_last_claim_at=$2,oidc_group_allowed=false,updated_at=clock_timestamp() WHERE id=$1`, id, issuedAt); err != nil {
			return AccessAccount{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AccessAccount{}, err
		}
		return account, ErrAccessMissing
	}
	if changed {
		if _, err := tx.Exec(ctx, `UPDATE access_accounts SET organization_id=$2,role=$3,oidc_last_claim_at=$4,oidc_group_allowed=true,
			updated_at=clock_timestamp() WHERE id=$1`, id, grant.OrganizationID, grant.Role, issuedAt); err != nil {
			return AccessAccount{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM access_memberships WHERE account_id=$1`, id); err != nil {
			return AccessAccount{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access_memberships(account_id,team_id) SELECT $1,unnest($2::uuid[])`, id, expectedTeams); err != nil {
			return AccessAccount{}, err
		}
		account, err = readAccessAccount(tx.QueryRow(ctx, "SELECT "+accountColumns+" FROM access_accounts a WHERE a.id=$1", id))
		if err != nil {
			return AccessAccount{}, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE access_accounts SET oidc_last_claim_at=$2,oidc_group_allowed=true WHERE id=$1`, id, issuedAt); err != nil {
		return AccessAccount{}, err
	}
	return account, tx.Commit(ctx)
}
