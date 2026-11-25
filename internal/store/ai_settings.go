package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) ReadAISettings(ctx context.Context) (string, error) {
	var value string
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='ai_runtime_v1'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Postgres) CompareAISettings(ctx context.Context, before, after string) (bool, error) {
	switch {
	case before == "" && after == "":
		var unchanged bool
		err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM settings WHERE key='ai_runtime_v1')`).Scan(&unchanged)
		return unchanged, err
	case before == "":
		tag, err := s.pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES('ai_runtime_v1',$1) ON CONFLICT DO NOTHING`, after)
		return tag.RowsAffected() == 1, err
	case after == "":
		tag, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key='ai_runtime_v1' AND value=$1`, before)
		return tag.RowsAffected() == 1, err
	default:
		tag, err := s.pool.Exec(ctx, `UPDATE settings SET value=$2 WHERE key='ai_runtime_v1' AND value=$1`, before, after)
		return tag.RowsAffected() == 1, err
	}
}
