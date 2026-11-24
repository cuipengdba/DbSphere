CREATE TABLE log_projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, name)
);

CREATE TABLE log_instances (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES log_projects(id) ON DELETE CASCADE,
  asset_id uuid REFERENCES assets(id) ON DELETE SET NULL,
  environment text NOT NULL,
  name text NOT NULL,
  engine text NOT NULL,
  version text NOT NULL,
  UNIQUE (project_id, environment, name)
);

CREATE TABLE log_batches (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  instance_id uuid NOT NULL REFERENCES log_instances(id) ON DELETE CASCADE,
  name text NOT NULL,
  coverage text NOT NULL,
  coverage_note text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE log_files (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  batch_id uuid NOT NULL REFERENCES log_batches(id) ON DELETE CASCADE,
  filename text NOT NULL,
  node text NOT NULL,
  expected_size bigint NOT NULL CHECK (expected_size > 0),
  format text NOT NULL,
  detected_format text NOT NULL DEFAULT '',
  prefix text NOT NULL,
  zone text NOT NULL,
  state text NOT NULL DEFAULT 'uploading' CHECK (state IN ('uploading','queued','complete','partial','failed','duplicate')),
  reason text NOT NULL DEFAULT '',
  raw_size bigint NOT NULL DEFAULT 0,
  stored_size bigint NOT NULL DEFAULT 0,
  parts integer NOT NULL DEFAULT 0,
  record_count integer NOT NULL DEFAULT 0,
  parsed_count integer NOT NULL DEFAULT 0,
  duplicate_of uuid REFERENCES log_files(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE log_source_keys (
  instance_id uuid NOT NULL REFERENCES log_instances(id) ON DELETE CASCADE,
  node text NOT NULL,
  digest text NOT NULL,
  file_id uuid NOT NULL REFERENCES log_files(id) ON DELETE CASCADE,
  PRIMARY KEY (instance_id, node, digest)
);

CREATE TABLE log_archive_blocks (
  file_id uuid NOT NULL REFERENCES log_files(id) ON DELETE CASCADE,
  part integer NOT NULL,
  raw_size integer NOT NULL,
  digest text NOT NULL,
  compressed bytea NOT NULL,
  PRIMARY KEY (file_id, part)
);
ALTER TABLE log_archive_blocks ALTER COLUMN compressed SET STORAGE EXTERNAL;

CREATE TABLE log_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  batch_id uuid NOT NULL REFERENCES log_batches(id) ON DELETE CASCADE,
  file_id uuid NOT NULL REFERENCES log_files(id) ON DELETE CASCADE,
  record_key text,
  raw_offset bigint NOT NULL,
  raw_length bigint NOT NULL,
  observed_at timestamptz,
  kind text NOT NULL,
  severity text NOT NULL,
  database_name text NOT NULL,
  application text NOT NULL,
  session_key text NOT NULL,
  process_id text NOT NULL,
  blocker_id text NOT NULL,
  sqlstate text NOT NULL,
  sql_shape text NOT NULL,
  fingerprint text NOT NULL,
  command text NOT NULL,
  duration_ms double precision,
  temp_bytes bigint NOT NULL,
  UNIQUE (file_id, raw_offset),
  UNIQUE (batch_id, record_key)
);

CREATE INDEX log_instances_project ON log_instances(project_id);
CREATE INDEX log_batches_instance ON log_batches(instance_id, created_at DESC);
CREATE INDEX log_files_batch ON log_files(batch_id);
CREATE INDEX log_files_queue ON log_files(updated_at, id) WHERE state='queued';
CREATE INDEX log_records_batch_time ON log_records(batch_id, observed_at DESC NULLS LAST, id);
CREATE INDEX log_records_batch_kind ON log_records(batch_id, kind, observed_at DESC NULLS LAST, id);
CREATE INDEX log_records_batch_fingerprint ON log_records(batch_id, fingerprint) WHERE fingerprint <> '';

INSERT INTO dbsphere_schema_migrations(version) VALUES (53);
