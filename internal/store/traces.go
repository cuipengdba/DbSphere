package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func (s *Postgres) TraceAssets(ctx context.Context) ([]trace.Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,host,port,database_name FROM assets`)
	if err != nil {
		return nil, fmt.Errorf("list OTLP asset identities: %w", err)
	}
	defer rows.Close()
	assets := []trace.Asset{}
	for rows.Next() {
		var asset trace.Asset
		if err := rows.Scan(&asset.ID, &asset.Host, &asset.Port, &asset.Database); err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (s *Postgres) SaveTraceSpans(ctx context.Context, spans []trace.Span) (int, error) {
	imported, _, err := s.SaveTraceBatch(ctx, spans, nil)
	return imported, err
}

func insertTraceSpan(ctx context.Context, tx pgx.Tx, span trace.Span) (int, error) {
	var parent any
	if span.ParentSpanID != "" {
		parent = span.ParentSpanID
	}
	result, err := tx.Exec(ctx, `INSERT INTO trace_spans
		(asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status,backend_pid)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(asset_id,trace_id,span_id) DO NOTHING`, span.AssetID,
		span.TraceID, span.SpanID, parent, span.Service, span.Kind,
		span.Start, span.End, span.Status, optionalBackendPID(span.BackendPID))
	if err != nil {
		return 0, fmt.Errorf("save OTLP span: %w", err)
	}
	return int(result.RowsAffected()), nil
}

// SaveTraceBatch attaches a late service ancestor only if an already mapped
// child in the same trace points to it. Unrelated service spans are discarded.
func (s *Postgres) SaveTraceBatch(ctx context.Context, spans, candidates []trace.Span) (int, int, error) {
	if len(spans) > 4096 || len(candidates) > 256 {
		return 0, 0, fmt.Errorf("too many OTLP spans")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	imported := 0
	for _, span := range spans {
		count, err := insertTraceSpan(ctx, tx, span)
		if err != nil {
			return 0, 0, err
		}
		imported += count
	}
	byID := map[string]trace.Span{}
	for _, candidate := range candidates {
		if candidate.AssetID != "" || candidate.Kind != "service" {
			return 0, 0, fmt.Errorf("invalid unmapped service span")
		}
		byID[candidate.TraceID+":"+candidate.SpanID] = candidate
	}
	links := map[string]map[string]bool{}
	queue := []string{}
	for key, candidate := range byID {
		rows, err := tx.Query(ctx, `SELECT DISTINCT asset_id::text FROM trace_spans
			WHERE trace_id=$1 AND parent_span_id=$2 AND started_at BETWEEN $3 AND $4 LIMIT 33`,
			candidate.TraceID, candidate.SpanID, candidate.Start.Add(-5*time.Second), candidate.End.Add(5*time.Second))
		if err != nil {
			return 0, 0, fmt.Errorf("resolve OTLP parent asset: %w", err)
		}
		assets := map[string]bool{}
		for rows.Next() {
			var assetID string
			if err := rows.Scan(&assetID); err != nil {
				rows.Close()
				return 0, 0, err
			}
			assets[assetID] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, 0, err
		}
		if len(assets) > 32 {
			return 0, 0, fmt.Errorf("OTLP parent matches too many assets")
		}
		if len(assets) > 0 {
			links[key] = assets
			queue = append(queue, key)
		}
	}
	processed := map[string]map[string]bool{}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		candidate := byID[key]
		if processed[key] == nil {
			processed[key] = map[string]bool{}
		}
		for assetID := range links[key] {
			if processed[key][assetID] {
				continue
			}
			processed[key][assetID] = true
			candidate.AssetID = assetID
			count, err := insertTraceSpan(ctx, tx, candidate)
			if err != nil {
				return 0, 0, err
			}
			imported += count
			parentKey := candidate.TraceID + ":" + candidate.ParentSpanID
			parent, exists := byID[parentKey]
			if !exists || parent.Start.After(candidate.End.Add(5*time.Second)) ||
				parent.End.Before(candidate.Start.Add(-5*time.Second)) {
				continue
			}
			if links[parentKey] == nil {
				links[parentKey] = map[string]bool{}
			}
			if !links[parentKey][assetID] {
				links[parentKey][assetID] = true
				queue = append(queue, parentKey)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return imported, len(processed), nil
}

func optionalBackendPID(value int) any {
	if value > 0 {
		return value
	}
	return nil
}

func (s *Postgres) PruneTraceSpans(ctx context.Context, days int) (int64, error) {
	if days < 1 || days > 3650 {
		return 0, fmt.Errorf("invalid trace retention")
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT asset_id,trace_id,span_id FROM trace_spans
		WHERE ended_at < now() - ($1::int * interval '1 day')
		ORDER BY ended_at LIMIT 10000)
		DELETE FROM trace_spans t USING expired e
		WHERE t.asset_id=e.asset_id AND t.trace_id=e.trace_id AND t.span_id=e.span_id`, days)
	if err != nil {
		return 0, fmt.Errorf("prune OTLP spans: %w", err)
	}
	return result.RowsAffected(), nil
}
