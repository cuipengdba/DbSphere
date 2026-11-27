package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/jackc/pgx/v5"
)

type SiteUpgradeInput struct {
	Revision      int    `json:"revision"`
	State         string `json:"state"`
	Batch         string `json:"batch"`
	Wave          int    `json:"wave"`
	TargetVersion string `json:"targetVersion"`
	Note          string `json:"note"`
}

const siteUpgradeJSON = `jsonb_build_object('revision',h.revision,'batch',h.batch,'wave',h.wave,
	'fromVersion',h.from_version,'targetVersion',h.target_version,'state',h.state,'note',h.note,
	'actor',h.actor,'changedAt',h.changed_at,'observedSummaryId',h.observed_summary_id)`

func (s *Postgres) SiteUpgradeHistory(ctx context.Context, site string, admin bool, delegate string) (any, error) {
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('site',m.site,'history',
		coalesce((SELECT jsonb_agg(record ORDER BY revision DESC) FROM
		(SELECT h.revision,`+siteUpgradeJSON+` AS record FROM site_upgrade_history h WHERE h.site=m.site ORDER BY h.revision DESC LIMIT 100) changes),'[]'::jsonb))
		FROM managed_sites m WHERE `+managedSiteScope+` AND m.site=$4`, nullable(OrganizationFromContext(ctx)), admin, nullable(delegate), site).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSiteUnavailable
	}
	return result, err
}

func (s *Postgres) RecordSiteUpgrade(ctx context.Context, site string, admin bool, delegate string, in SiteUpgradeInput) error {
	in.Note = strings.TrimSpace(in.Note)
	if !remoteobs.ValidSite(site) || in.Revision < 0 || !validChangeText(in.Note, 500) {
		return remoteobs.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize upgrade planning and wave transitions across an organization.
	var org string
	err = tx.QueryRow(ctx, `SELECT m.organization_id::text FROM managed_sites m WHERE `+managedSiteScope+` AND m.site=$4`,
		nullable(OrganizationFromContext(ctx)), admin, nullable(delegate), site).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSiteUnavailable
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('dbsphere-site-upgrades:'||$1,0))`, org); err != nil {
		return err
	}
	var currentVersion, summaryID string
	var summaryAt *time.Time
	var fresh bool
	err = tx.QueryRow(ctx, `SELECT coalesce(m.summary->>'version',''),coalesce(m.summary_id::text,''),m.summary_at,
		coalesce(m.rejected_at IS NULL AND (m.summary->>'assetCount')::int<=m.asset_cap AND
			least(m.summary_at,m.received_at)>=now()-make_interval(secs=>3*(m.summary->>'intervalSeconds')::int),false)
		FROM managed_sites m WHERE `+managedSiteScope+` AND m.site=$4 FOR UPDATE OF m`,
		nullable(OrganizationFromContext(ctx)), admin, nullable(delegate), site).Scan(&currentVersion, &summaryID, &summaryAt, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSiteUnavailable
	}
	if err != nil {
		return err
	}
	var revision, wave int
	var batch, from, target, state string
	var changed time.Time
	err = tx.QueryRow(ctx, `SELECT revision,batch,wave,from_version,target_version,state,changed_at FROM site_upgrade_history
		WHERE site=$1 ORDER BY revision DESC LIMIT 1`, site).Scan(&revision, &batch, &wave, &from, &target, &state, &changed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if revision != in.Revision {
		return ErrSiteTransition
	}
	var observed any
	switch in.State {
	case "scheduled":
		if !admin || (state != "" && state != "verified" && state != "rolled_back") || !fresh ||
			!remoteobs.ValidSite(in.Batch) || in.Wave < 1 || in.Wave > 100 ||
			!remoteobs.ValidSiteRelease(in.TargetVersion) || currentVersion == in.TargetVersion {
			return ErrSiteTransition
		}
		var started bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM site_upgrade_history h JOIN managed_sites m ON m.site=h.site
			WHERE m.organization_id=$1 AND h.batch=$2 AND h.wave>=$3 AND h.state<>'scheduled')`, org, in.Batch, in.Wave).Scan(&started); err != nil {
			return err
		}
		if started {
			return ErrSiteTransition
		}
		batch, wave, from, target = in.Batch, in.Wave, currentVersion, in.TargetVersion
	case "upgrading":
		if state != "scheduled" || !fresh || currentVersion != from {
			return ErrSiteTransition
		}
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM (
			SELECT DISTINCT ON (h.site) h.state FROM site_upgrade_history h JOIN managed_sites m ON m.site=h.site
			WHERE m.organization_id=$1 AND h.batch=$2 AND h.wave<$3 ORDER BY h.site,h.revision DESC
		) prior WHERE prior.state<>'verified') OR ($3::int>1 AND NOT EXISTS(SELECT FROM site_upgrade_history h
			JOIN managed_sites m ON m.site=h.site WHERE m.organization_id=$1 AND h.batch=$2 AND h.wave=$3::int-1))`, org, batch, wave).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return ErrSiteTransition
		}
	case "verified", "rolled_back":
		if !fresh || summaryAt == nil || !summaryAt.After(changed) ||
			(in.State == "verified" && (state != "upgrading" || currentVersion != target)) ||
			(in.State == "rolled_back" && (state != "rolling_back" || currentVersion != from)) {
			return ErrSiteTransition
		}
		observed = summaryID
	case "failed":
		if state != "upgrading" {
			return ErrSiteTransition
		}
	case "rolling_back":
		if state != "failed" && state != "upgrading" && state != "verified" {
			return ErrSiteTransition
		}
	default:
		return remoteobs.ErrInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO site_upgrade_history(site,revision,batch,wave,from_version,target_version,state,note,actor,observed_summary_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, site, revision+1, batch, wave, from, target, in.State, in.Note, accessActor(ctx, "admin"), observed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
