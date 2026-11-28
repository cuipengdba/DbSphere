package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/jackc/pgx/v5"
)

var ErrSiteUnavailable = errors.New("site not visible or not registered")
var ErrSiteCapacity = errors.New("site snapshot exceeds the asset limit")
var ErrSiteTransition = errors.New("site upgrade revision or transition is invalid")

func migrateSiteManagement(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS managed_sites (
		site text PRIMARY KEY, organization_id uuid NOT NULL REFERENCES organizations(id),
		label text NOT NULL, console_url text NOT NULL, asset_cap int NOT NULL CHECK(asset_cap BETWEEN 1 AND 10000),
		delegate_id uuid REFERENCES access_accounts(id) ON DELETE SET NULL,
		summary jsonb, summary_id uuid, summary_at timestamptz, summary_digest bytea, received_at timestamptz,
		rejected_assets int, rejected_at timestamptz,
		updated_at timestamptz NOT NULL DEFAULT now(), updated_by text NOT NULL
	);
	CREATE INDEX IF NOT EXISTS managed_sites_org ON managed_sites(organization_id,site);
	CREATE TABLE IF NOT EXISTS site_upgrade_history (
		site text NOT NULL REFERENCES managed_sites(site), revision int NOT NULL,
		batch text NOT NULL, wave int NOT NULL CHECK(wave BETWEEN 1 AND 100),
		from_version text NOT NULL, target_version text NOT NULL,
		state text NOT NULL CHECK(state IN ('scheduled','upgrading','verified','failed','rolling_back','rolled_back')),
		note text NOT NULL, actor text NOT NULL, changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
		observed_summary_id uuid, PRIMARY KEY(site,revision)
	);
	CREATE INDEX IF NOT EXISTS site_upgrade_batch ON site_upgrade_history(batch,wave,site,revision DESC);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(77) ON CONFLICT DO NOTHING`)
	return err
}

type ManagedSiteInput struct {
	Site         string `json:"site"`
	Organization string `json:"organization"`
	Label        string `json:"label"`
	ConsoleURL   string `json:"consoleUrl"`
	AssetCap     int    `json:"assetCap"`
	Delegate     string `json:"delegateAccountId"`
}

func (s *Postgres) SaveManagedSite(ctx context.Context, in ManagedSiteInput) error {
	org, err := requestedOrganization(ctx, in.Organization, DefaultOrganizationID)
	console, parseErr := url.Parse(in.ConsoleURL)
	in.Label = strings.TrimSpace(in.Label)
	if err != nil || !remoteobs.ValidSite(in.Site) || in.Site == "local" || !validChangeText(in.Label, 100) ||
		in.AssetCap < 1 || in.AssetCap > remoteobs.MaxSiteAssets || (in.Delegate != "" && !remoteobs.ValidID(in.Delegate)) ||
		parseErr != nil || console.Scheme != "https" || console.Hostname() == "" || console.User != nil ||
		console.RawQuery != "" || console.Fragment != "" || len(in.ConsoleURL) > 512 || strings.ContainsAny(in.ConsoleURL, "\\\r\n\t") {
		return remoteobs.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	license, err := readEntitlement(ctx, tx, true)
	if err != nil {
		return err
	}
	var existing bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM managed_sites WHERE site=$1)`, in.Site).Scan(&existing); err != nil {
		return err
	}
	if !existing {
		if err := s.allowExpansion(ctx, tx, license, 0, 1); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `INSERT INTO managed_sites(site,organization_id,label,console_url,asset_cap,delegate_id,updated_by)
		SELECT $1,$2,$3,$4,$5,$6,$7 WHERE EXISTS(SELECT FROM organizations WHERE id=$2)
		AND ($6::uuid IS NULL OR EXISTS(SELECT FROM access_accounts WHERE id=$6 AND organization_id=$2 AND role='operator' AND enabled))
		ON CONFLICT(site) DO UPDATE SET label=excluded.label,console_url=excluded.console_url,asset_cap=excluded.asset_cap,
			delegate_id=excluded.delegate_id,updated_by=excluded.updated_by,updated_at=now()
		WHERE managed_sites.organization_id=excluded.organization_id`, in.Site, org, in.Label, console.String(),
		in.AssetCap, nullable(in.Delegate), accessActor(ctx, "admin"))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrSiteUnavailable
	}
	return tx.Commit(ctx)
}

const managedSiteScope = `($1::uuid IS NULL OR m.organization_id=$1) AND ($2::boolean OR m.delegate_id=$3::uuid)`

func (s *Postgres) ManagedSites(ctx context.Context, allOrganizationSites bool, delegateID string) (any, error) {
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('site',m.site,'organizationId',m.organization_id,'organization',o.name,
		'label',m.label,'consoleUrl',m.console_url,'assetCap',m.asset_cap,'delegateAccountId',CASE WHEN $2 THEN m.delegate_id END,
		'summary',m.summary,'receivedAt',m.received_at,'rejectedAssets',m.rejected_assets,'rejectedAt',m.rejected_at,
		'siteStatus',CASE WHEN m.rejected_assets>m.asset_cap THEN 'over_capacity' WHEN m.summary IS NULL THEN 'pending'
		WHEN (m.summary->>'assetCount')::int>m.asset_cap THEN 'over_capacity'
		WHEN least(m.received_at,m.summary_at)<now()-make_interval(secs=>3*(m.summary->>'intervalSeconds')::int) THEN 'disconnected'
		ELSE 'connected' END,'upgrade',u.value)
		FROM managed_sites m JOIN organizations o ON o.id=m.organization_id
		LEFT JOIN LATERAL (SELECT `+siteUpgradeJSON+` AS value FROM site_upgrade_history h WHERE h.site=m.site ORDER BY revision DESC LIMIT 1) u ON true
		WHERE `+managedSiteScope+` ORDER BY o.name,m.site LIMIT 250`, nullable(OrganizationFromContext(ctx)), allOrganizationSites, nullable(delegateID))
	return map[string]any{"sites": rows}, err
}

func (s *Postgres) SaveSiteSummary(ctx context.Context, site string, in remoteobs.SiteSummary) (remoteobs.Receipt, error) {
	now := time.Now().UTC()
	if !remoteobs.ValidSite(site) || in.Validate(now) != nil {
		return remoteobs.Receipt{}, remoteobs.ErrInvalid
	}
	data, err := json.Marshal(in)
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	digest := sha256.Sum256(data)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	defer tx.Rollback(ctx)
	var cap int
	var previousID *string
	var previousAt *time.Time
	var previousDigest []byte
	err = tx.QueryRow(ctx, `SELECT asset_cap,summary_id::text,summary_at,summary_digest FROM managed_sites WHERE site=$1 FOR UPDATE`, site).
		Scan(&cap, &previousID, &previousAt, &previousDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return remoteobs.Receipt{}, ErrSiteUnavailable
	}
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	if in.AssetCount > cap {
		if _, err := tx.Exec(ctx, `UPDATE managed_sites SET rejected_assets=$2,rejected_at=$3
			WHERE site=$1 AND (summary_at IS NULL OR summary_at<$4) AND (rejected_at IS NULL OR rejected_at<=$3)`,
			site, in.AssetCount, now, in.ObservedAt); err != nil {
			return remoteobs.Receipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return remoteobs.Receipt{}, err
		}
		return remoteobs.Receipt{}, ErrSiteCapacity
	}
	status := "stored"
	if previousID != nil && *previousID == in.ID {
		if string(previousDigest) != string(digest[:]) {
			return remoteobs.Receipt{}, ErrRemoteConflict
		}
		status = "duplicate"
	} else if previousAt != nil && !in.ObservedAt.Truncate(time.Microsecond).After(*previousAt) {
		status = "superseded"
	} else {
		_, err = tx.Exec(ctx, `UPDATE managed_sites SET summary=$2,summary_id=$3,summary_at=$4,summary_digest=$5,received_at=$6,
			rejected_assets=NULL,rejected_at=NULL WHERE site=$1`,
			site, data, in.ID, in.ObservedAt, digest[:], now)
		if err != nil {
			return remoteobs.Receipt{}, err
		}
	}
	return remoteobs.Receipt{ID: in.ID, Status: status}, tx.Commit(ctx)
}

// ReadSiteSummary returns only the selected local organization's aggregate data.
func (s *Postgres) ReadSiteSummary(ctx context.Context, org string, interval int, point *remoteobs.SiteSummary) error {
	var data []byte
	err := s.pool.QueryRow(ctx, `WITH visible AS MATERIALIZED (SELECT id,last_status,last_sample_at,remote_interval_seconds FROM assets WHERE organization_id=$1),
		counts AS (SELECT count(*) AS assets, count(*) FILTER(WHERE last_status='ok' AND last_sample_at >= $2::timestamptz-make_interval(secs=>2*coalesce(remote_interval_seconds,$3::int))) AS healthy,
			count(*) FILTER(WHERE last_status='error' AND last_sample_at >= $2::timestamptz-make_interval(secs=>2*coalesce(remote_interval_seconds,$3::int))) AS failed FROM visible),
		recent_events AS (SELECT e.id,e.started_at,e.status FROM events e JOIN visible a ON a.id=e.asset_id
			WHERE e.status='open' OR (e.status='resolved' AND e.ended_at >= $2::timestamptz-interval '1 day'))
		SELECT jsonb_build_object('assetCount',assets,'healthy',healthy,'failed',failed,'missing',assets-healthy-failed,
			'openEvents',(SELECT count(*) FROM recent_events WHERE status='open'),
			'resolvedEventsLast24h',(SELECT count(*) FROM recent_events WHERE status='resolved'),
			'signedReportsLast30d',(SELECT count(*) FROM service_reports WHERE organization_id=$1 AND signed_at>=$2::timestamptz-interval '30 days'),
			'events',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'startedAt',started_at,'status',status)
				ORDER BY (status='open') DESC,started_at DESC,id) FROM
				(SELECT * FROM recent_events ORDER BY (status='open') DESC,started_at DESC,id LIMIT $4) d),'[]'::jsonb)) FROM counts
		WHERE EXISTS(SELECT FROM organizations WHERE id=$1)`,
		org, point.ObservedAt, interval, remoteobs.MaxSiteEvents).Scan(&data)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, point)
}
