package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

func (s *Postgres) StartSignedRoleAudit(ctx context.Context, actor, method, path string, key []byte) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	id, err := insertRoleAudit(ctx, tx, actor, method, path)
	if err != nil {
		return "", err
	}
	if err := appendOrganizationAudit(ctx, tx, id, key); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *Postgres) FinishSignedAdminAudit(ctx context.Context, id string, status int, key []byte) error {
	if !evidenceUUID.MatchString(id) || status < 100 || status > 599 {
		return ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	outcome := "rejected"
	if status >= 200 && status < 400 {
		outcome = "accepted"
	}
	row, err := tx.Exec(ctx, `UPDATE admin_audit SET outcome=$2,status_code=$3 WHERE id=$1 AND outcome='started'`, id, outcome, status)
	if err != nil {
		return err
	}
	if row.RowsAffected() != 1 {
		return ErrAccessMissing
	}
	if err := appendOrganizationAudit(ctx, tx, id, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appendOrganizationAudit(ctx context.Context, tx pgx.Tx, id string, key []byte) error {
	var organization string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT coalesce(organization_id::text,$2),
		jsonb_build_object('id',id,'observedAt',observed_at,'actor',actor,'method',method,'path',path,
			'outcome',outcome,'statusCode',status_code,'eventId',event_id)
		FROM admin_audit WHERE id=$1`, id, DefaultOrganizationID).Scan(&organization, &raw)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO organization_audit_heads(organization_id) VALUES($1) ON CONFLICT DO NOTHING`, organization); err != nil {
		return err
	}
	var sequence int64
	var previous string
	err = tx.QueryRow(ctx, `SELECT sequence,hash FROM organization_audit_heads WHERE organization_id=$1 FOR UPDATE`, organization).Scan(&sequence, &previous)
	if err != nil {
		return err
	}
	var record bytes.Buffer
	if err := json.Compact(&record, raw); err != nil {
		return err
	}
	entry := security.SignAuditEntry(key, organization, sequence+1, previous, record.Bytes())
	if _, err = tx.Exec(ctx, `INSERT INTO organization_audit_entries(organization_id,sequence,record,previous_hash,hash,signature)
		VALUES($1,$2,$3,$4,$5,$6)`, organization, entry.Sequence, []byte(entry.Record), entry.PreviousHash, entry.Hash, entry.Signature); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE organization_audit_heads SET sequence=$2,hash=$3 WHERE organization_id=$1`, organization, entry.Sequence, entry.Hash)
	return err
}

func (s *Postgres) OrganizationAudit(ctx context.Context, requested string, key []byte) (security.AuditExport, error) {
	organization, err := requestedOrganization(ctx, requested, DefaultOrganizationID)
	if err != nil {
		return security.AuditExport{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return security.AuditExport{}, err
	}
	defer tx.Rollback(ctx)
	var sequence int64
	var hash string
	err = tx.QueryRow(ctx, `SELECT coalesce(h.sequence,0),coalesce(h.hash,'') FROM organizations o
		LEFT JOIN organization_audit_heads h ON h.organization_id=o.id WHERE o.id=$1`, organization).Scan(&sequence, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return security.AuditExport{}, ErrAccessMissing
	}
	if err != nil {
		return security.AuditExport{}, err
	}
	rows, err := tx.Query(ctx, `SELECT sequence,previous_hash,hash,signature,record
		FROM organization_audit_entries WHERE organization_id=$1 ORDER BY sequence`, organization)
	if err != nil {
		return security.AuditExport{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (security.AuditEntry, error) {
		var item security.AuditEntry
		var raw []byte
		err := row.Scan(&item.Sequence, &item.PreviousHash, &item.Hash, &item.Signature, &raw)
		item.Record = raw
		return item, err
	})
	if err != nil {
		return security.AuditExport{}, err
	}
	export := security.BuildAuditExport(key, organization, sequence, hash, items)
	if err := security.VerifyAuditExport(export, export.KeyFingerprint); err != nil {
		return security.AuditExport{}, err
	}
	return export, tx.Commit(ctx)
}
