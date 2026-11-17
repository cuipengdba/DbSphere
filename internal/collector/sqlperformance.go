package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func sqlUnavailable(err error) map[string]any {
	reason := "pg_stat_statements query unavailable"
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		reason = "pg_stat_statements SQLSTATE " + pgErr.Code
	}
	return map[string]any{"status": "unavailable", "reason": reason}
}

// SQL text and bind parameters are never selected from pg_stat_statements.
func collectSQLPerformance(ctx context.Context, conn *pgx.Conn) map[string]any {
	var schema string
	err := conn.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e
		JOIN pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname='pg_stat_statements'`).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{"status": "not_configured",
			"reason": "pg_stat_statements extension is not installed in this database"}
	}
	if err != nil {
		return sqlUnavailable(err)
	}
	rows, err := conn.Query(ctx, `SELECT a.attname FROM pg_attribute a
		JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname='pg_stat_statements'
		AND a.attnum>0 AND NOT a.attisdropped`, schema)
	if err != nil {
		return sqlUnavailable(err)
	}
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return sqlUnavailable(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return sqlUnavailable(err)
	}
	rows.Close()
	for _, name := range []string{"dbid", "userid", "queryid", "toplevel", "calls", "total_exec_time",
		"rows", "shared_blks_hit", "shared_blks_read", "shared_blks_written",
		"temp_blks_read", "temp_blks_written", "wal_bytes"} {
		if !columns[name] {
			return map[string]any{"status": "unavailable",
				"reason": "pg_stat_statements view is missing required columns"}
		}
	}
	view := `"` + strings.ReplaceAll(schema, `"`, `""`) + `".pg_stat_statements`
	info := `"` + strings.ReplaceAll(schema, `"`, `""`) + `".pg_stat_statements_info`
	query := fmt.Sprintf(`SELECT to_jsonb(q) FROM (
		SELECT dbid::text AS "databaseOid",userid::text AS "userOid",queryid::text AS "queryId",
		toplevel AS "topLevel",calls AS calls,total_exec_time AS "totalExecMs",
		rows,shared_blks_hit AS "sharedBlocksHit",shared_blks_read AS "sharedBlocksRead",
		shared_blks_written AS "sharedBlocksWritten",temp_blks_read AS "tempBlocksRead",
		temp_blks_written AS "tempBlocksWritten",wal_bytes AS "walBytes"
		FROM %s WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
		ORDER BY total_exec_time DESC LIMIT 50) q`, view)
	resultRows, err := conn.Query(ctx, query)
	if err != nil {
		return sqlUnavailable(err)
	}
	statements := []map[string]any{}
	count := 0
	for resultRows.Next() {
		var raw []byte
		if err := resultRows.Scan(&raw); err != nil {
			resultRows.Close()
			return sqlUnavailable(err)
		}
		count++
		var statement map[string]any
		if err := json.Unmarshal(raw, &statement); err != nil {
			resultRows.Close()
			return map[string]any{"status": "unavailable", "reason": "invalid pg_stat_statements data"}
		}
		queryID, _ := statement["queryId"].(string)
		if queryID == "" {
			continue
		}
		top := "0"
		if statement["topLevel"] == true {
			top = "1"
		}
		statement["fingerprint"] = statement["databaseOid"].(string) + ":" +
			statement["userOid"].(string) + ":" + queryID + ":" + top
		statements = append(statements, statement)
	}
	if err := resultRows.Err(); err != nil {
		resultRows.Close()
		return sqlUnavailable(err)
	}
	resultRows.Close()
	var resetAt *time.Time
	if err := conn.QueryRow(ctx, "SELECT stats_reset FROM "+info).Scan(&resetAt); err != nil {
		resetAt = nil
	}
	return map[string]any{"status": "ok", "source": "pg_stat_statements",
		"resetAt": resetAt, "limit": 50, "truncated": count == 50,
		"hiddenQueryCount": count - len(statements), "statements": statements}
}
