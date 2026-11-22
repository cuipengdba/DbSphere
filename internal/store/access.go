package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrAccessMissing = errors.New("account or team not found")
var ErrAccessConflict = errors.New("account or team already exists")
var ErrAccessInput = errors.New("invalid identity, team or asset scope")

var accessName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,47}$`)

type accessActorKey struct{}

func WithAccessActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, accessActorKey{}, actor)
}

func accessActor(ctx context.Context, fallback string) string {
	name, _ := ctx.Value(accessActorKey{}).(string)
	if accessName.MatchString(name) {
		return name
	}
	return fallback
}

type AccessAccount struct {
	ID       string   `json:"id"`
	Username string   `json:"username"`
	Role     string   `json:"role"`
	Enabled  bool     `json:"enabled"`
	TeamIDs  []string `json:"teamIds"`
	AssetIDs []string `json:"assetIds"`
	Hash     string   `json:"-"`
}

type AccessAccountInput struct {
	Username string   `json:"username"`
	Role     string   `json:"role"`
	Enabled  *bool    `json:"enabled"`
	TeamIDs  []string `json:"teamIds"`
}

type AccessTeamInput struct {
	Name     string   `json:"name"`
	Project  string   `json:"project"`
	AssetIDs []string `json:"assetIds"`
}

func migrateAccess(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS access_accounts (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), username text NOT NULL UNIQUE,
			password_hash text NOT NULL, role text NOT NULL CHECK(role IN ('admin','operator','viewer','approver')),
			enabled boolean NOT NULL DEFAULT true, updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS access_teams (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL, project text NOT NULL, UNIQUE(project,name)
		);
		CREATE TABLE IF NOT EXISTS access_team_assets (
			team_id uuid NOT NULL REFERENCES access_teams(id),
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE, PRIMARY KEY(team_id,asset_id)
		);
		CREATE INDEX IF NOT EXISTS access_team_assets_asset ON access_team_assets(asset_id,team_id);
		CREATE TABLE IF NOT EXISTS access_memberships (
			account_id uuid NOT NULL REFERENCES access_accounts(id),
			team_id uuid NOT NULL REFERENCES access_teams(id), PRIMARY KEY(account_id,team_id)
		);
		CREATE INDEX IF NOT EXISTS access_memberships_team ON access_memberships(team_id,account_id);
		CREATE TABLE IF NOT EXISTS access_sessions (
			digest text PRIMARY KEY, account_id uuid NOT NULL REFERENCES access_accounts(id),
			expires_at timestamptz NOT NULL
		);
		CREATE INDEX IF NOT EXISTS access_sessions_account ON access_sessions(account_id);
		CREATE INDEX IF NOT EXISTS access_sessions_expiry ON access_sessions(expires_at);
		CREATE TABLE IF NOT EXISTS event_handoffs (
			event_id uuid PRIMARY KEY REFERENCES event_keys(id) ON DELETE CASCADE,
			account_id uuid NOT NULL REFERENCES access_accounts(id), assigned_by text NOT NULL,
			assigned_at timestamptz NOT NULL
		);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(44) ON CONFLICT DO NOTHING`)
	return err
}

const accountColumns = `a.id::text,a.username,a.role,a.enabled,a.password_hash,
	ARRAY(SELECT m.team_id::text FROM access_memberships m WHERE m.account_id=a.id ORDER BY m.team_id),
	ARRAY(SELECT DISTINCT t.asset_id::text FROM access_memberships m JOIN access_team_assets t USING(team_id)
		WHERE m.account_id=a.id ORDER BY t.asset_id::text)`

func readAccessAccount(row pgx.Row) (AccessAccount, error) {
	var account AccessAccount
	err := row.Scan(&account.ID, &account.Username, &account.Role, &account.Enabled, &account.Hash, &account.TeamIDs, &account.AssetIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrAccessMissing
	}
	return account, err
}

func (s *Postgres) AccessCredential(ctx context.Context, name string) (AccessAccount, error) {
	return readAccessAccount(s.pool.QueryRow(ctx, "SELECT "+accountColumns+" FROM access_accounts a WHERE a.username=$1", name))
}

func (s *Postgres) AccessSession(ctx context.Context, digest string, now time.Time) (AccessAccount, error) {
	return readAccessAccount(s.pool.QueryRow(ctx, "SELECT "+accountColumns+`
		FROM access_sessions ss JOIN access_accounts a ON a.id=ss.account_id
		WHERE ss.digest=$1 AND ss.expires_at>$2 AND a.enabled`, digest, now))
}

func (s *Postgres) CreateAccessSession(ctx context.Context, account AccessAccount, digest string, now, expiry time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var enabled bool
	var hash string
	err = tx.QueryRow(ctx, `SELECT enabled,password_hash FROM access_accounts WHERE id=$1 FOR UPDATE`, account.ID).Scan(&enabled, &hash)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!enabled || hash != account.Hash) {
		return ErrAccessMissing
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE expires_at<=$1`, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_sessions(digest,account_id,expires_at) VALUES($1,$2,$3)`, digest, account.ID, expiry); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) RevokeAccessSession(ctx context.Context, digest string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM access_sessions WHERE digest=$1`, digest)
	return err
}

func validAccessRole(role string) bool {
	return role == "admin" || role == "operator" || role == "viewer" || role == "approver"
}

func validAccessIDs(ids []string) bool {
	seen := map[string]bool{}
	for _, id := range ids {
		if !evidenceUUID.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func accessWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrAccessConflict
		case "23503":
			return ErrAccessInput
		}
	}
	return err
}

func (s *Postgres) AccessDirectory(ctx context.Context) (any, error) {
	accounts, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'username',username,'role',role,'enabled',enabled,
		'teamIds',ARRAY(SELECT team_id::text FROM access_memberships WHERE account_id=a.id ORDER BY team_id))
		FROM access_accounts a ORDER BY username`)
	if err != nil {
		return nil, err
	}
	teams, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'name',name,'project',project,
		'assetIds',ARRAY(SELECT asset_id::text FROM access_team_assets WHERE team_id=t.id ORDER BY asset_id))
		FROM access_teams t ORDER BY project,name`)
	return map[string]any{"accounts": accounts, "teams": teams}, err
}

func (s *Postgres) WriteAccessAccount(ctx context.Context, id string, input AccessAccountInput, hash string) (AccessAccount, error) {
	if len(input.TeamIDs) > 100 || !validAccessIDs(input.TeamIDs) || input.Role != "" && !validAccessRole(input.Role) ||
		id == "" && (!accessName.MatchString(input.Username) || input.Username == "admin" || input.Username == "viewer" ||
			input.Username == "approver" || input.Username == "operator" || hash == "" || !validAccessRole(input.Role)) ||
		id != "" && (!evidenceUUID.MatchString(id) || input.Username != "") {
		return AccessAccount{}, ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccessAccount{}, err
	}
	defer tx.Rollback(ctx)
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO access_accounts(username,role,password_hash,enabled)
			VALUES($1,$2,$3,coalesce($4,true)) RETURNING id::text`, input.Username, input.Role, hash, input.Enabled).Scan(&id)
	} else {
		result, updateErr := tx.Exec(ctx, `UPDATE access_accounts SET role=coalesce(nullif($2,''),role),
			enabled=coalesce($3,enabled),updated_at=now() WHERE id=$1`, id, input.Role, input.Enabled)
		err = updateErr
		if err == nil && result.RowsAffected() == 0 {
			err = ErrAccessMissing
		}
	}
	if err != nil {
		return AccessAccount{}, accessWriteError(err)
	}
	if input.TeamIDs != nil {
		if _, err = tx.Exec(ctx, `DELETE FROM access_memberships WHERE account_id=$1`, id); err != nil {
			return AccessAccount{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO access_memberships SELECT $1::uuid,unnest($2::text[])::uuid`, id, input.TeamIDs); err != nil {
			return AccessAccount{}, accessWriteError(err)
		}
	}
	if input.Enabled != nil && !*input.Enabled {
		if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE account_id=$1`, id); err != nil {
			return AccessAccount{}, err
		}
	}
	account, err := readAccessAccount(tx.QueryRow(ctx, "SELECT "+accountColumns+" FROM access_accounts a WHERE a.id=$1", id))
	if err != nil {
		return AccessAccount{}, err
	}
	return account, tx.Commit(ctx)
}

func (s *Postgres) RotateAccessCredential(ctx context.Context, id, hash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE access_accounts SET password_hash=$2,updated_at=now() WHERE id=$1`, id, hash)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrAccessMissing
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_sessions WHERE account_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) WriteAccessTeam(ctx context.Context, id string, input AccessTeamInput) (any, error) {
	input.Name, input.Project = strings.TrimSpace(input.Name), strings.TrimSpace(input.Project)
	if input.Name == "" || input.Project == "" || len(input.Name) > 120 || len(input.Project) > 120 ||
		strings.ContainsFunc(input.Name+input.Project, unicode.IsControl) || len(input.AssetIDs) > 400 || !validAccessIDs(input.AssetIDs) ||
		id != "" && !evidenceUUID.MatchString(id) {
		return nil, ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO access_teams(name,project) VALUES($1,$2) RETURNING id::text`, input.Name, input.Project).Scan(&id)
	} else {
		result, updateErr := tx.Exec(ctx, `UPDATE access_teams SET name=$2,project=$3 WHERE id=$1`, id, input.Name, input.Project)
		err = updateErr
		if err == nil && result.RowsAffected() == 0 {
			err = ErrAccessMissing
		}
	}
	if err != nil {
		return nil, accessWriteError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM access_team_assets WHERE team_id=$1`, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_team_assets SELECT $1::uuid,unnest($2::text[])::uuid`, id, input.AssetIDs); err != nil {
		return nil, accessWriteError(err)
	}
	return map[string]any{"id": id, "name": input.Name, "project": input.Project, "assetIds": input.AssetIDs}, tx.Commit(ctx)
}

func (s *Postgres) HasAccessApprover(ctx context.Context, assetID string) bool {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT FROM access_accounts a JOIN access_memberships m ON m.account_id=a.id
		JOIN access_team_assets t ON t.team_id=m.team_id
		WHERE a.enabled AND a.role='approver' AND ($1='' OR t.asset_id::text=$1))`, assetID).Scan(&found)
	return err == nil && found
}

func (s *Postgres) AccessHandoff(ctx context.Context, eventID string) (any, error) {
	var assetID string
	err := s.pool.QueryRow(ctx, "SELECT asset_id::text FROM event_keys WHERE id=$1", eventID).Scan(&assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	var assignment json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object('accountId',h.account_id,'username',a.username,
		'assignedBy',h.assigned_by,'assignedAt',h.assigned_at)
		FROM event_handoffs h JOIN access_accounts a ON a.id=h.account_id WHERE h.event_id=$1`, eventID).Scan(&assignment)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	eligible, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',a.id,'username',a.username)
		FROM access_accounts a WHERE a.enabled AND a.role='operator' AND EXISTS(
			SELECT FROM access_memberships m JOIN access_team_assets t USING(team_id)
			WHERE m.account_id=a.id AND t.asset_id=$1)
		ORDER BY a.username`, assetID)
	return map[string]any{"assignment": assignment, "eligible": eligible}, err
}

func (s *Postgres) SetAccessHandoff(ctx context.Context, eventID, accountID, note string) error {
	note = strings.TrimSpace(note)
	if !evidenceUUID.MatchString(eventID) || !evidenceUUID.MatchString(accountID) || len([]rune(note)) < 5 || len([]rune(note)) > 500 {
		return ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var assetID, status string
	err = tx.QueryRow(ctx, `SELECT asset_id::text,status FROM events WHERE id=$1 ORDER BY started_at DESC LIMIT 1 FOR UPDATE`,
		eventID).Scan(&assetID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEventNotFound
	}
	if err != nil {
		return err
	}
	if status != "open" {
		return ErrWorkOrderConflict
	}
	var username string
	err = tx.QueryRow(ctx, `SELECT a.username FROM access_accounts a WHERE a.id=$1 AND a.enabled AND a.role='operator'
		AND EXISTS(SELECT FROM access_memberships m JOIN access_team_assets t USING(team_id)
			WHERE m.account_id=a.id AND t.asset_id=$2) FOR SHARE`, accountID, assetID).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccessMissing
	}
	if err != nil {
		return err
	}
	now, actor := time.Now().UTC(), accessActor(ctx, "admin")
	if _, err = tx.Exec(ctx, `INSERT INTO event_handoffs(event_id,account_id,assigned_by,assigned_at)
		VALUES($1,$2,$3,$4) ON CONFLICT(event_id) DO UPDATE
		SET account_id=excluded.account_id,assigned_by=excluded.assigned_by,assigned_at=excluded.assigned_at`,
		eventID, accountID, actor, now); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"actor": actor, "assignedTo": username, "accountId": accountID, "note": note})
	if _, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'event_handoff',$2,$3)`, eventID, now, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
