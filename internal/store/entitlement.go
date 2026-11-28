package store

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/cuipengdba/dbsphere/internal/entitlement"
	"github.com/jackc/pgx/v5"
)

var ErrEntitlementConflict = errors.New("license deployment, sequence or clock changed")
var ErrEntitlementCapacity = errors.New("license does not allow additional capacity")

func migrateEntitlement(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS deployment_entitlement (
		singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
		deployment_id uuid NOT NULL DEFAULT gen_random_uuid(),
		document text NOT NULL DEFAULT '', sequence bigint NOT NULL DEFAULT 0,
		latest_use timestamptz, installed_at timestamptz, installed_by text
	);
	INSERT INTO deployment_entitlement(singleton) VALUES(true) ON CONFLICT DO NOTHING;
	INSERT INTO dbsphere_schema_migrations(version) VALUES(78) ON CONFLICT DO NOTHING`)
	return err
}

func (s *Postgres) ConfigureEntitlement(key ed25519.PublicKey) {
	s.entitlementKey = append(ed25519.PublicKey(nil), key...)
}

type EntitlementStatus struct {
	DeploymentID string               `json:"deploymentId"`
	Status       string               `json:"status"`
	Issuer       string               `json:"issuerFingerprint,omitempty"`
	AssetCount   int                  `json:"assetCount"`
	SiteCount    int                  `json:"siteCount"`
	License      *entitlement.License `json:"license,omitempty"`
	InstalledAt  *time.Time           `json:"installedAt,omitempty"`
	InstalledBy  *string              `json:"installedBy,omitempty"`
}

type entitlementRow struct {
	deployment string
	document   string
	sequence   int64
	latestUse  *time.Time
	installed  *time.Time
	actor      *string
}

func readEntitlement(ctx context.Context, tx pgx.Tx, lock bool) (entitlementRow, error) {
	var row entitlementRow
	query := `SELECT deployment_id::text,document,sequence,latest_use,installed_at,installed_by FROM deployment_entitlement WHERE singleton`
	if lock {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query).Scan(&row.deployment, &row.document, &row.sequence, &row.latestUse, &row.installed, &row.actor)
	return row, err
}

func (s *Postgres) entitlementStatus(row entitlementRow, now time.Time) EntitlementStatus {
	status := EntitlementStatus{DeploymentID: row.deployment, Status: "community", InstalledAt: row.installed, InstalledBy: row.actor}
	if len(s.entitlementKey) > 0 {
		digest := sha256.Sum256(s.entitlementKey)
		status.Issuer = hex.EncodeToString(digest[:])
		status.Status = "awaiting_license"
	}
	if row.document == "" {
		return status
	}
	license, err := entitlement.Verify([]byte(row.document), s.entitlementKey)
	if err != nil || license.Deployment != row.deployment || license.Sequence != row.sequence {
		status.Status = "invalid"
		return status
	}
	var latest time.Time
	if row.latestUse != nil {
		latest = *row.latestUse
	}
	status.License = &license
	status.Status = license.Status(now, latest)
	return status
}

func (s *Postgres) EntitlementStatus(ctx context.Context) (EntitlementStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return EntitlementStatus{}, err
	}
	defer tx.Rollback(ctx)
	row, err := readEntitlement(ctx, tx, false)
	if err != nil {
		return EntitlementStatus{}, err
	}
	status := s.entitlementStatus(row, time.Now().UTC())
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM assets),(SELECT count(*) FROM managed_sites)`).Scan(&status.AssetCount, &status.SiteCount)
	if err != nil {
		return EntitlementStatus{}, err
	}
	return status, tx.Commit(ctx)
}

func (s *Postgres) ImportEntitlement(ctx context.Context, document []byte) error {
	license, err := entitlement.Verify(document, s.entitlementKey)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	row, err := readEntitlement(ctx, tx, true)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var latest time.Time
	if row.latestUse != nil {
		latest = *row.latestUse
	}
	if license.Deployment != row.deployment || license.Status(now, latest) != "active" || license.Sequence < row.sequence {
		return ErrEntitlementConflict
	}
	if license.Sequence == row.sequence {
		if string(document) != row.document {
			return ErrEntitlementConflict
		}
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `UPDATE deployment_entitlement SET document=$1,sequence=$2,
		latest_use=greatest(latest_use,$3),installed_at=$3,installed_by=$4 WHERE singleton`,
		string(document), license.Sequence, now, accessActor(ctx, "admin"))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Call after locking the deployment row to serialize expansion across replicas.
func (s *Postgres) allowExpansion(ctx context.Context, tx pgx.Tx, row entitlementRow, assets, sites int) error {
	if assets == 0 && sites == 0 {
		return nil
	}
	now := time.Now().UTC()
	status := s.entitlementStatus(row, now)
	if status.Status == "community" {
		return nil
	}
	if status.Status != "active" {
		return ErrEntitlementCapacity
	}
	var currentAssets, currentSites int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM assets),(SELECT count(*) FROM managed_sites)`).Scan(&currentAssets, &currentSites); err != nil {
		return err
	}
	if assets > 0 && currentAssets+assets > status.License.Assets || sites > 0 && currentSites+sites > status.License.Sites {
		return ErrEntitlementCapacity
	}
	_, err := tx.Exec(ctx, `UPDATE deployment_entitlement SET latest_use=greatest(latest_use,$1) WHERE singleton`, now)
	return err
}

func (s *Postgres) CheckpointEntitlementClock(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE deployment_entitlement SET latest_use=$1 WHERE singleton AND sequence>0
		AND (latest_use IS NULL OR latest_use<$1::timestamptz-interval '1 minute')`, now.UTC())
	return err
}
