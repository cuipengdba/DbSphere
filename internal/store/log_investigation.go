package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateLogInvestigation(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=55)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE log_investigations (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		instance_id uuid NOT NULL REFERENCES log_instances(id) ON DELETE CASCADE,
		primary_record uuid NOT NULL UNIQUE REFERENCES log_records(id) ON DELETE CASCADE,
		event_id uuid REFERENCES event_keys(id) ON DELETE SET NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	); CREATE TABLE log_investigation_records (
		investigation_id uuid NOT NULL REFERENCES log_investigations(id) ON DELETE CASCADE,
		record_id uuid NOT NULL REFERENCES log_records(id) ON DELETE CASCADE,
		PRIMARY KEY(investigation_id,record_id)
	); INSERT INTO dbsphere_schema_migrations(version) VALUES (55)`)
	return err
}

type logInvestigationSource struct {
	Record, Batch, Instance, Organization string
	Asset                                 *string
	Kind, Severity, Engine, Version       string
	Coverage, Shape, Fingerprint          string
	Code, Blocker                         string
	Observed                              *time.Time
	Duration                              *float64
	RawAvailable                          bool
}

func investigationLogSource(ctx context.Context, tx pgx.Tx, record string) (logInvestigationSource, error) {
	var source logInvestigationSource
	err := tx.QueryRow(ctx, `SELECT r.id::text,b.id::text,i.id::text,p.organization_id::text,a.id::text,
		r.kind,r.severity,i.engine,i.version,b.coverage,r.sql_shape,r.fingerprint,r.sqlstate,r.blocker_id,r.observed_at,r.duration_ms,
		f.archive_expired_at IS NULL AND f.stored_size>0`+
		logBatchScope+` JOIN log_records r ON r.batch_id=b.id
		JOIN log_files f ON f.id=r.file_id
		LEFT JOIN assets a ON a.id=i.asset_id AND a.organization_id=p.organization_id
		WHERE r.id=$1 AND ($2='' OR p.organization_id::text=$2) FOR KEY SHARE OF f`, record, OrganizationFromContext(ctx)).
		Scan(&source.Record, &source.Batch, &source.Instance, &source.Organization, &source.Asset,
			&source.Kind, &source.Severity, &source.Engine, &source.Version, &source.Coverage, &source.Shape, &source.Fingerprint, &source.Code,
			&source.Blocker, &source.Observed, &source.Duration, &source.RawAvailable)
	return source, logWriteError(err)
}

type LogRecommendation struct {
	Title      string `json:"title"`
	Action     string `json:"action"`
	Validation string `json:"validation"`
	Rollback   string `json:"rollback"`
}

type LogDiagnosis struct {
	Priority        string              `json:"priority"`
	Reason          string              `json:"reason"`
	Confidence      string              `json:"confidence"`
	Recommendations []LogRecommendation `json:"recommendations"`
}

var logIndexShape = regexp.MustCompile(`^select .+? from ([a-z_][a-z0-9_]{0,60}(?: \. [a-z_][a-z0-9_]{0,60})?) where ([a-z_][a-z0-9_]{0,60}) = \?(?: |$)`)

func diagnoseLog(source logInvestigationSource) LogDiagnosis {
	defaultAction := LogRecommendation{"核对同窗采样", "确认工作负载与采集覆盖，检查同一时段的数据库会话和资源指标。",
		"在同样的业务时段收集日志及资源指标，确认异常是否重现。", "记录检查结果。"}
	result := LogDiagnosis{"P3", "保留该记录作为后续观察线索。", "日志记录", []LogRecommendation{defaultAction}}
	switch {
	case source.Severity == "PANIC":
		result.Priority, result.Reason = "P0", "数据库进程报告 PANIC。立即核对可用性、重启记录及业务影响。"
	case source.Kind == "deadlock" || source.Kind == "lock":
		result.Priority, result.Reason = "P1", "已观测到死锁或锁等待。先检查阻塞事务与持锁时长。"
		if source.Blocker != "" {
			result.Confidence = "日志包含阻塞会话"
		}
		result.Recommendations = []LogRecommendation{{"缩短事务持锁", "从原始记录定位等待者、阻塞者和锁对象。核对相关事务的操作顺序，将事务内等待外部调用的步骤移出事务。",
			"在等量负载下复测锁等待和死锁记录，核对事务一致性。", "恢复此前的应用版本和事务边界。"}}
	case source.Kind == "temp":
		result.Priority, result.Reason = "P2", "日志记录了临时文件写入，需关联 SQL 和执行计划定位算子。"
		result.Recommendations = []LogRecommendation{{"验证临时空间消耗", "用执行计划确认排序或哈希算子的落盘证据。首选减少中间结果规模，再比较语句级资源调整。",
			"对照同批次临时文件字节量和耗时分布，核对内存余量。", "撤销语句级试验参数或恢复先前 SQL。"}}
	case source.Duration != nil && source.Kind == "statement" && *source.Duration >= 1000:
		result.Priority, result.Reason = "P2", "本条 SQL 的已记录耗时达到慢查询观察阈值。"
		result.Recommendations = []LogRecommendation{{"核对执行计划与过滤条件", "对同参数执行计划核对扫描方式、过滤行数和估算偏差。比较能提前缩小结果集的 SQL 改写。",
			"对同指纹复测 P95、调用数及读写开销，并核对结果和事务语义。", "恢复原 SQL 与既有参数。"}}
		if field := logIndexShape.FindStringSubmatch(source.Shape); len(field) == 3 && source.Engine == "postgres" {
			relation, column := strings.ReplaceAll(field[1], " ", ""), field[2]
			result.Recommendations = append(result.Recommendations, LogRecommendation{"试验等值过滤索引",
				fmt.Sprintf("核对列选择性和已有索引后，在测试环境验证 CREATE INDEX ON %s (%s)。", relation, column),
				"比较执行计划、P95 和写入代价，记录试验索引名称。", "删除本次创建的试验索引。"})
		}
	case source.Kind == "error" || source.Severity == "FATAL":
		result.Priority, result.Reason = "P1", "日志记录了 SQL 错误。沿错误码、调用端与变更时间核实影响。"
	}
	return result
}

func (s *Postgres) StartLogInvestigation(ctx context.Context, record, event string) (any, error) {
	return s.startLogInvestigation(ctx, record, event, nil)
}

func (s *Postgres) startLogInvestigation(ctx context.Context, record, event string, window *logEventWindow) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	source, err := investigationLogSource(ctx, tx, record)
	if err != nil {
		return nil, err
	}
	if window != nil && (source.Asset == nil || *source.Asset != window.asset || source.Observed == nil ||
		source.Observed.Before(window.from) || source.Observed.After(window.to) || !source.RawAvailable) {
		return nil, ErrLogNotFound
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,55))`, record); err != nil {
		return nil, err
	}
	var caseID string
	if window != nil {
		err = tx.QueryRow(ctx, `SELECT i.id::text FROM log_investigations i
			JOIN log_investigation_records r ON r.investigation_id=i.id
			WHERE i.event_id=$1 AND r.record_id=$2 ORDER BY i.created_at,i.id LIMIT 1`, event, record).Scan(&caseID)
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return s.LogInvestigation(ctx, caseID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var existing *string
	err = tx.QueryRow(ctx, `SELECT id::text,event_id::text FROM log_investigations WHERE primary_record=$1`, record).Scan(&caseID, &existing)
	if err == nil {
		if event != "" && (existing == nil || event != *existing) {
			return nil, ErrLogConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return s.LogInvestigation(ctx, caseID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if event != "" && source.Asset == nil {
		return nil, ErrLogNotFound
	}
	if source.Asset != nil && event != "" {
		var matched string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM event_keys WHERE id=$1 AND asset_id=$2 FOR KEY SHARE`,
			event, *source.Asset).Scan(&matched); err != nil {
			return nil, logWriteError(err)
		}
	}
	if source.Asset != nil && event == "" {
		now := time.Now().UTC()
		if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
			VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, *source.Asset, now).Scan(&event); err != nil {
			return nil, err
		}
		title := "日志调查 · " + diagnoseLog(source).Priority + " · " + source.Kind
		if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
			VALUES($1,$2,'log_analysis',$3,'open',$4,'log_analysis')`, event, *source.Asset, title, now); err != nil {
			return nil, err
		}
	}
	err = tx.QueryRow(ctx, `INSERT INTO log_investigations(instance_id,primary_record,event_id)
		VALUES($1,$2,nullif($3,'')::uuid) RETURNING id::text`, source.Instance, record, event).Scan(&caseID)
	if err != nil {
		return nil, err
	}
	if err := linkInvestigationLog(ctx, tx, caseID, event, source); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.LogInvestigation(ctx, caseID)
}

func linkInvestigationLog(ctx context.Context, tx pgx.Tx, caseID, event string, source logInvestigationSource) error {
	inserted, err := tx.Exec(ctx, `INSERT INTO log_investigation_records(investigation_id,record_id)
		VALUES($1,$2) ON CONFLICT DO NOTHING`, caseID, source.Record)
	if err != nil || event == "" || inserted.RowsAffected() == 0 {
		return err
	}
	at := time.Now().UTC()
	if source.Observed != nil {
		at = *source.Observed
	}
	diagnosis := diagnoseLog(source)
	payload, err := json.Marshal(map[string]any{"recordId": source.Record, "batchId": source.Batch, "source": source.Engine,
		"engineVersion": source.Version, "code": diagnosis.Priority, "coverage": source.Coverage, "kind": source.Kind,
		"durationMs": source.Duration, "sqlState": source.Code, "sqlShape": limitedLogText(source.Shape, 640), "fingerprint": source.Fingerprint,
		"rootReason": diagnosis.Reason, "confidence": diagnosis.Confidence})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		SELECT gen_random_uuid(),$1::uuid,'log_record',$2::timestamptz,$3::jsonb
		WHERE NOT EXISTS (SELECT 1 FROM evidence WHERE event_id=$1 AND kind='log_record' AND payload_json->>'recordId'=$4)`,
		event, at, payload, source.Record)
	return err
}

func (s *Postgres) LinkLogInvestigation(ctx context.Context, caseID, record string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	source, err := investigationLogSource(ctx, tx, record)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,55))`, record); err != nil {
		return nil, err
	}
	var event string
	err = tx.QueryRow(ctx, `SELECT coalesce(ev.id::text,'') FROM log_investigations inv
		JOIN log_instances i ON i.id=inv.instance_id JOIN log_projects p ON p.id=i.project_id
		LEFT JOIN assets a ON a.id=i.asset_id AND a.organization_id=p.organization_id
		LEFT JOIN event_keys ev ON ev.id=inv.event_id AND ev.asset_id=a.id
		WHERE inv.id=$1 AND i.id=$2 AND p.organization_id=$3 FOR UPDATE OF inv`,
		caseID, source.Instance, source.Organization).Scan(&event)
	if err != nil {
		return nil, logWriteError(err)
	}
	if err := linkInvestigationLog(ctx, tx, caseID, event, source); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.LogInvestigation(ctx, caseID)
}

func (s *Postgres) LogInvestigation(ctx context.Context, id string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var original string
	var event *string
	err = tx.QueryRow(ctx, `SELECT inv.primary_record::text,ev.id::text FROM log_investigations inv
		JOIN log_instances i ON i.id=inv.instance_id JOIN log_projects p ON p.id=i.project_id
		LEFT JOIN assets a ON a.id=i.asset_id AND a.organization_id=p.organization_id
		LEFT JOIN event_keys ev ON ev.id=inv.event_id AND ev.asset_id=a.id
		WHERE inv.id=$1 AND ($2='' OR p.organization_id::text=$2)`,
		id, OrganizationFromContext(ctx)).Scan(&original, &event)
	if err != nil {
		return nil, logWriteError(err)
	}
	source, err := investigationLogSource(ctx, tx, original)
	if err != nil {
		return nil, err
	}
	linked, err := queryJSONRows(ctx, tx, `SELECT jsonb_build_object('id',r.id,'batch',b.id,'batchName',b.name,
		'at',r.observed_at,'kind',r.kind,'sql',r.sql_shape,'app',r.application,'durationMs',r.duration_ms,
		'archiveExpiredAt',a.archive_expired_at)
		FROM log_investigation_records f JOIN log_records r ON r.id=f.record_id JOIN log_batches b ON b.id=r.batch_id
		JOIN log_files a ON a.id=r.file_id
		WHERE f.investigation_id=$1 ORDER BY r.observed_at NULLS LAST,r.id LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "eventId": event, "diagnosis": diagnoseLog(source),
		"records": linked, "observedAt": source.Observed, "engine": source.Engine, "version": source.Version,
		"coverage": strings.TrimSpace(source.Coverage)}, nil
}
