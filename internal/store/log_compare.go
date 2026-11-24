package store

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"
)

type logBatchSummary struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	InstanceID string     `json:"instanceId"`
	Instance   string     `json:"instance"`
	Project    string     `json:"project"`
	Coverage   string     `json:"coverage"`
	Note       string     `json:"note"`
	From       *time.Time `json:"from"`
	To         *time.Time `json:"to"`
	Records    int64      `json:"records"`
	Scanned    int64      `json:"scanned"`
	Statements int64      `json:"statements"`
	Timed      int64      `json:"timed"`
	Slow       int64      `json:"slow"`
	Errors     int64      `json:"errors"`
	Locks      int64      `json:"locks"`
	Deadlocks  int64      `json:"deadlocks"`
	Health     *int       `json:"health"`
}

const logBatchCounts = `min(r.observed_at),max(r.observed_at),count(r.id) FILTER(WHERE r.observed_at IS NOT NULL),count(r.id),
	count(r.id) FILTER(WHERE r.kind='statement'), count(r.duration_ms) FILTER(WHERE r.kind='statement'),
	count(r.id) FILTER(WHERE r.kind='statement' AND r.duration_ms>=1000),
	count(r.id) FILTER(WHERE r.kind='error'),count(r.id) FILTER(WHERE r.kind='lock'),count(r.id) FILTER(WHERE r.kind='deadlock')`

const logBatchSummaryFields = `b.id::text,b.name,i.id::text,i.name,p.name,b.coverage,b.coverage_note,` + logBatchCounts

func (value *logBatchSummary) destinations() []any {
	return []any{&value.ID, &value.Name, &value.InstanceID, &value.Instance, &value.Project, &value.Coverage, &value.Note,
		&value.From, &value.To, &value.Records, &value.Scanned, &value.Statements, &value.Timed, &value.Slow,
		&value.Errors, &value.Locks, &value.Deadlocks}
}

func (value logBatchSummary) hours() float64 {
	if value.From == nil || value.To == nil {
		return 0
	}
	return value.To.Sub(*value.From).Hours()
}

func (value *logBatchSummary) score() {
	if value.Coverage == "unknown" || value.Records < 12 || value.Scanned <= 0 ||
		value.Records*100 < value.Scanned*95 || value.hours() < 1.0/60 {
		return
	}
	// Version 1 uses three independent, capped deductions. All rates account
	// for the actual recorded window; SQL uses only statements with a duration.
	slow := float64(value.Slow) / float64(max(1, value.Timed))
	errorsPerHour := float64(value.Errors) / value.hours()
	blockingPerHour := float64(value.Locks+4*value.Deadlocks) / value.hours()
	points := int(math.Round(100 - min(35, slow*100) - min(30, errorsPerHour) - min(35, blockingPerHour*2)))
	value.Health = &points
}

type logCompareMeasure struct {
	Count      int64    `json:"count"`
	Timed      int64    `json:"timed"`
	Slow       int64    `json:"slow"`
	PerHour    float64  `json:"perHour"`
	Mean       *float64 `json:"meanMs"`
	P95        *float64 `json:"p95Ms"`
	Total      *float64 `json:"totalMs"`
	EvidenceID string   `json:"evidenceId"`
}

type logCompareRow struct {
	Key         string             `json:"key"`
	Kind        string             `json:"kind"`
	Label       string             `json:"label"`
	Application string             `json:"application"`
	Status      string             `json:"status"`
	Baseline    *logCompareMeasure `json:"baseline"`
	Current     *logCompareMeasure `json:"current"`
}

func (s *Postgres) logBatchSummary(ctx context.Context, id string) (logBatchSummary, error) {
	var value logBatchSummary
	err := s.pool.QueryRow(ctx, "SELECT "+logBatchSummaryFields+logBatchScope+
		`LEFT JOIN log_records r ON r.batch_id=b.id WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)
		GROUP BY b.id,i.id,p.id`, id, OrganizationFromContext(ctx)).Scan(value.destinations()...)
	value.score()
	return value, logWriteError(err)
}

func logComparisonCoverage(base, current logBatchSummary) string {
	if base.Health == nil || current.Health == nil {
		return "样本不足或日志覆盖待确认"
	}
	if base.Coverage != current.Coverage ||
		base.Coverage != "full" && (base.Note == "" || current.Note == "" || base.Note != current.Note) {
		return "采集方式或日志阈值需要对齐"
	}
	if base.Note != current.Note || base.hours()/current.hours() < 0.8 || base.hours()/current.hours() > 1.25 {
		return "采集说明或时间跨度需要对齐"
	}
	baseMinute := 60*base.From.Hour() + base.From.Minute()
	currentMinute := 60*current.From.Hour() + current.From.Minute()
	distance := max(baseMinute-currentMinute, currentMinute-baseMinute)
	if min(distance, 1440-distance) > 120 {
		return "两个批次的工作时段需要对齐"
	}
	return ""
}

func (s *Postgres) LogComparison(ctx context.Context, baseID, currentID string) (any, error) {
	if baseID == currentID {
		return nil, ErrLogInput
	}
	baseline, err := s.logBatchSummary(ctx, baseID)
	if err != nil {
		return nil, err
	}
	current, err := s.logBatchSummary(ctx, currentID)
	if err != nil {
		return nil, err
	}
	if baseline.InstanceID != current.InstanceID {
		return nil, ErrLogInput
	}
	reason := logComparisonCoverage(baseline, current)
	rows, err := s.readComparisonRows(ctx, baseline, current, reason == "")
	if err != nil {
		return nil, err
	}
	counts := map[string]int{"resolved": 0, "improved": 0, "regressed": 0, "new": 0, "unobserved": 0, "uncertain": 0, "stable": 0}
	var slowImproved int
	for _, row := range rows {
		counts[row.Status]++
		if row.Kind == "statement" && (row.Status == "resolved" || row.Status == "improved") {
			slowImproved++
		}
	}
	more := len(rows) > 500
	if more {
		rows = rows[:500]
	}
	return map[string]any{"baseline": baseline, "current": current, "coverageReason": reason, "rows": rows,
		"counts": counts, "slowImproved": slowImproved, "scoreVersion": 1, "limited": more}, nil
}

const logIssueGrouping = `CASE
	WHEN kind='statement' AND fingerprint<>'' THEN fingerprint
	WHEN kind IN ('lock','deadlock') THEN coalesce(nullif(object_key,''),nullif(fingerprint,''),'')
	WHEN kind='error' AND sqlstate<>'' THEN concat(sqlstate,':',fingerprint)
	ELSE '' END`

func (s *Postgres) readComparisonRows(ctx context.Context, baseline, current logBatchSummary, comparable bool) ([]*logCompareRow, error) {
	cursor, err := s.pool.Query(ctx, `WITH marked AS (
		SELECT r.*,`+logIssueGrouping+` AS issue FROM log_records r
		WHERE batch_id IN ($1,$2) AND observed_at IS NOT NULL
	) SELECT batch_id::text,kind,issue,database_name,application,
		coalesce(nullif(min(sql_shape),''),nullif(min(object_key),''),nullif(min(sqlstate),''),kind),
		count(*),count(duration_ms),count(*) FILTER (WHERE duration_ms>=1000),
		avg(duration_ms),percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms),sum(duration_ms),
		(array_agg(id::text ORDER BY duration_ms DESC NULLS LAST,observed_at DESC,id))[1]
		FROM marked WHERE issue<>'' GROUP BY batch_id,kind,issue,database_name,application`, baseline.ID, current.ID)
	if err != nil {
		return nil, err
	}
	defer cursor.Close()
	type scope struct{ kind, issue, database, application string }
	items := map[scope]*logCompareRow{}
	for cursor.Next() {
		var batch, kind, issue, database, application, label string
		var measure logCompareMeasure
		if err := cursor.Scan(&batch, &kind, &issue, &database, &application, &label, &measure.Count, &measure.Timed,
			&measure.Slow, &measure.Mean, &measure.P95, &measure.Total, &measure.EvidenceID); err != nil {
			return nil, err
		}
		key := scope{kind, issue, database, application}
		value := items[key]
		if value == nil {
			value = &logCompareRow{Key: issue, Kind: kind, Label: label, Application: application}
			items[key] = value
		}
		if batch == baseline.ID {
			measure.PerHour = float64(measure.Count) / max(1.0/60, baseline.hours())
			value.Baseline = &measure
		} else {
			measure.PerHour = float64(measure.Count) / max(1.0/60, current.hours())
			value.Current = &measure
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	rows := []*logCompareRow{}
	for _, item := range items {
		item.Status = classifyLogChange(item, comparable)
		rows = append(rows, item)
	}
	priority := map[string]int{"regressed": 0, "new": 1, "resolved": 2, "improved": 3, "unobserved": 4, "uncertain": 5, "stable": 6}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if priority[a.Status] != priority[b.Status] {
			return priority[a.Status] < priority[b.Status]
		}
		if affected(a) != affected(b) {
			return affected(a) > affected(b)
		}
		return a.Label+"\x00"+a.Application+"\x00"+evidenceFor(a) < b.Label+"\x00"+b.Application+"\x00"+evidenceFor(b)
	})
	return rows, nil
}

func evidenceFor(row *logCompareRow) string {
	if row.Current != nil {
		return row.Current.EvidenceID
	}
	return row.Baseline.EvidenceID
}

func affected(row *logCompareRow) int64 {
	if row.Current != nil {
		return row.Current.Count
	}
	return row.Baseline.Count
}

func classifyLogChange(row *logCompareRow, comparable bool) string {
	base, current := row.Baseline, row.Current
	switch {
	case current == nil:
		return "unobserved"
	case !comparable:
		return "uncertain"
	case base == nil:
		if row.Kind != "statement" || current.Slow > 0 {
			return "new"
		}
		return "stable"
	}
	if row.Kind == "statement" {
		if base.Timed < 5 || current.Timed < 5 || base.P95 == nil || current.P95 == nil {
			return "uncertain"
		}
		before, after := *base.P95, *current.P95
		if before >= 1000 && current.Slow == 0 {
			return "resolved"
		}
		if before-after >= 10 && after <= 0.75*before {
			return "improved"
		}
		if after-before >= 10 && after >= 1.25*before {
			return "regressed"
		}
	} else if base.Count+current.Count >= 5 {
		if current.PerHour <= 0.75*base.PerHour {
			return "improved"
		}
		if current.PerHour >= 1.25*base.PerHour {
			return "regressed"
		}
	}
	return "stable"
}

func (s *Postgres) LogProjectHealth(ctx context.Context, project string) (any, error) {
	candidates, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id) FROM (
		SELECT b.id,b.created_at, row_number() OVER(PARTITION BY p.id ORDER BY b.created_at DESC,b.id) AS place
		FROM log_batches b JOIN log_instances i ON i.id=b.instance_id JOIN log_projects p ON p.id=i.project_id
		WHERE ($1='' OR p.id::text=$1) AND ($2='' OR p.organization_id::text=$2)
		) ranked WHERE place<=CASE WHEN $1='' THEN 3 ELSE 12 END
		ORDER BY created_at DESC,id LIMIT 60`, project, OrganizationFromContext(ctx))
	if err != nil {
		return nil, err
	}
	results := []logBatchSummary{}
	for _, candidate := range candidates {
		var batch struct{ ID string }
		if err := json.Unmarshal(candidate, &batch); err != nil {
			return nil, err
		}
		value, err := s.logBatchSummary(ctx, batch.ID)
		if err != nil {
			return nil, err
		}
		results = append(results, value)
	}
	return map[string]any{"history": results, "scoreVersion": 1}, nil
}
