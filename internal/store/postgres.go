package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	pool                          *pgxpool.Pool
	platformReplicaID             string
	sampleRetentionDays           int
	eventRetentionDays            int
	operationsWebhookOrganization string
	tracePodMu                    sync.Mutex
	tracePodCache                 map[string]tracePodCacheEntry
	containerStorage              collector.ContainerStorageConfig
}

func (s *Postgres) ConfigureContainerStorage(cfg collector.ContainerStorageConfig) {
	s.containerStorage = cfg
}

// SetPlatformReplica records the local collector identity before starting workers.
func (s *Postgres) SetPlatformReplica(id string) { s.platformReplicaID = id }

// ConfigureRetention is called once before the HTTP server starts. Zero means
// that historical samples or events are kept indefinitely.
func (s *Postgres) ConfigureRetention(sampleDays, eventDays int) {
	s.sampleRetentionDays = sampleDays
	s.eventRetentionDays = eventDays
}

//go:embed schema.sql
var schemaSQL string

func Open(ctx context.Context, url string) (*Postgres, error) {
	return OpenWithCollectorWorkers(ctx, url, 0)
}

func OpenWithCollectorWorkers(ctx context.Context, url string, workers int) (*Postgres, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL 18 URL: %w", err)
	}
	// Keep capacity available for the console and maintenance when every
	// collection worker is persisting a result or waiting on a lease.
	config.MaxConns = int32(5 + max(0, min(workers, 16)))
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "dbsphere-go"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL 18 pool: %w", err)
	}
	store := &Postgres{pool: pool}
	if err := store.Health(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *Postgres) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin DbSphere schema migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return fmt.Errorf("lock DbSphere schema migration: %w", err)
	}
	var migrationTable *string
	if err = tx.QueryRow(ctx, "SELECT to_regclass('public.dbsphere_schema_migrations')::text").Scan(&migrationTable); err != nil {
		return fmt.Errorf("check DbSphere migration table: %w", err)
	}
	current := false
	if migrationTable != nil {
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=13)").Scan(&current); err != nil {
			return fmt.Errorf("check DbSphere schema version: %w", err)
		}
	}
	if !current {
		if _, err = tx.Exec(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply DbSphere schema: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(13) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record DbSphere Go schema migration: %w", err)
		}
	}
	var tracePIDMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=14)").Scan(&tracePIDMigrated); err != nil {
		return fmt.Errorf("check trace backend PID migration: %w", err)
	}
	if !tracePIDMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS backend_pid integer"); err != nil {
			return fmt.Errorf("migrate trace backend PID: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(14) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace backend PID migration: %w", err)
		}
	}
	var aiBudgetMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=15)").Scan(&aiBudgetMigrated); err != nil {
		return fmt.Errorf("check AI budget migration: %w", err)
	}
	if !aiBudgetMigrated {
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS evidence_ai_invocation_time ON evidence(observed_at) WHERE kind='ai_invocation'"); err != nil {
			return fmt.Errorf("index AI invocation budget: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(15) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record AI budget migration: %w", err)
		}
	}
	var auditMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=16)").Scan(&auditMigrated); err != nil {
		return fmt.Errorf("check admin audit migration: %w", err)
	}
	if !auditMigrated {
		if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS admin_audit (
			id uuid PRIMARY KEY, observed_at timestamptz NOT NULL DEFAULT now(),
			actor text NOT NULL, method text NOT NULL, path text NOT NULL,
			outcome text NOT NULL, status_code integer)`); err != nil {
			return fmt.Errorf("create admin audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS admin_audit_time ON admin_audit(observed_at DESC)"); err != nil {
			return fmt.Errorf("index admin audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(16) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record admin audit migration: %w", err)
		}
	}
	var eventAuditMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=17)").Scan(&eventAuditMigrated); err != nil {
		return fmt.Errorf("check event audit migration: %w", err)
	}
	if !eventAuditMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS event_id uuid"); err != nil {
			return fmt.Errorf("migrate event audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS admin_audit_event_time ON admin_audit(event_id,observed_at DESC) WHERE event_id IS NOT NULL"); err != nil {
			return fmt.Errorf("index event audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(17) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record event audit migration: %w", err)
		}
	}
	var traceParentMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=18)").Scan(&traceParentMigrated); err != nil {
		return fmt.Errorf("check trace parent lookup migration: %w", err)
	}
	if !traceParentMigrated {
		if _, err = tx.Exec(ctx, `CREATE INDEX IF NOT EXISTS trace_spans_parent_lookup
			ON trace_spans(trace_id,parent_span_id) WHERE parent_span_id IS NOT NULL`); err != nil {
			return fmt.Errorf("index trace parent lookup: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(18) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace parent lookup migration: %w", err)
		}
	}
	var tracePendingMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=19)").Scan(&tracePendingMigrated); err != nil {
		return fmt.Errorf("check pending trace migration: %w", err)
	}
	if !tracePendingMigrated {
		if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS trace_pending_spans (
			trace_id text NOT NULL, span_id text NOT NULL, parent_span_id text,
			service text NOT NULL, started_at timestamptz NOT NULL,
			ended_at timestamptz NOT NULL, status text NOT NULL,
			received_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY(trace_id,span_id))`); err != nil {
			return fmt.Errorf("create pending trace buffer: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS trace_pending_received ON trace_pending_spans(received_at)"); err != nil {
			return fmt.Errorf("index pending trace cleanup: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(19) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record pending trace migration: %w", err)
		}
	}
	var patroniMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=20)").Scan(&patroniMigrated); err != nil {
		return fmt.Errorf("check Patroni endpoint migration: %w", err)
	}
	if !patroniMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE assets ADD COLUMN IF NOT EXISTS patroni_url_enc text"); err != nil {
			return fmt.Errorf("migrate Patroni endpoint: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(20) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record Patroni endpoint migration: %w", err)
		}
	}
	var evidenceRefsMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=21)").Scan(&evidenceRefsMigrated); err != nil {
		return fmt.Errorf("check evidence sample reference migration: %w", err)
	}
	if !evidenceRefsMigrated {
		if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS evidence_sample_refs (
			evidence_id uuid NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
			sample_id text NOT NULL,
			PRIMARY KEY(evidence_id,sample_id))`); err != nil {
			return fmt.Errorf("create evidence sample reference index: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS evidence_sample_refs_sample ON evidence_sample_refs(sample_id)"); err != nil {
			return fmt.Errorf("index evidence sample references: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS inspection_runs_sample ON inspection_runs(sample_id) WHERE sample_id IS NOT NULL"); err != nil {
			return fmt.Errorf("index inspection sample references: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS inspection_runs_events ON inspection_runs USING gin(event_ids_json)"); err != nil {
			return fmt.Errorf("index inspection event references: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS samples_retention_time ON samples(collected_at,id)"); err != nil {
			return fmt.Errorf("index sample retention time: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS events_resolved_ended ON events(ended_at) WHERE status='resolved'"); err != nil {
			return fmt.Errorf("index resolved event retention time: %w", err)
		}
		if _, err = tx.Exec(ctx, `CREATE OR REPLACE FUNCTION dbsphere_index_evidence_samples()
			RETURNS trigger LANGUAGE plpgsql AS $function$
			BEGIN
				DELETE FROM evidence_sample_refs WHERE evidence_id=NEW.id;
				INSERT INTO evidence_sample_refs(evidence_id,sample_id)
				SELECT DISTINCT NEW.id, ref.value #>> '{}' FROM jsonb_path_query(NEW.payload_json,
				  '$.** ? (@.type() == "object").keyvalue() ? (@.key like_regex "SampleId$" flag "i").value') AS ref(value)
				WHERE jsonb_typeof(ref.value)='string';
				RETURN NEW;
			END
			$function$`); err != nil {
			return fmt.Errorf("create evidence sample reference trigger function: %w", err)
		}
		if _, err = tx.Exec(ctx, `CREATE TRIGGER dbsphere_evidence_sample_refs
			AFTER INSERT OR UPDATE OF payload_json ON evidence
			FOR EACH ROW EXECUTE FUNCTION dbsphere_index_evidence_samples()`); err != nil {
			return fmt.Errorf("create evidence sample reference trigger: %w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO evidence_sample_refs(evidence_id,sample_id)
			SELECT DISTINCT ev.id, ref.value #>> '{}' FROM evidence ev
			CROSS JOIN LATERAL jsonb_path_query(ev.payload_json,
			  '$.** ? (@.type() == "object").keyvalue() ? (@.key like_regex "SampleId$" flag "i").value') AS ref(value)
			WHERE jsonb_typeof(ref.value)='string'
			ON CONFLICT DO NOTHING`); err != nil {
			return fmt.Errorf("backfill evidence sample references: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(21) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record evidence sample reference migration: %w", err)
		}
	}
	var eventPageMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=22)").Scan(&eventPageMigrated); err != nil {
		return fmt.Errorf("check event pagination migration: %w", err)
	}
	if !eventPageMigrated {
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS events_page_order ON events(started_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index event pagination order: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS events_status_page_order ON events(status,started_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index event pagination status: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(22) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record event pagination migration: %w", err)
		}
	}
	var assetEventPageMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=23)").Scan(&assetEventPageMigrated); err != nil {
		return fmt.Errorf("check asset event pagination migration: %w", err)
	}
	if !assetEventPageMigrated {
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS events_asset_page_order ON events(asset_id,started_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index asset event pagination order: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS events_asset_status_page_order ON events(asset_id,status,started_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index asset event pagination status: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(23) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record asset event pagination migration: %w", err)
		}
	}
	var assetPageMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=24)").Scan(&assetPageMigrated); err != nil {
		return fmt.Errorf("check asset pagination migration: %w", err)
	}
	if !assetPageMigrated {
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS assets_page_order ON assets(created_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index asset pagination order: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS assets_runtime_page_order ON assets(runtime,created_at DESC,id DESC)"); err != nil {
			return fmt.Errorf("index asset pagination runtime: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(24) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record asset pagination migration: %w", err)
		}
	}
	var traceQueryIDMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=25)").Scan(&traceQueryIDMigrated); err != nil {
		return fmt.Errorf("check trace Query ID migration: %w", err)
	}
	if !traceQueryIDMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS query_id text"); err != nil {
			return fmt.Errorf("migrate trace Query ID: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(25) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace Query ID migration: %w", err)
		}
	}
	var workOrdersMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=26)").Scan(&workOrdersMigrated); err != nil {
		return fmt.Errorf("check work order migration: %w", err)
	}
	if !workOrdersMigrated {
		if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS work_orders (
			id uuid PRIMARY KEY, event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			action text NOT NULL CHECK(action='collect_once'), reason text NOT NULL,
			status text NOT NULL CHECK(status IN ('requested','approved','rejected','executing','completed','failed')),
			requested_by text NOT NULL, approved_by text,
			requested_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
			approved_at timestamptz, executed_at timestamptz, result_json jsonb)`); err != nil {
			return fmt.Errorf("create work orders: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS work_orders_event_time ON work_orders(event_id,requested_at DESC)"); err != nil {
			return fmt.Errorf("index event work orders: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(26) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record work order migration: %w", err)
		}
	}
	var traceRouteMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=27)").Scan(&traceRouteMigrated); err != nil {
		return fmt.Errorf("check trace route migration: %w", err)
	}
	if !traceRouteMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS route text NOT NULL DEFAULT 'direct'"); err != nil {
			return fmt.Errorf("migrate trace route: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(27) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace route migration: %w", err)
		}
	}
	var appPodMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=28)").Scan(&appPodMigrated); err != nil {
		return fmt.Errorf("check trace application Pod migration: %w", err)
	}
	if !appPodMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS app_pod_reported jsonb, ADD COLUMN IF NOT EXISTS app_pod_verified jsonb"); err != nil {
			return fmt.Errorf("migrate trace application Pod identity: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(28) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace application Pod migration: %w", err)
		}
	}
	var poolerEndpointMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=29)").Scan(&poolerEndpointMigrated); err != nil {
		return fmt.Errorf("check PgBouncer application endpoint migration: %w", err)
	}
	if !poolerEndpointMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE assets ADD COLUMN IF NOT EXISTS pooler_endpoint text"); err != nil {
			return fmt.Errorf("migrate PgBouncer application endpoint: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(29) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record PgBouncer application endpoint migration: %w", err)
		}
	}
	var traceTimingMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=30)").Scan(&traceTimingMigrated); err != nil {
		return fmt.Errorf("check trace timing migration: %w", err)
	}
	if !traceTimingMigrated {
		if _, err = tx.Exec(ctx, `ALTER TABLE trace_spans
			ADD COLUMN IF NOT EXISTS received_at timestamptz,
			ADD COLUMN IF NOT EXISTS sampling_probability double precision
				CHECK (sampling_probability > 0 AND sampling_probability <= 1)`); err != nil {
			return fmt.Errorf("migrate trace timing columns: %w", err)
		}
		if _, err = tx.Exec(ctx, `ALTER TABLE trace_pending_spans
			ADD COLUMN IF NOT EXISTS sampling_probability double precision
				CHECK (sampling_probability > 0 AND sampling_probability <= 1)`); err != nil {
			return fmt.Errorf("migrate pending trace sampling column: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(30) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace timing migration: %w", err)
		}
	}
	var pendingIngressMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=31)").Scan(&pendingIngressMigrated); err != nil {
		return fmt.Errorf("check pending trace ingress migration: %w", err)
	}
	if !pendingIngressMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_pending_spans ADD COLUMN IF NOT EXISTS ingress_at timestamptz"); err != nil {
			return fmt.Errorf("migrate pending trace ingress time: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(31) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record pending trace ingress migration: %w", err)
		}
	}
	var clusterNameMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=32)").Scan(&clusterNameMigrated); err != nil {
		return fmt.Errorf("check monitoring cluster migration: %w", err)
	}
	if !clusterNameMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE assets ADD COLUMN IF NOT EXISTS cluster_name text"); err != nil {
			return fmt.Errorf("migrate monitoring cluster name: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(32)"); err != nil {
			return fmt.Errorf("record monitoring cluster migration: %w", err)
		}
	}
	var fleetLabelsMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=33)").Scan(&fleetLabelsMigrated); err != nil {
		return fmt.Errorf("check fleet label migration: %w", err)
	}
	if !fleetLabelsMigrated {
		if _, err = tx.Exec(ctx, `ALTER TABLE assets ADD COLUMN IF NOT EXISTS environment text,
			ADD COLUMN IF NOT EXISTS tags text[] NOT NULL DEFAULT '{}'`); err != nil {
			return fmt.Errorf("migrate fleet labels: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(33)"); err != nil {
			return fmt.Errorf("record fleet label migration: %w", err)
		}
	}
	if err := migrateTrendProjection(ctx, tx); err != nil {
		return err
	}
	if err := migrateHourlyTrend(ctx, tx); err != nil {
		return err
	}
	if err := migrateCollectionSchedule(ctx, tx); err != nil {
		return err
	}
	if err := migrateWaitCaptures(ctx, tx); err != nil {
		return err
	}
	if err := migrateSQLEvidenceIdentity(ctx, tx); err != nil {
		return err
	}
	if err := migrateRouting(ctx, tx); err != nil {
		return fmt.Errorf("migrate route diagnostics: %w", err)
	}
	if err := migrateChangeTimeline(ctx, tx); err != nil {
		return fmt.Errorf("migrate asset change timeline: %w", err)
	}
	if err := migrateIncidentImpact(ctx, tx); err != nil {
		return fmt.Errorf("migrate cross-instance incident impact: %w", err)
	}
	if err := migrateRCAPolicies(ctx, tx); err != nil {
		return fmt.Errorf("migrate versioned RCA rules: %w", err)
	}
	if err := migrateReadEntry(ctx, tx); err != nil {
		return fmt.Errorf("migrate read entry monitoring: %w", err)
	}
	if err := migrateAccess(ctx, tx); err != nil {
		return fmt.Errorf("migrate named account access: %w", err)
	}
	if err := migrateWorkPlans(ctx, tx); err != nil {
		return fmt.Errorf("migrate controlled work plans: %w", err)
	}
	if err := migratePlatformHA(ctx, tx); err != nil {
		return fmt.Errorf("migrate platform replica leadership: %w", err)
	}
	if err := migrateOrganizations(ctx, tx); err != nil {
		return fmt.Errorf("migrate organizations and signed audit: %w", err)
	}
	if err := migrateOperations(ctx, tx); err != nil {
		return fmt.Errorf("migrate operations integration: %w", err)
	}
	if err := migrateHistoryTiers(ctx, tx); err != nil {
		return fmt.Errorf("migrate history retention tiers: %w", err)
	}
	if err := migrateRiskBaseline(ctx, tx); err != nil {
		return fmt.Errorf("migrate rule-based risk forecasting: %w", err)
	}
	if err := migrateConnectionStages(ctx, tx); err != nil {
		return fmt.Errorf("migrate application connection stages: %w", err)
	}
	if err := migrateCollectorHealth(ctx, tx); err != nil {
		return fmt.Errorf("migrate collector health: %w", err)
	}
	if err := migrateLogAnalysis(ctx, tx); err != nil {
		return fmt.Errorf("migrate log analysis: %w", err)
	}
	if err := migrateLogReport(ctx, tx); err != nil {
		return fmt.Errorf("migrate log analysis reports: %w", err)
	}
	if err := migrateLogInvestigation(ctx, tx); err != nil {
		return fmt.Errorf("migrate log investigations: %w", err)
	}
	if err := migrateLogArchiveManagement(ctx, tx); err != nil {
		return fmt.Errorf("migrate log archive management: %w", err)
	}
	if err := migrateLogTasks(ctx, tx); err != nil {
		return fmt.Errorf("migrate log tasks: %w", err)
	}
	if err := migrateLogEventSearch(ctx, tx); err != nil {
		return fmt.Errorf("migrate incident log search: %w", err)
	}
	if err := migrateLogRetests(ctx, tx); err != nil {
		return fmt.Errorf("migrate log retest results: %w", err)
	}
	if err := migrateDutyViews(ctx, tx); err != nil {
		return fmt.Errorf("migrate duty views and handoff: %w", err)
	}
	if err := migrateNativeRisk(ctx, tx); err != nil {
		return fmt.Errorf("migrate native risk profiles: %w", err)
	}
	if err := migrateCollectionFleetHealth(ctx, tx); err != nil {
		return fmt.Errorf("migrate collection fleet health: %w", err)
	}
	if err := migrateAssetOnboarding(ctx, tx); err != nil {
		return fmt.Errorf("migrate asset onboarding: %w", err)
	}
	if err := migrateCollectionHistory(ctx, tx); err != nil {
		return fmt.Errorf("migrate collection history: %w", err)
	}
	if err := migrateLogStreams(ctx, tx); err != nil {
		return fmt.Errorf("migrate log sources: %w", err)
	}
	if err := migrateLogRetestPlans(ctx, tx); err != nil {
		return fmt.Errorf("migrate log retest plans: %w", err)
	}
	if err := seedGoRules(ctx, tx); err != nil {
		return fmt.Errorf("seed supported Go alert rules: %w", err)
	}
	if err := seedGoInspections(ctx, tx); err != nil {
		return fmt.Errorf("seed Go inspection templates: %w", err)
	}
	if err := ensureTimePartitions(ctx, tx, time.Now().UTC()); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit DbSphere schema migration: %w", err)
	}
	return nil
}

// EnsureTimePartitions keeps writes available when a server stays up across
// month boundaries. The migration lock also serializes multiple replicas.
func (s *Postgres) EnsureTimePartitions(ctx context.Context, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin partition maintenance: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return fmt.Errorf("lock partition maintenance: %w", err)
	}
	if err := ensureTimePartitions(ctx, tx, at); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit partition maintenance: %w", err)
	}
	return nil
}

func ensureTimePartitions(ctx context.Context, tx pgx.Tx, at time.Time) error {
	at = at.UTC()
	thisMonth := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, table := range []string{"samples", "events"} {
		for offset := range 3 {
			start := thisMonth.AddDate(0, offset, 0)
			end := start.AddDate(0, 1, 0)
			name := fmt.Sprintf("%s_%04d%02d", table, start.Year(), int(start.Month()))
			var present *string
			if err := tx.QueryRow(ctx, "SELECT to_regclass($1)::text", "public."+name).Scan(&present); err != nil {
				return fmt.Errorf("check %s partition: %w", name, err)
			}
			if present == nil {
				query := fmt.Sprintf("CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
					name, table, start.Format(time.RFC3339), end.Format(time.RFC3339))
				if _, err := tx.Exec(ctx, query); err != nil {
					return fmt.Errorf("create %s partition: %w", name, err)
				}
			}
			var attached bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_inherits
				WHERE inhrelid=to_regclass($1) AND inhparent=$2::regclass)`, "public."+name, table).
				Scan(&attached); err != nil {
				return fmt.Errorf("verify %s partition: %w", name, err)
			}
			if !attached {
				return fmt.Errorf("partition %s is not attached to %s", name, table)
			}
		}
	}
	return nil
}

func (s *Postgres) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var version int
	var writable bool
	if err := s.pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer, NOT pg_is_in_recovery() AND current_setting('transaction_read_only')='off'").Scan(&version, &writable); err != nil {
		return fmt.Errorf("check PostgreSQL 18 storage: %w", err)
	}
	if version/10000 != 18 {
		return errors.New("DbSphere long-term store requires PostgreSQL 18")
	}
	if !writable {
		s.pool.Reset()
		return errors.New("DbSphere long-term store requires a writable primary")
	}
	return nil
}

// CurrentAdminPasswordHash returns an empty hash before the first administrator
// credential is committed. The initial password is never stored as a normal login.
func (s *Postgres) CurrentAdminPasswordHash(ctx context.Context) (string, error) {
	var stored string
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='admin_password_hash'").Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read administrator password hash: %w", err)
	}
	return stored, nil
}

func (s *Postgres) AdminPasswordHash(ctx context.Context, bootstrapPassword string) (string, error) {
	stored, err := s.CurrentAdminPasswordHash(ctx)
	if err != nil || stored != "" || security.NeedsInitialAdministratorPassword(bootstrapPassword) {
		return stored, err
	}
	if len(bootstrapPassword) < 12 {
		return "", errors.New("DBSPHERE_ADMIN_PASSWORD is required for first start")
	}
	hash, err := security.HashPassword(bootstrapPassword)
	if err != nil {
		return "", fmt.Errorf("hash administrator password: %w", err)
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO settings(key,value) VALUES('admin_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash); err != nil {
		return "", fmt.Errorf("initialize administrator password: %w", err)
	}
	return s.CurrentAdminPasswordHash(ctx)
}

// CompleteAdministratorSetup commits the first real password and its audit row
// together. Concurrent setup requests can only commit one password.
func (s *Postgres) CompleteAdministratorSetup(ctx context.Context, password string) (string, error) {
	if !security.ValidNewAdministratorPassword(password) {
		return "", errors.New("administrator password must contain 12 to 256 characters")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	inserted, err := tx.Exec(ctx, "INSERT INTO settings(key,value) VALUES('admin_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash)
	if err != nil || inserted.RowsAffected() == 0 {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit(id,actor,method,path,outcome,status_code,organization_id)
		VALUES(gen_random_uuid(),'admin','POST','/api/login','accepted',200,$1)`, DefaultOrganizationID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return hash, nil
}

// RotateAdminPassword is an explicit operation. Ordinary startup only uses
// DBSPHERE_ADMIN_PASSWORD to bootstrap an empty store, so a configuration
// change cannot silently replace an existing administrator credential.
func (s *Postgres) RotateAdminPassword(ctx context.Context, password string) error {
	if len(password) < 12 {
		return errors.New("DBSPHERE_ADMIN_PASSWORD must contain at least 12 characters")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash administrator password: %w", err)
	}
	result, err := s.pool.Exec(ctx, "UPDATE settings SET value=$1 WHERE key='admin_password_hash'", hash)
	if err != nil {
		return fmt.Errorf("rotate administrator password: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("administrator password is not initialized")
	}
	return nil
}

// ViewerPasswordHash reconciles the optional global read-only account with
// startup configuration. Removing the password disables the account.
func (s *Postgres) ViewerPasswordHash(ctx context.Context, bootstrapPassword string) (string, error) {
	var stored string
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='viewer_password_hash'").Scan(&stored)
	if err == nil {
		if bootstrapPassword == "" {
			_, err := s.pool.Exec(ctx, "DELETE FROM settings WHERE key='viewer_password_hash'")
			return "", err
		}
		if security.VerifyPassword(bootstrapPassword, stored) {
			return stored, nil
		}
		hash, err := security.HashPassword(bootstrapPassword)
		if err != nil {
			return "", err
		}
		if _, err = s.pool.Exec(ctx, "UPDATE settings SET value=$1 WHERE key='viewer_password_hash'", hash); err != nil {
			return "", err
		}
		return hash, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if bootstrapPassword == "" {
		return "", nil
	}
	if len(bootstrapPassword) < 12 {
		return "", errors.New("DBSPHERE_VIEWER_PASSWORD must contain at least 12 characters")
	}
	hash, err := security.HashPassword(bootstrapPassword)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO settings(key,value) VALUES('viewer_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash); err != nil {
		return "", err
	}
	if err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='viewer_password_hash'").Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

// ApproverPasswordHash reconciles a separate approval-only account. A changed
// password invalidates its sessions without changing the administrator key.
func (s *Postgres) ApproverPasswordHash(ctx context.Context, password string) (string, error) {
	var stored string
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='approver_password_hash'").Scan(&stored)
	if err == nil {
		if password == "" {
			_, err = s.pool.Exec(ctx, "DELETE FROM settings WHERE key='approver_password_hash'")
			return "", err
		}
		if security.VerifyPassword(password, stored) {
			return stored, nil
		}
		hash, err := security.HashPassword(password)
		if err != nil {
			return "", err
		}
		_, err = s.pool.Exec(ctx, "UPDATE settings SET value=$1 WHERE key='approver_password_hash'", hash)
		return hash, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if password == "" {
		return "", nil
	}
	if len(password) < 12 {
		return "", errors.New("DBSPHERE_APPROVER_PASSWORD must contain at least 12 characters")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return "", err
	}
	if _, err = s.pool.Exec(ctx, "INSERT INTO settings(key,value) VALUES('approver_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash); err != nil {
		return "", err
	}
	err = s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='approver_password_hash'").Scan(&stored)
	return stored, err
}

func (s *Postgres) Close() { s.pool.Close() }
