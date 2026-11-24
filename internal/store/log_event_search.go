package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateLogEventSearch(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=58)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE INDEX log_instances_linked_asset ON log_instances(asset_id) WHERE asset_id IS NOT NULL;
		CREATE INDEX log_investigations_event ON log_investigations(event_id,created_at,id) WHERE event_id IS NOT NULL;
		INSERT INTO dbsphere_schema_migrations(version) VALUES (58)`)
	return err
}

type logEventWindow struct {
	asset            string
	anchor, from, to time.Time
}

func (s *Postgres) logEventWindow(ctx context.Context, event string) (logEventWindow, error) {
	var window logEventWindow
	err := s.pool.QueryRow(ctx, `SELECT a.id::text,
		coalesce(CASE WHEN e.kind='log_analysis' THEN (SELECT r.observed_at FROM log_investigations i
			JOIN log_records r ON r.id=i.primary_record WHERE i.event_id=k.id ORDER BY i.created_at,i.id LIMIT 1)
			END,k.started_at)
		FROM event_keys k JOIN assets a ON a.id=k.asset_id
		JOIN events e ON e.id=k.id AND e.started_at=k.started_at
		WHERE k.id=$1 AND ($2='' OR a.organization_id::text=$2)`, event, OrganizationFromContext(ctx)).
		Scan(&window.asset, &window.anchor)
	if err != nil {
		return window, logWriteError(err)
	}
	window.from = window.anchor.Add(-30 * time.Minute)
	window.to = window.anchor.Add(30 * time.Minute)
	return window, nil
}

func (s *Postgres) LogEventCandidates(ctx context.Context, event string) (any, error) {
	window, err := s.logEventWindow(ctx, event)
	if err != nil {
		return nil, err
	}
	const limit = 50
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',r.id,'batchId',b.id,'batch',b.name,
		'at',r.observed_at,'application',r.application,'database',r.database_name,'process',r.process_id,
		'blocker',r.blocker_id,'kind',r.kind,'sqlState',r.sqlstate,'fingerprint',r.fingerprint,'sql',r.sql_shape,
		'durationMs',r.duration_ms,'retained',f.archive_expired_at IS NULL AND f.stored_size>0,'caseId',linked.id)`+
		logBatchScope+` JOIN assets a ON a.id=i.asset_id AND a.organization_id=p.organization_id
		JOIN log_records r ON r.batch_id=b.id JOIN log_files f ON f.id=r.file_id
		LEFT JOIN LATERAL (
			SELECT inv.id FROM log_investigations inv JOIN log_investigation_records j ON j.investigation_id=inv.id
			WHERE inv.event_id=$1 AND j.record_id=r.id ORDER BY inv.created_at,inv.id LIMIT 1
		) linked ON true
		WHERE a.id=$2 AND ($3='' OR p.organization_id::text=$3)
			AND r.observed_at BETWEEN $4 AND $5
			AND (r.kind IN ('lock','deadlock','error','temp') OR (r.kind='statement' AND (r.duration_ms>=1000 OR linked.id IS NOT NULL)))
		ORDER BY linked.id IS NULL, CASE r.kind WHEN 'deadlock' THEN 0 WHEN 'lock' THEN 1 WHEN 'error' THEN 2 ELSE 3 END,
			abs(extract(epoch FROM r.observed_at-$6::timestamptz)),r.id LIMIT $7`,
		event, window.asset, OrganizationFromContext(ctx), window.from, window.to, window.anchor, limit+1)
	if err != nil {
		return nil, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return map[string]any{"eventId": event, "from": window.from, "to": window.to, "records": rows, "limited": more}, nil
}

func (s *Postgres) AdoptEventLogRecord(ctx context.Context, event, record string) (any, error) {
	window, err := s.logEventWindow(ctx, event)
	if err != nil {
		return nil, err
	}
	return s.startLogInvestigation(ctx, record, event, &window)
}
