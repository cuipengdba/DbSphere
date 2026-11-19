
  CREATE TABLE IF NOT EXISTS dbsphere_schema_migrations (
    version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
  );
  CREATE TABLE IF NOT EXISTS settings (
    key text PRIMARY KEY, value text NOT NULL
  );
  CREATE TABLE IF NOT EXISTS admin_audit (
    id uuid PRIMARY KEY, observed_at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL, method text NOT NULL, path text NOT NULL,
    outcome text NOT NULL, status_code integer, event_id uuid
  );
  CREATE INDEX IF NOT EXISTS admin_audit_time ON admin_audit(observed_at DESC);
  CREATE INDEX IF NOT EXISTS admin_audit_event_time ON admin_audit(event_id,observed_at DESC) WHERE event_id IS NOT NULL;
  CREATE TABLE IF NOT EXISTS assets (
    id uuid PRIMARY KEY, name text NOT NULL, host text NOT NULL, port integer NOT NULL,
    database_name text NOT NULL, username text NOT NULL, password_enc text NOT NULL,
    ssl_mode text NOT NULL, runtime text NOT NULL, linux_host text, linux_exporter_port integer,
    pooler_url_enc text, pooler_endpoint text, patroni_url_enc text, k8s_cluster text,
    k8s_namespace text, k8s_workload text, k8s_pod text, k8s_node text,
    created_at timestamptz NOT NULL, last_sample_at timestamptz,
    last_status text NOT NULL DEFAULT 'pending', last_error text, version_num integer, role text
  );
  ALTER TABLE assets ADD COLUMN IF NOT EXISTS linux_exporter_port integer;
  ALTER TABLE assets ADD COLUMN IF NOT EXISTS pooler_url_enc text;
  ALTER TABLE assets ADD COLUMN IF NOT EXISTS pooler_endpoint text;
  CREATE TABLE IF NOT EXISTS samples (
    id uuid NOT NULL, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    collected_at timestamptz NOT NULL, status text NOT NULL,
    metrics_json jsonb, error text, PRIMARY KEY(id,collected_at)
  ) PARTITION BY RANGE (collected_at);
  CREATE INDEX IF NOT EXISTS samples_asset_time ON samples(asset_id, collected_at DESC);
  CREATE TABLE IF NOT EXISTS event_keys (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    started_at timestamptz NOT NULL
  );
  CREATE TABLE IF NOT EXISTS events (
    id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    kind text NOT NULL, title text NOT NULL, status text NOT NULL,
    started_at timestamptz NOT NULL, ended_at timestamptz, event_group text, acknowledged_at timestamptz,
    PRIMARY KEY(id,started_at)
  ) PARTITION BY RANGE (started_at);
  ALTER TABLE events ADD COLUMN IF NOT EXISTS event_group text;
  ALTER TABLE events ADD COLUMN IF NOT EXISTS acknowledged_at timestamptz;
  UPDATE events SET event_group='telemetry' WHERE event_group IS NULL;
  CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
  DROP INDEX IF EXISTS events_one_open_kind;
  DROP INDEX IF EXISTS events_one_open_group;
  CREATE TABLE IF NOT EXISTS open_event_groups (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    event_group text NOT NULL, event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    PRIMARY KEY(asset_id,event_group)
  );
  CREATE TABLE IF NOT EXISTS evidence (
    id uuid PRIMARY KEY, event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    kind text NOT NULL, observed_at timestamptz NOT NULL, payload_json jsonb NOT NULL
  );
  CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence(event_id, observed_at);
  CREATE TABLE IF NOT EXISTS alert_rules (
    id text PRIMARY KEY, definition_json jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
  );
  CREATE TABLE IF NOT EXISTS alert_states (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    rule_id text NOT NULL REFERENCES alert_rules(id), state_json jsonb NOT NULL,
    PRIMARY KEY(asset_id,rule_id)
  );
  CREATE TABLE IF NOT EXISTS silences (
    id uuid PRIMARY KEY, kind text NOT NULL, asset_id uuid REFERENCES assets(id) ON DELETE CASCADE,
    rule_id text REFERENCES alert_rules(id), starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL, reason text NOT NULL,
    created_at timestamptz NOT NULL, revoked_at timestamptz
  );
  CREATE INDEX IF NOT EXISTS silences_active ON silences(starts_at,ends_at);
  CREATE TABLE IF NOT EXISTS notification_outbox (
    id uuid PRIMARY KEY, evidence_id uuid NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
    channel text NOT NULL, status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL,
    delivered_at timestamptz, last_error text, UNIQUE(evidence_id,channel)
  );
  CREATE INDEX IF NOT EXISTS notification_outbox_due ON notification_outbox(status,next_attempt_at);
  CREATE TABLE IF NOT EXISTS inspection_templates (
    id text PRIMARY KEY, definition_json jsonb NOT NULL, updated_at timestamptz NOT NULL
  );
  CREATE TABLE IF NOT EXISTS inspection_schedules (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    template_id text NOT NULL REFERENCES inspection_templates(id),
    interval_minutes integer NOT NULL, next_due_at timestamptz NOT NULL,
    enabled boolean NOT NULL DEFAULT true, PRIMARY KEY(asset_id,template_id)
  );
  CREATE INDEX IF NOT EXISTS inspection_schedules_due ON inspection_schedules(enabled,next_due_at);
  CREATE TABLE IF NOT EXISTS inspection_runs (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    template_id text NOT NULL REFERENCES inspection_templates(id), template_version integer NOT NULL,
    sample_id uuid, retry_of uuid, started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL, summary_json jsonb NOT NULL, findings_json jsonb NOT NULL,
    status text NOT NULL DEFAULT 'completed', error text,
    event_ids_json jsonb NOT NULL DEFAULT '[]'::jsonb
  );
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'completed';
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS error text;
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS event_ids_json jsonb NOT NULL DEFAULT '[]'::jsonb;
  CREATE INDEX IF NOT EXISTS inspection_runs_asset_time ON inspection_runs(asset_id,completed_at DESC);
  CREATE TABLE IF NOT EXISTS slow_log_entries (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source_key text NOT NULL, observed_at timestamptz NOT NULL, duration_ms double precision NOT NULL,
    query_id text, source_format text NOT NULL, imported_at timestamptz NOT NULL,
    UNIQUE(asset_id,source_key)
  );
  CREATE INDEX IF NOT EXISTS slow_log_asset_time ON slow_log_entries(asset_id,observed_at DESC);
  CREATE TABLE IF NOT EXISTS sql_plan_snapshots (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    query_id text NOT NULL, observed_at timestamptz NOT NULL, source text NOT NULL,
    shape_hash text NOT NULL, summary_json jsonb NOT NULL, imported_at timestamptz NOT NULL
  );
  CREATE INDEX IF NOT EXISTS sql_plan_asset_time ON sql_plan_snapshots(asset_id,observed_at DESC);
  CREATE TABLE IF NOT EXISTS trace_spans (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    trace_id text NOT NULL, span_id text NOT NULL, parent_span_id text, backend_pid integer, query_id text,
    route text NOT NULL DEFAULT 'direct', app_pod_reported jsonb, app_pod_verified jsonb,
    service text NOT NULL, kind text NOT NULL, started_at timestamptz NOT NULL,
    ended_at timestamptz NOT NULL, status text NOT NULL,
    received_at timestamptz, sampling_probability double precision
      CHECK (sampling_probability > 0 AND sampling_probability <= 1),
    PRIMARY KEY(asset_id,trace_id,span_id)
  );
  CREATE INDEX IF NOT EXISTS trace_spans_asset_time ON trace_spans(asset_id,started_at DESC);
  CREATE INDEX IF NOT EXISTS trace_spans_parent_lookup ON trace_spans(trace_id,parent_span_id)
    WHERE parent_span_id IS NOT NULL;
  CREATE TABLE IF NOT EXISTS trace_pending_spans (
    trace_id text NOT NULL, span_id text NOT NULL, parent_span_id text,
    service text NOT NULL, started_at timestamptz NOT NULL,
    ended_at timestamptz NOT NULL, status text NOT NULL,
    sampling_probability double precision
      CHECK (sampling_probability > 0 AND sampling_probability <= 1),
    ingress_at timestamptz,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(trace_id,span_id)
  );
  CREATE INDEX IF NOT EXISTS trace_pending_received ON trace_pending_spans(received_at);

