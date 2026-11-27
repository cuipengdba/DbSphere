package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrStatementComparison = errors.New("invalid SQL comparison")

var postgresFingerprint = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}:-?[0-9]{1,19}:[01]$`)
var mysqlFingerprint = regexp.MustCompile(`^[0-9a-f]{64}$`)

const statementSampleLimit = 360
const minimumStatementCalls = 5

type StatementWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type StatementComparisonInput struct {
	Fingerprint  string          `json:"fingerprint"`
	SlowLogID    string          `json:"slowLogId,omitempty"`
	Baseline     StatementWindow `json:"baseline"`
	Current      StatementWindow `json:"current"`
	BaselinePlan json.RawMessage `json:"baselinePlan,omitempty"`
	CurrentPlan  json.RawMessage `json:"currentPlan,omitempty"`
	SQLText      string          `json:"sqlText,omitempty"`
}

type StatementWindowResult struct {
	StatementWindow
	Status         string   `json:"status"`
	Samples        int      `json:"samples"`
	Pairs          int      `json:"pairs"`
	MissingPairs   int      `json:"missingPairs"`
	ResetPairs     int      `json:"resetPairs"`
	CoveredSeconds float64  `json:"coveredSeconds"`
	Calls          float64  `json:"calls"`
	TotalMS        float64  `json:"totalMs"`
	AverageMS      *float64 `json:"averageMs"`
	FirstSampleID  string   `json:"firstSampleId,omitempty"`
	LastSampleID   string   `json:"lastSampleId,omitempty"`
	Truncated      bool     `json:"truncated"`
	identity       string
}

type StatementComparison struct {
	AssetID     string                `json:"assetId"`
	EventID     string                `json:"eventId"`
	Engine      string                `json:"engine"`
	Source      string                `json:"source"`
	Fingerprint string                `json:"fingerprint"`
	Baseline    StatementWindowResult `json:"baseline"`
	Current     StatementWindowResult `json:"current"`
	Ratio       *float64              `json:"ratio"`
	Status      string                `json:"status"`
	EvidenceID  string                `json:"evidenceId,omitempty"`
	HasText     bool                  `json:"hasText,omitempty"`
	Plan        *StatementPlanPair    `json:"plan,omitempty"`
	SlowLogs    *StatementLogPair     `json:"slowLogs,omitempty"`
}

type statementEvent struct {
	AssetID, Engine, Database, Organization string
	StartedAt                               time.Time
}

func findStatementEvent(ctx context.Context, db assetQueryer, eventID string) (statementEvent, error) {
	var target statementEvent
	err := db.QueryRow(ctx, `SELECT a.id::text,a.runtime,a.database_name,a.organization_id::text,k.started_at
		FROM event_keys k JOIN assets a ON a.id=k.asset_id
		WHERE k.id=$1 AND ($2='' OR a.organization_id::text=$2)`, eventID, OrganizationFromContext(ctx)).
		Scan(&target.AssetID, &target.Engine, &target.Database, &target.Organization, &target.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, ErrEventNotFound
	}
	if err != nil {
		return target, err
	}
	if ids, ok := ctx.Value(incidentAssetScopeKey{}).([]string); ok && !slices.Contains(ids, target.AssetID) {
		return target, ErrEventNotFound
	}
	switch target.Engine {
	case "linux", "k8s", "managed":
		target.Engine = "postgresql"
	case "mysql":
	default:
		return target, ErrStatementComparison
	}
	return target, nil
}

func (s *Postgres) StatementComparisonEvent(ctx context.Context, eventID string) (map[string]any, error) {
	target, err := findStatementEvent(ctx, s.pool, eventID)
	if err != nil {
		return nil, err
	}
	rows, err := s.queryJSON(ctx, `SELECT e.payload_json || jsonb_build_object('evidenceId',e.id,'savedAt',e.observed_at,
		'hasText',EXISTS(SELECT 1 FROM statement_retest_text t JOIN statement_text_policy p ON p.organization_id=t.organization_id
		WHERE t.evidence_id=e.id AND t.organization_id=$2 AND p.days>0 AND t.expires_at>now()))
		FROM evidence e WHERE e.event_id=$1 AND e.kind='statement_retest' ORDER BY e.observed_at DESC,e.id DESC LIMIT 20`, eventID, target.Organization)
	if err != nil {
		return nil, err
	}
	return map[string]any{"assetId": target.AssetID, "engine": target.Engine, "database": target.Database,
		"organizationId": target.Organization, "startedAt": target.StartedAt, "retests": rows}, nil
}

func (s *Postgres) CompareEventStatements(ctx context.Context, eventID string, input StatementComparisonInput, save bool, key []byte) (StatementComparison, error) {
	var result StatementComparison
	if input.SQLText != "" && (!save || len(input.SQLText) > statementTextLimit) {
		return result, ErrStatementComparison
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	target, err := findStatementEvent(ctx, tx, eventID)
	if err != nil {
		return result, err
	}
	if !validStatementWindow(input.Baseline) || !validStatementWindow(input.Current) || input.Baseline.To.After(input.Current.From) ||
		input.Baseline.To.After(target.StartedAt) || input.Current.To.Before(target.StartedAt) || input.Current.To.After(time.Now().Add(5*time.Minute)) {
		return result, ErrStatementComparison
	}
	source := "pg_stat_statements"
	if target.Engine == "mysql" {
		source = "performance_schema.events_statements_summary_by_digest"
	}
	if target.Engine == "postgresql" && !postgresFingerprint.MatchString(input.Fingerprint) ||
		target.Engine == "mysql" && !mysqlFingerprint.MatchString(input.Fingerprint) {
		return result, ErrStatementComparison
	}
	if input.SlowLogID != "" && (target.Engine != "mysql" || !evidenceUUID.MatchString(input.SlowLogID)) {
		return result, ErrStatementComparison
	}
	result = StatementComparison{AssetID: target.AssetID, EventID: eventID, Engine: target.Engine, Source: source, Fingerprint: input.Fingerprint, Status: "insufficient"}
	queryID := input.Fingerprint
	if target.Engine == "postgresql" {
		queryID = strings.Split(input.Fingerprint, ":")[2]
	}
	result.Plan, err = statementPlans(target.Engine, queryID, input)
	if err != nil {
		return result, err
	}
	result.Baseline, err = loadStatementWindow(ctx, tx, target, input.Fingerprint, input.Baseline)
	if err != nil {
		return result, err
	}
	result.SlowLogs, err = loadStatementLogPair(ctx, tx, target, queryID, input)
	if err != nil {
		return result, err
	}
	result.Current, err = loadStatementWindow(ctx, tx, target, input.Fingerprint, input.Current)
	if err != nil {
		return result, err
	}
	if result.Baseline.Status == "ok" && result.Current.Status == "ok" && result.Baseline.identity != result.Current.identity {
		result.Status = "identity_changed"
	} else if result.Baseline.Status == "ok" && result.Current.Status == "ok" && *result.Baseline.AverageMS > 0 {
		ratio := *result.Current.AverageMS / *result.Baseline.AverageMS
		if !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
			result.Ratio, result.Status = &ratio, "comparable"
		}
	}
	if save {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM event_keys WHERE id=$1 FOR UPDATE`, eventID); err != nil {
			return result, err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE event_id=$1 AND kind='statement_retest'`, eventID).Scan(&count); err != nil {
			return result, err
		}
		if count >= 20 {
			return result, ErrStatementComparison
		}
		// Save only evidence generated from the event's own asset and bounded time windows.
		payload, err := json.Marshal(result)
		if err != nil {
			return result, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'statement_retest',now(),$2) RETURNING id::text`, eventID, payload).Scan(&result.EvidenceID)
		if err != nil {
			return result, err
		}
		if err := retainStatementText(ctx, tx, target.Organization, result.EvidenceID, input.SQLText, key); err != nil {
			return result, err
		}
		result.HasText = input.SQLText != ""
	}
	return result, tx.Commit(ctx)
}

func validStatementWindow(window StatementWindow) bool {
	return !window.From.IsZero() && window.To.After(window.From) && window.To.Sub(window.From) <= 24*time.Hour
}

func loadStatementWindow(ctx context.Context, tx pgx.Tx, target statementEvent, fingerprint string, window StatementWindow) (StatementWindowResult, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,collected_at,status,
		jsonb_build_object('sqlPerformance',metrics_json->'sqlPerformance','mysql',metrics_json->'mysql',
		'adapter',metrics_json->'adapter','collector',metrics_json->'collector','databaseName',metrics_json->'databaseName',
		'postgresStartedAt',metrics_json->'postgresStartedAt','postgresEndpoint',metrics_json->'postgresEndpoint',
		'version',metrics_json->'version_num')
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		ORDER BY collected_at,id LIMIT $4`, target.AssetID, window.From, window.To, statementSampleLimit+1)
	if err != nil {
		return StatementWindowResult{}, err
	}
	defer rows.Close()
	var samples []sampleObservation
	for rows.Next() {
		var item sampleObservation
		if err := rows.Scan(&item.ID, &item.At, &item.Status, &item.Metrics); err != nil {
			return StatementWindowResult{}, err
		}
		samples = append(samples, item)
	}
	return summarizeStatementWindow(window, target, fingerprint, samples), rows.Err()
}

func summarizeStatementWindow(window StatementWindow, target statementEvent, fingerprint string, samples []sampleObservation) StatementWindowResult {
	result := StatementWindowResult{StatementWindow: window, Status: "missing"}
	if len(samples) > statementSampleLimit {
		result.Truncated = true
		samples = samples[:statementSampleLimit]
	}
	result.Samples = len(samples)
	for i := 1; i < len(samples); i++ {
		before, after := samples[i-1], samples[i]
		elapsed := after.At.Sub(before.At).Seconds()
		if elapsed <= 0 || elapsed > 600 {
			result.MissingPairs++
			continue
		}
		calls, total, state := statementPair(target, fingerprint, before, after, elapsed)
		switch state {
		case "reset":
			result.ResetPairs++
		case "ok":
			identity := statementIdentity(target.Engine, fingerprint, after.Metrics)
			if result.identity != "" && result.identity != identity {
				result.ResetPairs++
				continue
			}
			result.identity = identity
			result.Pairs++
			result.CoveredSeconds += elapsed
			result.Calls += calls
			result.TotalMS += total
			if result.FirstSampleID == "" {
				result.FirstSampleID = before.ID
			}
			result.LastSampleID = after.ID
		default:
			result.MissingPairs++
		}
	}
	if result.Calls > 0 {
		mean := result.TotalMS / result.Calls
		result.AverageMS = &mean
	}
	switch {
	case result.Truncated:
		result.Status = "truncated"
	case result.ResetPairs > 0:
		result.Status = "reset"
	case result.Pairs > 0 && result.Calls < minimumStatementCalls:
		result.Status = "low_calls"
	case result.Pairs > 0 && result.MissingPairs == 0:
		result.Status = "ok"
	case result.Pairs > 0:
		result.Status = "partial"
	}
	return result
}

func statementPair(target statementEvent, fingerprint string, before, after sampleObservation, elapsed float64) (float64, float64, string) {
	if target.Engine == "mysql" {
		older, newer := object(before.Metrics["mysql"]), object(after.Metrics["mysql"])
		if !mysqlStatementAvailable(before.Metrics) || !mysqlStatementAvailable(after.Metrics) {
			return 0, 0, "missing"
		}
		if stringValue(older["database"]) != target.Database || stringValue(newer["database"]) != target.Database {
			return 0, 0, "missing"
		}
		oldUptime, oldOK := older["uptimeSeconds"].(float64)
		newUptime, newOK := newer["uptimeSeconds"].(float64)
		if stringValue(older["serverUuid"]) == "" || !oldOK || !newOK {
			return 0, 0, "missing"
		}
		if stringValue(older["serverUuid"]) != stringValue(newer["serverUuid"]) || stringValue(older["version"]) != stringValue(newer["version"]) || newUptime < oldUptime || newUptime-oldUptime+5 < elapsed {
			return 0, 0, "reset"
		}
		return statementCounterDelta(sqlStatementRows(older["topSql"]), sqlStatementRows(newer["topSql"]), "digest", fingerprint, "totalMs")
	}
	older, newer := object(before.Metrics["sqlPerformance"]), object(after.Metrics["sqlPerformance"])
	if before.Status != "ok" && stringValue(object(before.Metrics["collector"])["pgStatus"]) != "ok" ||
		after.Status != "ok" && stringValue(object(after.Metrics["collector"])["pgStatus"]) != "ok" {
		return 0, 0, "missing"
	}
	if stringValue(older["status"]) != "ok" || stringValue(newer["status"]) != "ok" ||
		stringValue(before.Metrics["databaseName"]) != target.Database || stringValue(after.Metrics["databaseName"]) != target.Database {
		return 0, 0, "missing"
	}
	oldReset, newReset := sqlResetAt(older["resetAt"]), sqlResetAt(newer["resetAt"])
	if oldReset == "" || newReset == "" {
		return 0, 0, "missing"
	}
	if oldReset != newReset || stringValue(before.Metrics["postgresStartedAt"]) != stringValue(after.Metrics["postgresStartedAt"]) ||
		!reflect.DeepEqual(before.Metrics["postgresEndpoint"], after.Metrics["postgresEndpoint"]) || number(before.Metrics["version"]) != number(after.Metrics["version"]) {
		return 0, 0, "reset"
	}
	return statementCounterDelta(sqlStatementRows(older["statements"]), sqlStatementRows(newer["statements"]), "fingerprint", fingerprint, "totalExecMs")
}

type StatementLogPair struct {
	Identity string             `json:"identity"`
	Baseline StatementLogWindow `json:"baseline"`
	Current  StatementLogWindow `json:"current"`
}

type StatementLogWindow struct {
	Status    string   `json:"status"`
	Count     int      `json:"count"`
	MedianMS  *float64 `json:"medianMs"`
	P95MS     *float64 `json:"p95Ms"`
	LastLogID string   `json:"lastLogId,omitempty"`
	BatchID   string   `json:"batchId,omitempty"`
}

func loadStatementLogPair(ctx context.Context, tx pgx.Tx, target statementEvent, queryID string, input StatementComparisonInput) (*StatementLogPair, error) {
	result := StatementLogPair{Identity: "query_id"}
	key, fingerprint := target.AssetID, queryID
	selector := `SELECT id,NULL::uuid AS batch_id,duration_ms,observed_at FROM slow_log_entries
		WHERE asset_id=$1 AND database_name=$2 AND query_id=$3 AND observed_at>=$4 AND observed_at<$5`
	if target.Engine == "mysql" {
		if input.SlowLogID == "" {
			return &StatementLogPair{Identity: "unmapped", Baseline: StatementLogWindow{Status: "missing"}, Current: StatementLogWindow{Status: "missing"}}, nil
		}
		err := tx.QueryRow(ctx, `SELECT i.id::text,r.fingerprint`+logBatchScope+`
			JOIN log_records r ON r.batch_id=b.id
			WHERE r.id=$1 AND i.asset_id=$2 AND p.organization_id=$3 AND i.engine='mysql'
			AND r.database_name=$4 AND r.kind='statement' AND r.fingerprint<>''`,
			input.SlowLogID, target.AssetID, target.Organization, target.Database).Scan(&key, &fingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrStatementComparison
		}
		if err != nil {
			return nil, err
		}
		result.Identity = "operator"
		selector = `SELECT r.id,r.batch_id,r.duration_ms,r.observed_at FROM log_batches b JOIN log_records r ON r.batch_id=b.id
			WHERE b.instance_id=$1 AND r.database_name=$2 AND r.fingerprint=$3 AND r.kind='statement'
			AND r.duration_ms IS NOT NULL AND r.observed_at>=$4 AND r.observed_at<$5`
	}
	for index, window := range []StatementWindow{input.Baseline, input.Current} {
		var row StatementLogWindow
		if err := tx.QueryRow(ctx, `WITH matched AS (`+selector+`
			ORDER BY observed_at DESC,id DESC LIMIT 1001
		)
		SELECT count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY duration_ms),
			CASE WHEN count(*)>=20 THEN percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) END,
			coalesce((SELECT id::text FROM matched ORDER BY observed_at DESC,id DESC LIMIT 1),''),
			coalesce((SELECT batch_id::text FROM matched ORDER BY observed_at DESC,id DESC LIMIT 1),'') FROM matched`,
			key, target.Database, fingerprint, window.From, window.To).
			Scan(&row.Count, &row.MedianMS, &row.P95MS, &row.LastLogID, &row.BatchID); err != nil {
			return nil, err
		}
		switch {
		case row.Count == 0:
			row.Status = "missing"
		case row.Count == 1001:
			row.Status = "truncated"
			row.MedianMS, row.P95MS = nil, nil
		default:
			row.Status = result.Identity
		}
		if index == 0 {
			result.Baseline = row
		} else {
			result.Current = row
		}
	}
	return &result, nil
}

type StatementPlanNode struct {
	Path          string   `json:"path"`
	Operation     string   `json:"operation"`
	EstimatedRows *float64 `json:"estimatedRows,omitempty"`
}

type StatementPlanPair struct {
	Identity string              `json:"identity"`
	Baseline []StatementPlanNode `json:"baseline"`
	Current  []StatementPlanNode `json:"current"`
	Changed  bool                `json:"changed"`
}

func statementPlans(engine, queryID string, input StatementComparisonInput) (*StatementPlanPair, error) {
	if len(input.BaselinePlan) == 0 && len(input.CurrentPlan) == 0 {
		return nil, nil
	}
	pair := StatementPlanPair{Identity: "operator"}
	for index, raw := range []json.RawMessage{input.BaselinePlan, input.CurrentPlan} {
		if len(raw) == 0 || len(raw) > 50000 {
			return nil, ErrStatementComparison
		}
		nodes, identity, err := parseStatementPlan(engine, queryID, raw)
		if err != nil {
			return nil, err
		}
		if index == 0 {
			pair.Baseline, pair.Identity = nodes, identity
		} else {
			pair.Current = nodes
			if pair.Identity != identity {
				pair.Identity = "operator"
			}
		}
	}
	pair.Changed = !slices.EqualFunc(pair.Baseline, pair.Current, func(left, right StatementPlanNode) bool {
		return left.Path == right.Path && left.Operation == right.Operation
	})
	return &pair, nil
}

func parseStatementPlan(engine, queryID string, raw json.RawMessage) ([]StatementPlanNode, string, error) {
	if engine == "postgresql" {
		_, _, summary, err := sanitizeSQLPlan(queryID, "", raw)
		if err != nil {
			return nil, "", ErrStatementComparison
		}
		var report struct {
			Identity string `json:"queryIdentity"`
			Nodes    []struct {
				Depth    int      `json:"depth"`
				Type     string   `json:"type"`
				JoinType string   `json:"joinType"`
				Rows     *float64 `json:"planRows"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(summary, &report); err != nil {
			return nil, "", err
		}
		nodes := make([]StatementPlanNode, 0, len(report.Nodes))
		paths := make([]int, 13)
		for _, node := range report.Nodes {
			paths[node.Depth]++
			clear(paths[node.Depth+1:])
			parts := make([]string, node.Depth+1)
			for i, value := range paths[:node.Depth+1] {
				parts[i] = strconv.Itoa(value)
			}
			nodes = append(nodes, StatementPlanNode{Path: strings.Join(parts, "."), Operation: strings.TrimSpace(node.Type + " " + node.JoinType), EstimatedRows: node.Rows})
		}
		return nodes, report.Identity, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var input map[string]any
	if !json.Valid(raw) || decoder.Decode(&input) != nil || object(input["query_block"]) == nil {
		return nil, "", ErrStatementComparison
	}
	nodes := []StatementPlanNode{}
	var walk func(map[string]any, string, int) error
	walk = func(node map[string]any, path string, depth int) error {
		if depth > 12 || len(nodes) >= 100 {
			return ErrStatementComparison
		}
		if table := object(node["table"]); table != nil {
			op := stringValue(table["access_type"])
			switch op {
			case "system", "const", "eq_ref", "ref", "fulltext", "ref_or_null", "index_merge", "unique_subquery", "index_subquery", "range", "index", "ALL":
			default:
				op = "unknown"
			}
			rows, ok := planNumber(table["rows_examined_per_scan"]).(float64)
			entry := StatementPlanNode{Path: path + ".table", Operation: op}
			if ok {
				entry.EstimatedRows = &rows
			}
			nodes = append(nodes, entry)
		}
		for _, key := range []string{"query_block", "ordering_operation", "grouping_operation", "duplicates_removal", "buffer_result", "union_result", "materialized_from_subquery", "table"} {
			if child := object(node[key]); child != nil {
				if err := walk(child, path+"."+key, depth+1); err != nil {
					return err
				}
			}
		}
		for _, key := range []string{"nested_loop", "query_specifications", "attached_subqueries"} {
			for index, child := range list(node[key]) {
				if err := walk(object(child), path+"."+key+"."+strconv.Itoa(index+1), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if walk(object(input["query_block"]), "query_block", 0) != nil || len(nodes) == 0 {
		return nil, "", ErrStatementComparison
	}
	return nodes, "operator", nil
}

func mysqlStatementAvailable(metrics map[string]any) bool {
	if stringValue(object(metrics["collector"])["mysqlStatus"]) != "ok" {
		return false
	}
	for _, raw := range list(object(metrics["adapter"])["signals"]) {
		signal := object(raw)
		if signal["key"] == "sqlPerformance" {
			return signal["status"] == "ok"
		}
	}
	return false
}

func statementCounterDelta(before, after []map[string]any, key, fingerprint, duration string) (float64, float64, string) {
	prior, next := findStatementCounter(before, key, fingerprint), findStatementCounter(after, key, fingerprint)
	if prior == nil || next == nil {
		return 0, 0, "missing"
	}
	if key == "digest" {
		first := stringValue(prior["firstSeen"])
		if first == "" || stringValue(next["firstSeen"]) == "" {
			return 0, 0, "missing"
		}
		if first != stringValue(next["firstSeen"]) {
			return 0, 0, "reset"
		}
	}
	oldCalls, oc := prior["calls"].(float64)
	newCalls, nc := next["calls"].(float64)
	oldTime, ot := prior[duration].(float64)
	newTime, nt := next[duration].(float64)
	if !oc || !nc || !ot || !nt || oldCalls < 0 || oldTime < 0 || newCalls < 0 || newTime < 0 ||
		oldCalls >= 1<<53 || newCalls >= 1<<53 || math.Trunc(oldCalls) != oldCalls || math.Trunc(newCalls) != newCalls {
		return 0, 0, "missing"
	}
	if newCalls < oldCalls || newTime < oldTime || newCalls == oldCalls && newTime != oldTime {
		return 0, 0, "reset"
	}
	return newCalls - oldCalls, newTime - oldTime, "ok"
}

func statementIdentity(engine, fingerprint string, metrics map[string]any) string {
	if engine == "mysql" {
		mysql := object(metrics["mysql"])
		row := findStatementCounter(sqlStatementRows(mysql["topSql"]), "digest", fingerprint)
		return stringValue(mysql["serverUuid"]) + "|" + stringValue(mysql["version"]) + "|" + stringValue(row["firstSeen"])
	}
	endpoint, _ := json.Marshal(metrics["postgresEndpoint"])
	return string(endpoint) + "|" + stringValue(metrics["postgresStartedAt"]) + "|" +
		strconv.FormatFloat(number(metrics["version"]), 'f', -1, 64) + "|" + sqlResetAt(object(metrics["sqlPerformance"])["resetAt"])
}

func findStatementCounter(rows []map[string]any, key, fingerprint string) map[string]any {
	for _, row := range rows {
		if stringValue(row[key]) == fingerprint {
			return row
		}
	}
	return nil
}
