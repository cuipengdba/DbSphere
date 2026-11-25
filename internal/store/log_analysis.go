package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed log_analysis.sql
var logAnalysisSchema string

var (
	ErrLogInput    = errors.New("invalid log analysis input")
	ErrLogNotFound = errors.New("log analysis resource not found")
	ErrLogConflict = errors.New("log analysis resource conflicts with current state")
)

func migrateLogAnalysis(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=53)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, logAnalysisSchema)
	return err
}

type LogProjectInput struct{ Name, OrganizationID string }
type LogInstanceInput struct{ ProjectID, AssetID, Environment, Name, Engine, Version string }
type LogBatchInput struct{ InstanceID, Name, Coverage, CoverageNote string }

func analysisLabel(value string, length int) bool {
	return len(value) <= length && !strings.ContainsAny(value, "\x00\r\n") && strings.TrimSpace(value) != ""
}

func logWriteError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLogNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrLogConflict
	}
	return err
}

func (s *Postgres) LogDirectory(ctx context.Context) (any, error) {
	org := OrganizationFromContext(ctx)
	projects, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',p.id,'name',p.name,'organizationId',o.id,'organization',o.name)
		FROM log_projects p JOIN organizations o ON o.id=p.organization_id
		WHERE ($1='' OR p.organization_id::text=$1) ORDER BY o.name,p.name`, org)
	if err != nil {
		return nil, err
	}
	instances, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',i.id,'projectId',i.project_id,'assetId',i.asset_id,
		'name',i.name,'engine',i.engine,'version',i.version,'environment',i.environment)
		FROM log_instances i JOIN log_projects p ON p.id=i.project_id
		WHERE ($1='' OR p.organization_id::text=$1) ORDER BY i.environment,i.name,i.id`, org)
	if err != nil {
		return nil, err
	}
	return map[string]any{"projects": projects, "instances": instances}, nil
}

func (s *Postgres) CreateLogProject(ctx context.Context, input LogProjectInput) (any, error) {
	org, err := requestedOrganization(ctx, input.OrganizationID, DefaultOrganizationID)
	if err != nil || !analysisLabel(input.Name, 120) {
		return nil, ErrLogInput
	}
	var value json.RawMessage
	err = s.pool.QueryRow(ctx, `INSERT INTO log_projects(organization_id,name)
		SELECT id,$2 FROM organizations WHERE id=$1
		RETURNING jsonb_build_object('id',id,'name',name,'organizationId',organization_id)`,
		org, strings.TrimSpace(input.Name)).Scan(&value)
	return value, logWriteError(err)
}

func logsEngine(engine string) bool {
	return engine == "postgres" || engine == "highgo" || engine == "kingbase" || engine == "dm8" || engine == "mysql"
}

func (s *Postgres) CreateLogInstance(ctx context.Context, input LogInstanceInput) (any, error) {
	if !evidenceUUID.MatchString(input.ProjectID) || input.AssetID != "" && !evidenceUUID.MatchString(input.AssetID) ||
		!analysisLabel(input.Name, 120) || !analysisLabel(input.Environment, 80) ||
		!logsEngine(input.Engine) || !analysisLabel(input.Version, 80) {
		return nil, ErrLogInput
	}
	var value json.RawMessage
	err := s.pool.QueryRow(ctx, `INSERT INTO log_instances(project_id,asset_id,environment,name,engine,version)
		SELECT p.id,a.id,$4,$5,$6,$7 FROM log_projects p
		LEFT JOIN assets a ON a.id::text=$2 AND a.organization_id=p.organization_id
		WHERE p.id=$1 AND ($2='' OR a.id IS NOT NULL) AND ($3='' OR p.organization_id::text=$3)
		RETURNING jsonb_build_object('id',id,'projectId',project_id,'name',name,'assetId',asset_id,
			'environment',environment,'engine',engine,'version',version)`, input.ProjectID, input.AssetID,
		OrganizationFromContext(ctx), strings.TrimSpace(input.Environment), strings.TrimSpace(input.Name),
		input.Engine, strings.TrimSpace(input.Version)).Scan(&value)
	return value, logWriteError(err)
}

func (s *Postgres) CreateLogBatch(ctx context.Context, input LogBatchInput) (any, error) {
	if !evidenceUUID.MatchString(input.InstanceID) || !analysisLabel(input.Name, 120) ||
		len(input.CoverageNote) > 360 || strings.ContainsRune(input.CoverageNote, 0) {
		return nil, ErrLogInput
	}
	switch input.Coverage {
	case "", "unknown":
		input.Coverage = "unknown"
	case "sampled", "slow", "full":
	default:
		return nil, ErrLogInput
	}
	var value json.RawMessage
	err := s.pool.QueryRow(ctx, `INSERT INTO log_batches(instance_id,name,coverage,coverage_note)
		SELECT i.id,$3,$4,$5 FROM log_instances i JOIN log_projects p ON p.id=i.project_id
		WHERE i.id=$1 AND ($2='' OR p.organization_id::text=$2)
		RETURNING jsonb_build_object('id',id,'instanceId',instance_id,'name',name,'coverage',coverage)`,
		input.InstanceID, OrganizationFromContext(ctx), strings.TrimSpace(input.Name), input.Coverage, strings.TrimSpace(input.CoverageNote)).Scan(&value)
	return value, logWriteError(err)
}

const logBatchScope = ` FROM log_batches b JOIN log_instances i ON i.id=b.instance_id
	JOIN log_projects p ON p.id=i.project_id `

const logFileScope = ` FROM log_files f JOIN log_batches b ON b.id=f.batch_id
	JOIN log_instances i ON i.id=b.instance_id JOIN log_projects p ON p.id=i.project_id `

func (s *Postgres) LogBatches(ctx context.Context, instance string) (any, error) {
	if instance != "" && !evidenceUUID.MatchString(instance) {
		return nil, ErrLogInput
	}
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',b.id,'instanceId',i.id,'instance',i.name,'project',p.name,
		'environment',i.environment,'engine',i.engine,'version',i.version,'coverage',b.coverage,
		'coverageNote',b.coverage_note,'name',b.name,'createdAt',b.created_at)`+logBatchScope+`
		WHERE ($1='' OR p.organization_id::text=$1) AND ($2='' OR i.id::text=$2)
		ORDER BY b.created_at DESC,b.id LIMIT 100`, OrganizationFromContext(ctx), instance)
	return map[string]any{"batches": rows}, err
}

func (s *Postgres) LogBatch(ctx context.Context, batch string) (any, error) {
	org := OrganizationFromContext(ctx)
	var value json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',b.id,'name',b.name,'coverage',b.coverage,
		'coverageNote',b.coverage_note,'instanceId',i.id,'assetId',i.asset_id,'engine',i.engine,'version',i.version,
		'instance',i.name,'project',p.name,'environment',i.environment,'createdAt',b.created_at)`+logBatchScope+`
		WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)`, batch, org).Scan(&value)
	if err != nil {
		return nil, logWriteError(err)
	}
	files, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'filename',filename,'state',state,'parts',parts,
		'format',format,'detectedFormat',detected_format,'rawSize',raw_size,'storedSize',stored_size,
		'recordCount',record_count,'parsedCount',parsed_count,'reason',reason,'node',node,
		'verifiedAt',verified_at,'archiveExpiredAt',archive_expired_at,
		'duplicateBatch', (SELECT original.batch_id FROM log_files original WHERE original.id=f.duplicate_of))
		FROM log_files f WHERE batch_id=$1 ORDER BY created_at,id`, batch)
	if err != nil {
		return nil, err
	}
	var totals json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object('records',count(*),'slow',count(*) FILTER(WHERE kind='statement' AND duration_ms>=1000),
		'errors',count(*) FILTER(WHERE kind IN ('error','deadlock')),'unknown',count(*) FILTER(WHERE kind='unknown'),
		'from',min(observed_at),'to',max(observed_at)) FROM log_records WHERE batch_id=$1`, batch).Scan(&totals)
	return map[string]any{"batch": value, "files": files, "totals": totals}, err
}

type LogRecordQuery struct {
	Kind, Application, Fingerprint string
	From, To                       *time.Time
	Offset                         int
}

func (s *Postgres) LogRecords(ctx context.Context, batch string, filters LogRecordQuery) (any, error) {
	if len(filters.Application) > 256 || len(filters.Fingerprint) > 64 || filters.Offset < 0 || filters.Offset > 1000000 {
		return nil, ErrLogInput
	}
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',r.id,'at',r.observed_at,'kind',r.kind,'severity',r.severity,
		'application',r.application,'database',r.database_name,'fingerprint',r.fingerprint,'sql',r.sql_shape,'durationMs',r.duration_ms,
		'file',f.filename,'offset',r.raw_offset,'process',r.process_id,'blocker',r.blocker_id,'sqlstate',r.sqlstate,
		'archiveExpiredAt',f.archive_expired_at)
		FROM log_records r JOIN log_files f ON f.id=r.file_id JOIN log_batches b ON b.id=r.batch_id
		JOIN log_instances i ON i.id=b.instance_id JOIN log_projects p ON p.id=i.project_id
		WHERE r.batch_id=$1 AND ($2='' OR p.organization_id::text=$2) AND ($3='' OR r.kind=$3)
		AND ($4='' OR r.application=$4) AND ($5='' OR r.fingerprint=$5)
		AND ($6::timestamptz IS NULL OR r.observed_at >= $6) AND ($7::timestamptz IS NULL OR r.observed_at < $7)
		ORDER BY r.observed_at DESC NULLS LAST,r.id LIMIT 51 OFFSET $8`,
		batch, OrganizationFromContext(ctx), filters.Kind, filters.Application, filters.Fingerprint, filters.From, filters.To, filters.Offset)
	if err != nil {
		return nil, err
	}
	more := len(rows) > 50
	if more {
		rows = rows[:50]
	}
	return map[string]any{"records": rows, "more": more}, nil
}
