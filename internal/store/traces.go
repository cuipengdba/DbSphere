package store

import (
	"context"
	"fmt"

	"github.com/cuipengdba/dbsphere/internal/trace"
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
	if len(spans) > 4096 {
		return 0, fmt.Errorf("too many mapped spans")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	imported := 0
	for _, span := range spans {
		var parent any
		if span.ParentSpanID != "" {
			parent = span.ParentSpanID
		}
		result, err := tx.Exec(ctx, `INSERT INTO trace_spans
			(asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(asset_id,trace_id,span_id) DO NOTHING`, span.AssetID,
			span.TraceID, span.SpanID, parent, span.Service, span.Kind,
			span.Start, span.End, span.Status)
		if err != nil {
			return 0, fmt.Errorf("save OTLP span: %w", err)
		}
		imported += int(result.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return imported, nil
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
