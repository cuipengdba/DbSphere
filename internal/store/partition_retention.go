package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DropEmptyExpiredPartitions reclaims empty historical monthly partitions.
// A partition with even one retained row is never removed.
func (s *Postgres) DropEmptyExpiredPartitions(ctx context.Context, sampleCutoff, eventCutoff time.Time, limit int) (int, error) {
	if limit < 1 || limit > 4 {
		return 0, fmt.Errorf("invalid partition retention sweep")
	}
	if sampleCutoff.IsZero() && eventCutoff.IsZero() {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin partition retention sweep: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return 0, fmt.Errorf("lock partition retention sweep: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
		return 0, fmt.Errorf("set partition retention timezone: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL DateStyle TO 'ISO, MDY'"); err != nil {
		return 0, fmt.Errorf("set partition retention date style: %w", err)
	}
	type candidate struct{ table, name, bound string }
	var candidates []candidate
	now := time.Now().UTC()
	for _, target := range []struct {
		table  string
		cutoff time.Time
	}{{"events", eventCutoff}, {"samples", sampleCutoff}} {
		if target.cutoff.IsZero() {
			continue
		}
		rows, err := tx.Query(ctx, `SELECT c.relname,pg_get_expr(c.relpartbound,c.oid)
			FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE i.inhparent=to_regclass($1) AND n.nspname='public'
			ORDER BY c.relname`, "public."+target.table)
		if err != nil {
			return 0, fmt.Errorf("list %s partitions: %w", target.table, err)
		}
		for rows.Next() {
			var name, bound string
			if err := rows.Scan(&name, &bound); err != nil {
				rows.Close()
				return 0, fmt.Errorf("read %s partition: %w", target.table, err)
			}
			prefix := target.table + "_"
			if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+6 {
				continue
			}
			start, err := time.Parse("200601", strings.TrimPrefix(name, prefix))
			if err != nil {
				continue
			}
			end := start.AddDate(0, 1, 0)
			if end.After(target.cutoff.UTC()) || end.After(now) {
				continue
			}
			expected := fmt.Sprintf("FOR VALUES FROM ('%s 00:00:00+00') TO ('%s 00:00:00+00')",
				start.Format("2006-01-02"), end.Format("2006-01-02"))
			if bound != expected {
				rows.Close()
				return 0, fmt.Errorf("partition %s has unexpected bounds", name)
			}
			candidates = append(candidates, candidate{target.table, name, bound})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("iterate %s partitions: %w", target.table, err)
		}
		rows.Close()
	}
	removed := 0
	for _, part := range candidates {
		if removed >= limit {
			break
		}
		name := pgx.Identifier{"public", part.name}.Sanitize()
		var empty bool
		if err := tx.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM "+name+" LIMIT 1)").Scan(&empty); err != nil {
			return 0, fmt.Errorf("check expired partition %s rows: %w", part.name, err)
		}
		if !empty {
			continue
		}
		parent := pgx.Identifier{"public", part.table}.Sanitize()
		if _, err := tx.Exec(ctx, "LOCK TABLE "+parent+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return 0, fmt.Errorf("lock partition parent %s: %w", part.table, err)
		}
		if _, err := tx.Exec(ctx, "LOCK TABLE "+name+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return 0, fmt.Errorf("lock expired partition %s: %w", part.name, err)
		}
		var attached bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			WHERE i.inhrelid=to_regclass($1) AND i.inhparent=to_regclass($2)
			AND pg_get_expr(c.relpartbound,c.oid)=$3)`, "public."+part.name,
			"public."+part.table, part.bound).Scan(&attached); err != nil || !attached {
			return 0, fmt.Errorf("recheck expired partition %s attachment: %v", part.name, err)
		}
		if err := tx.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM "+name+" LIMIT 1)").Scan(&empty); err != nil {
			return 0, fmt.Errorf("check expired partition %s rows: %w", part.name, err)
		}
		if !empty {
			continue
		}
		if _, err := tx.Exec(ctx, "DROP TABLE "+name); err != nil {
			return 0, fmt.Errorf("drop empty expired partition %s: %w", part.name, err)
		}
		removed++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit partition retention sweep: %w", err)
	}
	return removed, nil
}
