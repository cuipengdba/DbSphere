package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) EmergencyAdminWindow(ctx context.Context) (string, error) {
	var until string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='emergency_admin_until'
		AND value::timestamptz>now()`).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return until, err
}

// SetEmergencyAdmin is a host-operated recovery control. The enable or review
// operation and its signed audit are committed together.
func (s *Postgres) SetEmergencyAdmin(ctx context.Context, minutes int, master []byte) error {
	if minutes < 0 || minutes > 60 {
		return ErrAccessInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var initialized bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM settings WHERE key='admin_password_hash' AND value<>'')`).Scan(&initialized); err != nil {
		return err
	}
	if !initialized {
		return ErrAccessMissing
	}
	path := "/api/admin/emergency/review"
	if minutes > 0 {
		path = "/api/admin/emergency/enable"
		_, err = tx.Exec(ctx, `INSERT INTO settings(key,value) VALUES('emergency_admin_until',$1)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, time.Now().UTC().Add(time.Duration(minutes)*time.Minute).Format(time.RFC3339Nano))
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM settings WHERE key='emergency_admin_until'`)
	}
	if err != nil {
		return err
	}
	id, err := insertRoleAudit(ctx, tx, "admin", "POST", path)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_audit SET outcome='accepted',status_code=200 WHERE id=$1`, id); err != nil {
		return err
	}
	if err := appendOrganizationAudit(ctx, tx, id, master); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
