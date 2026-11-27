package store

import (
	"context"
	"errors"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

func migrateAuditDelivery(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=70)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE audit_delivery_cursor (
		organization_id uuid NOT NULL REFERENCES organizations(id), destination text NOT NULL,
		delivered_sequence bigint NOT NULL DEFAULT 0, retry_at timestamptz NOT NULL DEFAULT now(),
		attempts integer NOT NULL DEFAULT 0, lease uuid, lease_until timestamptz,
		PRIMARY KEY(organization_id,destination)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(70)`)
	return err
}

type AuditDelivery struct {
	Lease string
	Entry security.AuditEntry
}

func (s *Postgres) ClaimAuditDelivery(ctx context.Context, org, destination string, now time.Time) (*AuditDelivery, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_delivery_cursor(organization_id,destination) VALUES($1,$2)
		ON CONFLICT DO NOTHING`, org, destination); err != nil {
		return nil, err
	}
	var item AuditDelivery
	err := s.pool.QueryRow(ctx, `WITH claimed AS (
		UPDATE audit_delivery_cursor c SET lease=gen_random_uuid(), lease_until=$3+interval '30 seconds'
		WHERE c.organization_id=$1 AND c.destination=$2 AND c.retry_at<=$3
		AND (c.lease_until IS NULL OR c.lease_until<=$3)
		AND EXISTS (SELECT FROM organization_audit_entries e WHERE e.organization_id=c.organization_id AND e.sequence=c.delivered_sequence+1)
		RETURNING c.lease::text,c.delivered_sequence+1 AS sequence
	) SELECT c.lease,e.sequence,e.previous_hash,e.hash,e.signature,e.record
	FROM claimed c JOIN organization_audit_entries e ON e.organization_id=$1 AND e.sequence=c.sequence`, org, destination, now).
		Scan(&item.Lease, &item.Entry.Sequence, &item.Entry.PreviousHash, &item.Entry.Hash, &item.Entry.Signature, &item.Entry.Record)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &item, err
}

func (s *Postgres) FinishAuditDelivery(ctx context.Context, org, destination, lease string, delivered bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE audit_delivery_cursor SET
		delivered_sequence=delivered_sequence+CASE WHEN $4 THEN 1 ELSE 0 END,
		retry_at=now()+make_interval(secs=>CASE WHEN $4 THEN 0 ELSE least(300, 2 << least(attempts,8)) END),
		attempts=CASE WHEN $4 THEN 0 ELSE least(1000,attempts+1) END, lease=NULL, lease_until=NULL
		WHERE organization_id=$1 AND destination=$2 AND lease=$3`, org, destination, lease, delivered)
	return err
}
