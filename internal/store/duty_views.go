package store

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var ErrDutyViewLimit = errors.New("saved view limit")

type DutyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	TeamID    string    `json:"teamId,omitempty"`
	Team      string    `json:"team,omitempty"`
	CanEdit   bool      `json:"canEdit"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type DutyViewTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const dutyTeamsSQL = `SELECT t.id,t.name FROM access_teams t JOIN access_memberships m ON m.team_id=t.id
	JOIN access_accounts a ON a.id=m.account_id WHERE a.id=nullif($1,'')::uuid AND a.enabled
	AND coalesce(a.organization_id,$2::uuid)=coalesce(t.organization_id,$2::uuid)`

func migrateDutyViews(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=60)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		CREATE TABLE duty_views (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner text NOT NULL,
			team_id uuid REFERENCES access_teams(id) ON DELETE CASCADE,
			bucket text NOT NULL, name text NOT NULL, route text NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE(bucket,name)
		);
		CREATE INDEX duty_views_owner ON duty_views(owner) WHERE team_id IS NULL;
		CREATE INDEX duty_views_team ON duty_views(team_id) WHERE team_id IS NOT NULL;
		ALTER TABLE event_handoffs ADD COLUMN accepted_at timestamptz;
		INSERT INTO dbsphere_schema_migrations(version) VALUES(60)`)
	return err
}

func (s *Postgres) DutyViews(ctx context.Context, accountID, owner string) ([]DutyView, []DutyViewTeam, error) {
	teamRows, err := s.pool.Query(ctx, dutyTeamsSQL+" ORDER BY t.name,t.id", accountID, DefaultOrganizationID)
	if err != nil {
		return nil, nil, err
	}
	teams, err := pgx.CollectRows(teamRows, pgx.RowToStructByPos[DutyViewTeam])
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH teams AS (`+dutyTeamsSQL+`)
		SELECT v.id::text,v.name,v.route,coalesce(t.id::text,''),coalesce(t.name,''),v.owner=$3,v.updated_at
		FROM duty_views v LEFT JOIN teams t ON t.id=v.team_id
		WHERE t.id IS NOT NULL OR (v.team_id IS NULL AND v.owner=$3)
		ORDER BY v.updated_at DESC,v.id`, accountID, DefaultOrganizationID, owner)
	if err != nil {
		return nil, nil, err
	}
	views, err := pgx.CollectRows(rows, pgx.RowToStructByPos[DutyView])
	return views, teams, err
}

func DutyViewRoute(path string, allowed []string) (string, error) {
	if !strings.HasPrefix(path, "wallboard?") || len(path) > 4000 {
		return "", ErrAccessInput
	}
	source, err := url.ParseQuery(strings.TrimPrefix(path, "wallboard?"))
	if err != nil {
		return "", ErrAccessInput
	}
	q := url.Values{"pins": {"1"}}
	limits := map[string]int{"environment": 80, "cluster": 256, "state": 16, "window": 4, "page": 5,
		"layout": 7, "metrics": 5, "compare": 8, "change": 35, "from": 35, "end": 35, "cursor": 35}
	for name, limit := range limits {
		value := source.Get(name)
		if value != "" && utf8.ValidString(value) && len([]rune(value)) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0 {
			q.Set(name, value)
		}
	}
	if org := source.Get("organization"); evidenceUUID.MatchString(org) {
		q.Set("organization", org)
	}
	if engine := source.Get("engine"); IsLiveEngine(engine) {
		q.Set("engine", engine)
	}
	canRead := func(id string) bool {
		return evidenceUUID.MatchString(id) && (allowed == nil || slices.Contains(allowed, id))
	}
	if asset := source.Get("asset"); canRead(asset) {
		q.Set("asset", asset)
	}
	seen := map[string]bool{}
	for _, id := range source["watch"] {
		if canRead(id) && !seen[id] && len(seen) < 4 {
			q.Add("watch", id)
			seen[id] = true
		}
	}
	return "wallboard?" + q.Encode(), nil
}

func (s *Postgres) SaveDutyView(ctx context.Context, accountID, owner, id, teamID, name, route string, importOnly bool) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 72 ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 || (teamID != "" && !evidenceUUID.MatchString(teamID)) {
		return "", ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	bucket := owner
	if teamID != "" {
		var authorized bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(`+dutyTeamsSQL+` AND t.id=$3)`, accountID, DefaultOrganizationID, teamID).Scan(&authorized)
		if err != nil {
			return "", err
		}
		if !authorized {
			return "", ErrAccessMissing
		}
		bucket = "team:" + teamID
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", bucket); err != nil {
		return "", err
	}
	var duplicateID, existingOwner string
	var size int
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(max(id::text) FILTER(WHERE name=$2),''),
		coalesce(max(owner) FILTER(WHERE name=$2),'') FROM duty_views WHERE bucket=$1`, bucket, name).
		Scan(&size, &duplicateID, &existingOwner); err != nil {
		return "", err
	}
	switch {
	case importOnly && duplicateID != "" && existingOwner == owner:
		return duplicateID, nil
	case duplicateID != "" && (existingOwner != owner || (id != "" && duplicateID != id)):
		return "", ErrAccessConflict
	case id == "" && duplicateID != "":
		id = duplicateID
	case id == "" && size >= 20:
		return "", ErrDutyViewLimit
	}
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO duty_views(owner,team_id,bucket,name,route)
			VALUES($1,nullif($2,'')::uuid,$3,$4,$5) RETURNING id::text`, owner, teamID, bucket, name, route).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `UPDATE duty_views SET name=$3,route=$4,updated_at=now()
			WHERE id=$1 AND owner=$2 AND bucket=$5 RETURNING id::text`, id, owner, name, route, bucket).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAccessMissing
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *Postgres) DeleteDutyView(ctx context.Context, accountID, owner, id string) error {
	tag, err := s.pool.Exec(ctx, `WITH teams AS (`+dutyTeamsSQL+`)
		DELETE FROM duty_views WHERE id=$4 AND owner=$3
		AND (team_id IS NULL OR team_id IN (SELECT id FROM teams))`, accountID, DefaultOrganizationID, owner, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrAccessMissing
	}
	return err
}
