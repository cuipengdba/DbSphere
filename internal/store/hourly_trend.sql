CREATE TABLE monitoring_hourly_buckets (
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  bucket timestamptz NOT NULL,
  value jsonb NOT NULL,
  PRIMARY KEY (asset_id,bucket)
);
CREATE INDEX monitoring_hourly_buckets_time ON monitoring_hourly_buckets(bucket);

CREATE TABLE monitoring_hourly_status (
  singleton boolean PRIMARY KEY CHECK (singleton),
  first_hour timestamptz NOT NULL,
  through_hour timestamptz NOT NULL,
  CHECK (first_hour <= through_hour)
);
INSERT INTO monitoring_hourly_status
  SELECT true, hour_start, hour_start FROM (SELECT date_trunc('hour',now(),'UTC') AS hour_start) init;

CREATE TABLE monitoring_hourly_dirty (
  bucket timestamptz NOT NULL,
  asset_id uuid NOT NULL,
  PRIMARY KEY (bucket,asset_id)
);

CREATE FUNCTION dbsphere_mark_hourly_dirty(target_id uuid, sample_at timestamptz)
RETURNS void LANGUAGE sql AS $$
  INSERT INTO monitoring_hourly_dirty(bucket,asset_id)
  SELECT DISTINCT bucket,target_id FROM (
    VALUES (date_trunc('hour',sample_at,'UTC')),
           (date_trunc('hour',sample_at + interval '10 minutes','UTC'))
  ) AS affected(bucket)
  WHERE bucket >= date_trunc('hour',now(),'UTC') - interval '31 days'
    AND bucket <= date_trunc('hour',now(),'UTC') + interval '1 hour'
  ORDER BY bucket
  ON CONFLICT (bucket,asset_id) DO UPDATE SET asset_id=excluded.asset_id
$$;

CREATE FUNCTION dbsphere_invalidate_hourly_sample()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP <> 'INSERT' THEN
    PERFORM dbsphere_mark_hourly_dirty(OLD.asset_id,OLD.collected_at);
  END IF;
  IF TG_OP <> 'DELETE' THEN
    PERFORM dbsphere_mark_hourly_dirty(NEW.asset_id,NEW.collected_at);
  END IF;
  RETURN NULL;
END;
$$;

CREATE TRIGGER monitoring_hourly_sample_change
  AFTER INSERT OR UPDATE OR DELETE ON samples
  FOR EACH ROW EXECUTE FUNCTION dbsphere_invalidate_hourly_sample();
