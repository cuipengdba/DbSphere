package collector

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// PgBouncer reads only SHOW POOLS and SHOW STATS from the admin console.
func PgBouncer(ctx context.Context, connectionURL, database string) (map[string]any, error) {
	parsed, err := url.Parse(connectionURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") ||
		parsed.Path != "/pgbouncer" || parsed.Hostname() == "" || parsed.User == nil ||
		parsed.User.Username() == "" || database == "" {
		return nil, fmt.Errorf("invalid PgBouncer admin URL")
	}
	config, err := pgx.ParseConfig(connectionURL)
	if err != nil {
		return nil, fmt.Errorf("parse PgBouncer admin URL: %w", err)
	}
	config.ConnectTimeout = 4 * time.Second
	config.RuntimeParams["application_name"] = "dbsphere-pgbouncer-go"
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	pools, err := pgBouncerRows(ctx, conn, "SHOW POOLS", database)
	if err != nil {
		return nil, err
	}
	stats, err := pgBouncerRows(ctx, conn, "SHOW STATS", database)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 && len(stats) == 0 {
		return map[string]any{"status": "unmapped", "reason": "No PgBouncer pool maps to " + database}, nil
	}
	poolValues := make([]map[string]any, 0, len(pools))
	for _, row := range pools {
		poolValues = append(poolValues, map[string]any{"user": row["user"], "mode": row["pool_mode"],
			"clientActive": poolNumber(row["cl_active"]), "clientWaiting": poolNumber(row["cl_waiting"]),
			"serverActive": poolNumber(row["sv_active"]), "serverIdle": poolNumber(row["sv_idle"]),
			"maxWaitSeconds": poolNumber(row["maxwait"]) + poolNumber(row["maxwait_us"])/1e6})
	}
	statValues := make([]map[string]any, 0, len(stats))
	for _, row := range stats {
		statValues = append(statValues, map[string]any{"transactionsTotal": poolNumber(row["total_xact_count"]),
			"queriesTotal": poolNumber(row["total_query_count"]), "avgQueryMicroseconds": poolNumber(row["avg_query_time"])})
	}
	return map[string]any{"status": "ok", "source": "PgBouncer SHOW POOLS/STATS", "observedAt": time.Now().UTC(),
		"database": database, "pools": poolValues, "stats": statValues}, nil
}

func pgBouncerRows(ctx context.Context, conn *pgx.Conn, command, database string) ([]map[string]any, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := conn.Query(queryCtx, command, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	result := []map[string]any{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		item := make(map[string]any, len(fields))
		for i, field := range fields {
			item[string(field.Name)] = values[i]
		}
		if fmt.Sprint(item["database"]) != database {
			continue
		}
		if len(result) >= 100 {
			break
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func poolNumber(value any) float64 {
	if value == nil {
		return 0
	}
	number, err := strconv.ParseFloat(fmt.Sprint(value), 64)
	if err != nil {
		return 0
	}
	return number
}
