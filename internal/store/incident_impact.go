package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidIncidentLink = errors.New("事件关联操作或原因无效")
var ErrIncidentLinkConflict = errors.New("事件已属于其他关联，或操作会形成多级事件链")

type IncidentLinkInput struct {
	Action       string `json:"action"`
	ChildEventID string `json:"childEventId"`
	Reason       string `json:"reason"`
}

func migrateIncidentImpact(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS incident_dependencies (
		event_id uuid PRIMARY KEY REFERENCES event_keys(id) ON DELETE CASCADE,
		asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		dependency text NOT NULL, entry text NOT NULL, kind text NOT NULL, signal text NOT NULL,
		sample_id uuid NOT NULL, observed_at timestamptz NOT NULL
	);
	CREATE INDEX IF NOT EXISTS incident_dependency_lookup ON incident_dependencies(dependency,signal,observed_at);
	CREATE TABLE IF NOT EXISTS incident_links (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		parent_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
		child_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
		source text NOT NULL, reason text NOT NULL, linked_at timestamptz NOT NULL DEFAULT now(),
		unlinked_at timestamptz, unlink_reason text,
		CHECK (parent_id<>child_id)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS incident_parent_once ON incident_links(child_id) WHERE unlinked_at IS NULL;
	CREATE INDEX IF NOT EXISTS incident_link_parent ON incident_links(parent_id,linked_at DESC);
	CREATE INDEX IF NOT EXISTS incident_link_child ON incident_links(child_id,linked_at DESC);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(41) ON CONFLICT DO NOTHING`)
	return err
}

func incidentLinkLock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(19131001)`)
	return err
}

func observedDependency(metrics map[string]any) (key, entry, kind, signal string) {
	if object(metrics["collector"])["pgStatus"] != "ok" {
		return "", "", "", ""
	}
	route := object(metrics["routing"])
	kind, entry = stringValue(route["kind"]), stringValue(route["entry"])
	status := stringValue(route["status"])
	probeStatus := stringValue(object(route["probe"])["status"])
	if probeStatus != "dns_error" && probeStatus != "unreachable" {
		return "", "", "", ""
	}
	if status != "dns_error" && status != "unreachable" && status != "ready_pod_entry_unreachable" {
		return "", "", "", ""
	}
	host, port, err := net.SplitHostPort(entry)
	if err != nil {
		return "", "", "", ""
	}
	dependency := net.JoinHostPort(strings.ToLower(strings.TrimSuffix(host, ".")), port)
	switch kind {
	case "pgbouncer", "haproxy":
		return kind + "|" + dependency, entry, kind, probeStatus
	case "k8s_service":
		kube := object(metrics["k8s"])
		cluster := stringValue(kube["cluster"])
		serviceUID := stringValue(object(route["service"])["uid"])
		if cluster != "" && serviceUID != "" && status == "ready_pod_entry_unreachable" {
			return kind + "|" + cluster + "|" + serviceUID + "|" + dependency, entry, kind, probeStatus
		}
	}
	return "", "", "", ""
}

func recordDependencyIncident(ctx context.Context, tx pgx.Tx, asset, event, sample string, at time.Time, metrics map[string]any) error {
	key, entry, kind, signal := observedDependency(metrics)
	if key == "" {
		return nil
	}
	if err := incidentLinkLock(ctx, tx); err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO incident_dependencies(event_id,asset_id,dependency,entry,kind,signal,sample_id,observed_at)
		SELECT id,asset_id,$3,$4,$5,$6,$7,$8 FROM events WHERE id=$1 AND asset_id=$2 AND started_at=$8 AND status='open'
		ON CONFLICT(event_id) DO NOTHING`, event, asset, key, entry, kind, signal, sample, at)
	if err != nil || inserted.RowsAffected() == 0 {
		return err
	}
	var parent string
	err = tx.QueryRow(ctx, `SELECT root.id::text
		FROM incident_dependencies d
		LEFT JOIN incident_links link ON link.child_id=d.event_id AND link.unlinked_at IS NULL
		JOIN events root ON root.id=coalesce(link.parent_id,d.event_id) AND root.status='open'
		JOIN events peer ON peer.id=d.event_id AND peer.status='open'
		JOIN incident_dependencies cause ON cause.event_id=root.id AND cause.dependency=d.dependency AND cause.signal=d.signal
		WHERE d.asset_id<>$1 AND root.asset_id<>$1 AND d.dependency=$2 AND d.signal=$3
		AND d.observed_at BETWEEN $4::timestamptz-interval '2 minutes' AND $4::timestamptz+interval '2 minutes'
		AND NOT EXISTS (SELECT 1 FROM incident_links old WHERE old.child_id=d.event_id AND old.unlinked_at IS NOT NULL)
		ORDER BY root.started_at,root.id LIMIT 1`, asset, key, signal, at).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO incident_links(parent_id,child_id,source,reason)
		VALUES($1,$2,'dependency','同一访问入口出现同类网络故障，两个资产的数据库直连正常')`, parent, event)
	return err
}

func (s *Postgres) UpdateIncidentLink(ctx context.Context, parent string, input IncidentLinkInput) error {
	note := strings.TrimSpace(input.Reason)
	if !evidenceUUID.MatchString(input.ChildEventID) || input.ChildEventID == parent ||
		(input.Action != "link" && input.Action != "split") || note == "" || len(note) > 600 ||
		strings.ContainsAny(note, "\r\n\x00") {
		return ErrInvalidIncidentLink
	}
	child := input.ChildEventID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := incidentLinkLock(ctx, tx); err != nil {
		return err
	}
	var found, distinct bool
	if err := tx.QueryRow(ctx, `SELECT count(*)=2,count(DISTINCT asset_id)=2 FROM events WHERE id=$1 OR id=$2`,
		parent, child).Scan(&found, &distinct); err != nil {
		return err
	}
	if !found {
		return ErrEventNotFound
	}
	if !distinct {
		return ErrInvalidIncidentLink
	}
	if input.Action == "split" {
		proof, err := tx.Exec(ctx, `UPDATE incident_links SET unlinked_at=now(),unlink_reason=$3
			WHERE parent_id=$1 AND child_id=$2 AND unlinked_at IS NULL`, parent, child, note)
		if err != nil {
			return err
		}
		if proof.RowsAffected() == 0 {
			var previous bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM incident_links WHERE parent_id=$1 AND child_id=$2)`,
				parent, child).Scan(&previous); err != nil {
				return err
			}
			if !previous {
				return ErrIncidentLinkConflict
			}
		}
		return tx.Commit(ctx)
	}
	var existing, conflict bool
	err = tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM incident_links WHERE parent_id=$1 AND child_id=$2 AND unlinked_at IS NULL),
		EXISTS(SELECT 1 FROM incident_links WHERE unlinked_at IS NULL AND
			(child_id=$1 OR parent_id=$2 OR (child_id=$2 AND parent_id<>$1)))`, parent, child).Scan(&existing, &conflict)
	if err != nil {
		return err
	}
	if conflict {
		return ErrIncidentLinkConflict
	}
	if !existing {
		if _, err := tx.Exec(ctx, `INSERT INTO incident_links(parent_id,child_id,source,reason)
			VALUES($1,$2,'operator',$3)`, parent, child, note); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Postgres) IncidentImpact(ctx context.Context, id string, scope []string) (any, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var primary string
	err = tx.QueryRow(ctx, `SELECT coalesce(parent.id,e.id)::text
		FROM events e LEFT JOIN incident_links link ON link.child_id=e.id AND link.unlinked_at IS NULL
		LEFT JOIN events parent ON parent.id=link.parent_id AND ($2::uuid[] IS NULL OR parent.asset_id=ANY($2))
		WHERE e.id=$1 AND ($2::uuid[] IS NULL OR e.asset_id=ANY($2))`, id, scope).Scan(&primary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrEventNotFound
	}
	if err != nil {
		return nil, err
	}
	members, err := queryJSONRows(ctx, tx, `WITH selected AS (
		SELECT e.* FROM events e WHERE (e.id=$1 OR e.id IN
			(SELECT child_id FROM incident_links WHERE parent_id=$1 AND unlinked_at IS NULL))
		AND ($2::uuid[] IS NULL OR e.asset_id=ANY($2))
		ORDER BY (e.id=$1) DESC,e.started_at,e.id LIMIT 101
	) SELECT jsonb_build_object(
		'eventId',e.id,'assetId',e.asset_id,'assetName',a.name,'cluster',a.cluster_name,'title',e.title,
		'startedAt',e.started_at,'endedAt',e.ended_at,'status',e.status,'source',link.source,'reason',link.reason,
		'dependency',CASE WHEN d.event_id IS NULL THEN NULL ELSE jsonb_build_object('entry',d.entry,'kind',d.kind,
			'signal',d.signal,'sampleId',d.sample_id,'observedAt',d.observed_at) END,
		'services',coalesce(failed.services,'[]'::jsonb))
		FROM selected e JOIN assets a ON a.id=e.asset_id
		LEFT JOIN incident_links link ON link.child_id=e.id AND link.parent_id=$1 AND link.unlinked_at IS NULL
		LEFT JOIN incident_dependencies d ON d.event_id=e.id
		LEFT JOIN LATERAL (
			SELECT jsonb_agg(jsonb_build_object('name',service,'failures',n) ORDER BY n DESC,service) AS services
			FROM (SELECT service,count(*) AS n FROM trace_spans WHERE asset_id=e.asset_id AND kind='postgresql_client'
				AND status='error' AND started_at BETWEEN e.started_at-interval '5 minutes'
				AND least(coalesce(e.ended_at,now())+interval '5 minutes',e.started_at+interval '24 hours')
				GROUP BY service ORDER BY n DESC,service LIMIT 20) top
		) failed ON true
		ORDER BY (e.id=$1) DESC,e.started_at,e.id`, primary, scope)
	if err != nil {
		return nil, err
	}
	history, err := queryJSONRows(ctx, tx, `SELECT jsonb_build_object(
		'id',l.id,'parentEventId',l.parent_id,'parentAsset',pa.name,'childEventId',l.child_id,'childAsset',ca.name,
		'source',l.source,'reason',l.reason,'linkedAt',l.linked_at,'unlinkedAt',l.unlinked_at,'unlinkReason',l.unlink_reason)
		FROM incident_links l JOIN events p ON p.id=l.parent_id JOIN events c ON c.id=l.child_id
		JOIN assets pa ON pa.id=p.asset_id JOIN assets ca ON ca.id=c.asset_id
		WHERE (l.parent_id=$1 OR l.child_id=$1 OR l.parent_id=$3)
		AND ($2::uuid[] IS NULL OR (p.asset_id=ANY($2) AND c.asset_id=ANY($2)))
		ORDER BY l.linked_at DESC,l.id DESC LIMIT 100`, id, scope, primary)
	if err != nil {
		return nil, err
	}
	limited := len(members) > 100
	if limited {
		members = members[:100]
	}
	return map[string]any{"primaryEventId": primary, "members": members, "history": history, "truncated": limited}, nil
}
