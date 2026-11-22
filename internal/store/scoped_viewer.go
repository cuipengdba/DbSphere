package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) AssetIDForResource(ctx context.Context, kind, id string) (string, error) {
	var query string
	switch kind {
	case "event":
		query = "SELECT asset_id::text FROM event_keys WHERE id=$1"
	case "inspection":
		query = "SELECT asset_id::text FROM inspection_runs WHERE id=$1"
	case "work_order":
		query = "SELECT asset_id::text FROM work_orders WHERE id=$1"
	default:
		return "", errors.New("unsupported scoped resource")
	}
	var assetID string
	if err := s.pool.QueryRow(ctx, query, id).Scan(&assetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read scoped resource ownership: %w", err)
	}
	return assetID, nil
}
