package store

import (
	"context"
	"fmt"
	"time"
)

// PruneSamples removes a bounded number of old samples. Any sample ID cited
// by retained evidence or inspection history stays available for drill-down.
func (s *Postgres) PruneSamples(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if cutoff.IsZero() || limit < 1 || limit > 10000 {
		return 0, fmt.Errorf("invalid sample retention sweep")
	}
	result, err := s.pool.Exec(ctx, `WITH doomed AS (
		SELECT id,collected_at FROM samples candidate
		WHERE collected_at<$1
		AND NOT EXISTS (SELECT 1 FROM evidence_sample_refs refs
			WHERE refs.sample_id=candidate.id::text)
		AND NOT EXISTS (SELECT 1 FROM inspection_runs run
			WHERE run.sample_id=candidate.id)
		ORDER BY collected_at,id LIMIT $2
	) DELETE FROM samples s USING doomed d
	  WHERE s.id=d.id AND s.collected_at=d.collected_at`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune PostgreSQL 18 samples: %w", err)
	}
	return result.RowsAffected(), nil
}

// PruneResolvedEvents cascades through event_keys only after notifications are
// delivered and no active group still points at the event. Open events stay.
func (s *Postgres) PruneResolvedEvents(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if cutoff.IsZero() || limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("invalid event retention sweep")
	}
	result, err := s.pool.Exec(ctx, `WITH doomed AS (
		SELECT e.id FROM events e WHERE e.status='resolved' AND e.ended_at<$1
		AND NOT EXISTS (SELECT 1 FROM open_event_groups g WHERE g.event_id=e.id)
		AND NOT EXISTS (SELECT 1 FROM inspection_runs r
			WHERE r.event_ids_json @> jsonb_build_array(e.id::text))
		AND NOT EXISTS (SELECT 1 FROM notification_outbox n
			JOIN evidence ev ON ev.id=n.evidence_id
			WHERE ev.event_id=e.id AND n.status IN ('pending','failed'))
		ORDER BY e.ended_at,e.id LIMIT $2
	) DELETE FROM event_keys k USING doomed d WHERE k.id=d.id`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune resolved PostgreSQL 18 events: %w", err)
	}
	return result.RowsAffected(), nil
}
