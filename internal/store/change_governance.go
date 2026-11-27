package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var ErrChangeState = errors.New("change status, window or receipt conflicts with the request")

type ChangeWindowInput struct {
	Title        string    `json:"title"`
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	Organization bool      `json:"organization"`
}

type ChangeOrderInput struct {
	WindowID   string `json:"windowId"`
	Title      string `json:"title"`
	Reason     string `json:"reason"`
	ExternalID string `json:"externalId"`
}

type ChangeReceipt struct {
	ChangeID   string `json:"changeId"`
	ExternalID string `json:"externalId"`
	ReceiptID  string `json:"receiptId"`
	State      string `json:"state"`
}

type ChangeReview struct {
	EventID string `json:"eventId"`
	Note    string `json:"note"`
}

func migrateChangeGovernance(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS change_windows (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
		asset_id uuid REFERENCES assets(id) ON DELETE CASCADE, title text NOT NULL,
		starts_at timestamptz NOT NULL, ends_at timestamptz NOT NULL CHECK(ends_at>starts_at), creator text NOT NULL
	);
	CREATE INDEX IF NOT EXISTS change_windows_scope ON change_windows(organization_id,asset_id,ends_at);
	CREATE TABLE IF NOT EXISTS change_orders (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
		asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE, window_id uuid NOT NULL REFERENCES change_windows(id) ON DELETE CASCADE,
		title text NOT NULL, reason text NOT NULL, external_id text NOT NULL, target_revision text NOT NULL,
		status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','approved','rejected','started','succeeded','failed','rolled_back')),
		requester text NOT NULL, reviewer text,
		created_at timestamptz NOT NULL DEFAULT now(), reviewed_at timestamptz, started_at timestamptz,
		finished_at timestamptz, rolled_back_at timestamptz,
		baseline jsonb, after_sample jsonb, recovery_sample jsonb, review_note text, review_actor text, review_at timestamptz,
		recovery_note text, recovery_actor text, recovery_at timestamptz,
		event_id uuid REFERENCES event_keys(id) ON DELETE SET NULL,
		UNIQUE(organization_id,external_id)
	);
	CREATE INDEX IF NOT EXISTS change_orders_asset ON change_orders(asset_id,created_at DESC);
	CREATE INDEX IF NOT EXISTS change_orders_event ON change_orders(event_id) WHERE event_id IS NOT NULL;
	CREATE UNIQUE INDEX IF NOT EXISTS change_orders_running ON change_orders(asset_id) WHERE status='started';
	CREATE TABLE IF NOT EXISTS change_receipts (
		change_id uuid NOT NULL REFERENCES change_orders(id) ON DELETE CASCADE, receipt_id text NOT NULL,
		state text NOT NULL, received_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(change_id,receipt_id)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(73) ON CONFLICT DO NOTHING`)
	return err
}

func validChangeText(value string, max int) bool {
	return value != "" && utf8.RuneCountInString(value) <= max && !strings.ContainsFunc(value, unicode.IsControl)
}

func (s *Postgres) CreateChangeWindow(ctx context.Context, assetID string, in ChangeWindowInput) (any, error) {
	in.Title = strings.TrimSpace(in.Title)
	if !evidenceUUID.MatchString(assetID) || !validChangeText(in.Title, 120) ||
		!in.EndsAt.After(time.Now()) || in.StartsAt.IsZero() || in.EndsAt.Sub(in.StartsAt) > 7*24*time.Hour || !in.EndsAt.After(in.StartsAt) {
		return nil, ErrInvalidChange
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO change_windows(organization_id,asset_id,title,starts_at,ends_at,creator)
		SELECT a.organization_id,CASE WHEN $7 THEN NULL ELSE a.id END,$3,$4,$5,$6 FROM assets a
		WHERE a.id=$1 AND ($2::uuid IS NULL OR a.organization_id=$2) RETURNING id::text`,
		assetID, nullable(OrganizationFromContext(ctx)), in.Title, in.StartsAt, in.EndsAt, accessActor(ctx, "admin"), in.Organization).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	return map[string]string{"id": id}, err
}

const changeOrderJSON = `jsonb_build_object('id',c.id,'assetId',c.asset_id,'eventId',c.event_id,
	'windowId',c.window_id,'windowTitle',w.title,'startsAt',w.starts_at,'endsAt',w.ends_at,
	'title',c.title,'reason',c.reason,'externalId',c.external_id,'status',c.status,'requester',c.requester,
	'reviewer',c.reviewer,'createdAt',c.created_at,'reviewedAt',c.reviewed_at,'startedAt',c.started_at,
	'finishedAt',c.finished_at,'rolledBackAt',c.rolled_back_at,'baseline',c.baseline,'after',c.after_sample,
	'recovery',c.recovery_sample,'reviewNote',c.review_note,'reviewActor',c.review_actor,'reviewAt',c.review_at,
	'recoveryNote',c.recovery_note,'recoveryActor',c.recovery_actor,'recoveryAt',c.recovery_at,
	'events', (SELECT coalesce(jsonb_agg(ev ORDER BY started_at DESC),'[]'::jsonb) FROM
		(SELECT jsonb_build_object('id',id,'title',title,'status',status,'at',started_at) AS ev,started_at
		FROM events WHERE asset_id=c.asset_id AND started_at>=c.started_at
		AND started_at<=coalesce(c.rolled_back_at,c.finished_at,c.started_at)+interval '1 hour'
		ORDER BY started_at DESC LIMIT 10) recent),
	'receipts', (SELECT coalesce(jsonb_agg(r ORDER BY received_at,receipt_id),'[]'::jsonb) FROM
		(SELECT jsonb_build_object('receiptId',receipt_id,'state',state,'receivedAt',received_at) AS r,received_at,receipt_id
		FROM change_receipts WHERE change_id=c.id ORDER BY received_at DESC,receipt_id DESC LIMIT 10) receipts))`

func (s *Postgres) ChangeGovernance(ctx context.Context, assetID string) (any, error) {
	if !evidenceUUID.MatchString(assetID) {
		return nil, ErrInvalidChange
	}
	var org string
	err := s.pool.QueryRow(ctx, `SELECT organization_id::text FROM assets WHERE id=$1 AND ($2::uuid IS NULL OR organization_id=$2)`,
		assetID, nullable(OrganizationFromContext(ctx))).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	windows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'title',title,'assetId',asset_id,'startsAt',starts_at,'endsAt',ends_at)
		FROM change_windows WHERE organization_id=$1 AND (asset_id=$2 OR asset_id IS NULL) AND ends_at>now()
		ORDER BY starts_at,id LIMIT 100`, org, assetID)
	if err != nil {
		return nil, err
	}
	orders, err := s.queryJSON(ctx, `SELECT `+changeOrderJSON+` FROM change_orders c JOIN change_windows w ON w.id=c.window_id
		WHERE c.organization_id=$1 AND c.asset_id=$2 ORDER BY c.created_at DESC,c.id LIMIT 50`, org, assetID)
	return map[string]any{"windows": windows, "orders": orders}, err
}

const changeTargetRevision = `encode(sha256(convert_to(jsonb_build_array(a.host,a.port,a.database_name,a.username,
	a.ssl_mode,a.runtime,a.remote_site,a.password_enc)::text,'UTF8')),'hex')`

func (s *Postgres) RequestChange(ctx context.Context, assetID string, in ChangeOrderInput) (any, error) {
	in.Title, in.Reason = strings.TrimSpace(in.Title), strings.TrimSpace(in.Reason)
	if !evidenceUUID.MatchString(assetID) || !evidenceUUID.MatchString(in.WindowID) || !validChangeText(in.Title, 120) ||
		!validChangeText(in.Reason, 500) || !externalKey.MatchString(in.ExternalID) {
		return nil, ErrInvalidChange
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO change_orders(organization_id,asset_id,window_id,title,reason,external_id,target_revision,requester)
		SELECT a.organization_id,a.id,w.id,$4,$5,$6,`+changeTargetRevision+`,$7
		FROM assets a JOIN change_windows w ON w.id=$3 AND w.organization_id=a.organization_id
		AND (w.asset_id=a.id OR w.asset_id IS NULL) WHERE a.id=$1 AND ($2::uuid IS NULL OR a.organization_id=$2)
		AND a.runtime IN ('linux','k8s','managed','mysql') AND a.remote_site=''
		AND w.ends_at>now() ON CONFLICT(organization_id,external_id) DO NOTHING RETURNING id::text`,
		assetID, nullable(OrganizationFromContext(ctx)), in.WindowID, in.Title, in.Reason, in.ExternalID, accessActor(ctx, "admin")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChangeState
	}
	return map[string]string{"id": id}, err
}

func (s *Postgres) DecideChange(ctx context.Context, assetID, changeID string, approve bool) (any, error) {
	if !evidenceUUID.MatchString(assetID) || !evidenceUUID.MatchString(changeID) {
		return nil, ErrInvalidChange
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	result, err := s.pool.Exec(ctx, `UPDATE change_orders c SET status=$4,reviewer=$5,reviewed_at=now()
		FROM assets a,change_windows w WHERE c.id=$1 AND c.asset_id=$2 AND a.id=c.asset_id
		AND a.organization_id=c.organization_id AND ($3::uuid IS NULL OR c.organization_id=$3)
		AND w.id=c.window_id AND c.status='requested' AND c.requester<>$5
		AND (NOT $6 OR (w.ends_at>now() AND c.target_revision=`+changeTargetRevision+`))`,
		changeID, assetID, nullable(OrganizationFromContext(ctx)), status, accessActor(ctx, "approver"), approve)
	if err == nil && result.RowsAffected() != 1 {
		return nil, ErrChangeState
	}
	return map[string]string{"id": changeID, "status": status}, err
}

type changeStateRow struct {
	assetID, externalID, status, revision, currentRevision string
	startsAt, endsAt, now                                  time.Time
	baseline                                               json.RawMessage
	startedAt, finishedAt, rolledBackAt                    *time.Time
	eventID                                                *string
}

func lockChangeOrder(ctx context.Context, tx pgx.Tx, changeID, assetID, org string) (changeStateRow, error) {
	var row changeStateRow
	err := tx.QueryRow(ctx, `SELECT c.asset_id::text,c.external_id,c.status,c.target_revision,`+
		changeTargetRevision+`,w.starts_at,w.ends_at,c.baseline,c.started_at,c.finished_at,c.rolled_back_at,c.event_id::text
		FROM change_orders c JOIN change_windows w ON w.id=c.window_id JOIN assets a ON a.id=c.asset_id
		AND a.organization_id=c.organization_id WHERE c.id=$1 AND ($2::uuid IS NULL OR c.asset_id=$2)
		AND ($3::uuid IS NULL OR c.organization_id=$3) FOR UPDATE OF c,a`, changeID, nullable(assetID), nullable(org)).Scan(
		&row.assetID, &row.externalID, &row.status, &row.revision, &row.currentRevision,
		&row.startsAt, &row.endsAt, &row.baseline, &row.startedAt, &row.finishedAt, &row.rolledBackAt, &row.eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrAssetNotFound
	}
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&row.now)
	}
	return row, err
}

func (s *Postgres) RecordChangeReceipt(ctx context.Context, org string, in ChangeReceipt) (any, bool, error) {
	if !evidenceUUID.MatchString(org) || !evidenceUUID.MatchString(in.ChangeID) ||
		!externalKey.MatchString(in.ExternalID) || !externalKey.MatchString(in.ReceiptID) {
		return nil, false, ErrInvalidChange
	}
	var required string
	switch in.State {
	case "started":
		required = "approved"
	case "succeeded", "failed":
		required = "started"
	case "rolled_back":
	default:
		return nil, false, ErrInvalidChange
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	row, err := lockChangeOrder(ctx, tx, in.ChangeID, "", org)
	if err != nil {
		return nil, false, err
	}
	if row.externalID != in.ExternalID {
		return nil, false, ErrChangeState
	}
	result := map[string]any{"id": in.ChangeID, "state": in.State, "duplicate": false}
	var prior string
	err = tx.QueryRow(ctx, `SELECT state FROM change_receipts WHERE change_id=$1 AND receipt_id=$2`, in.ChangeID, in.ReceiptID).Scan(&prior)
	if err == nil {
		if prior != in.State {
			return nil, false, ErrChangeState
		}
		result["duplicate"] = true
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if row.status != required && !(in.State == "rolled_back" && (row.status == "succeeded" || row.status == "failed")) {
		return nil, false, ErrChangeState
	}
	if in.State == "started" {
		var snapshot changeSnapshot
		if row.now.Before(row.startsAt) || !row.now.Before(row.endsAt) || row.currentRevision != row.revision ||
			json.Unmarshal(row.baseline, &snapshot) != nil || snapshot.At.Before(row.now.Add(-15*time.Minute)) ||
			snapshot.At.After(row.now) || snapshot.Risk != "ready" {
			return nil, false, ErrChangeState
		}
		var latest string
		if err := tx.QueryRow(ctx, `SELECT s.id::text FROM assets a JOIN samples s ON s.asset_id=a.id AND s.collected_at=a.last_sample_at
			WHERE a.id=$1 AND s.status='ok' ORDER BY s.id DESC LIMIT 1`, row.assetID).Scan(&latest); errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrChangeState
		} else if err != nil {
			return nil, false, err
		}
		if latest != snapshot.SampleID {
			return nil, false, ErrChangeState
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM change_orders WHERE asset_id=$1 AND status='started')`, row.assetID).Scan(&running); err != nil {
			return nil, false, err
		}
		if running {
			return nil, false, ErrChangeState
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO change_receipts(change_id,receipt_id,state,received_at) VALUES($1,$2,$3,$4)`,
		in.ChangeID, in.ReceiptID, in.State, row.now); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE change_orders SET status=$2,
		started_at=CASE WHEN $2='started' THEN $3::timestamptz ELSE started_at END,
		finished_at=CASE WHEN $2 IN ('succeeded','failed') THEN $3::timestamptz ELSE finished_at END,
		rolled_back_at=CASE WHEN $2='rolled_back' THEN $3::timestamptz ELSE rolled_back_at END
		WHERE id=$1`, in.ChangeID, in.State, row.now); err != nil {
		return nil, false, err
	}
	if row.eventID != nil {
		if err := insertChangeEvidence(ctx, tx, *row.eventID, "change_receipt", row.now, map[string]any{
			"changeId": in.ChangeID, "externalId": in.ExternalID, "receiptId": in.ReceiptID, "state": in.State,
		}); err != nil {
			return nil, false, err
		}
	}
	return result, false, tx.Commit(ctx)
}
