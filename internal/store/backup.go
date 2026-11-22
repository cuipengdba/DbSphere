package store

import (
	"context"
	"net/url"
	"time"
)

func (s *Postgres) backupHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	sample := params.Get("sample")
	if err != nil || params.Get("to") == "" || sample != "" && !evidenceUUID.MatchString(sample) {
		return nil, ErrInvalidRange
	}
	values, err := s.queryJSON(ctx, `WITH observations AS (
		SELECT id,collected_at,metrics_json->'backup' AS report,metrics_json#>'{diagnostics,archiver,data}' AS archiver
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
	), changed AS (
		SELECT *,lag(report) OVER chronological AS prior,lag(archiver->>'lastFailedAt') OVER chronological AS prior_failure
		FROM observations WINDOW chronological AS (ORDER BY collected_at,id)
	), selected AS (
		(SELECT id,collected_at,report,archiver FROM observations ORDER BY collected_at DESC,id DESC LIMIT 1)
		UNION
		(SELECT id,collected_at,report,archiver FROM changed WHERE report IS DISTINCT FROM prior
			OR archiver->>'lastFailedAt' IS DISTINCT FROM prior_failure ORDER BY collected_at DESC,id DESC LIMIT 20)
		UNION SELECT id,collected_at,report,archiver FROM observations WHERE id::text=$4
	) SELECT jsonb_build_object('id',id,'collectedAt',collected_at,'report',report,'archiver',archiver)
	FROM selected ORDER BY collected_at DESC,id DESC`, id, from, to, sample)
	return map[string]any{"observations": values}, err
}
