package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type PlatformReplica struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Collector       bool      `json:"collector"`
	CompletedCycles int64     `json:"completedCycles"`
	SeenAt          time.Time `json:"seenAt"`
	Online          bool      `json:"online"`
}

type PlatformLeadership struct {
	Owner      string    `json:"owner"`
	Generation int64     `json:"generation"`
	LeaseUntil time.Time `json:"leaseUntil"`
}

type PlatformStatus struct {
	Replicas    []PlatformReplica   `json:"replicas"`
	Maintenance *PlatformLeadership `json:"maintenance"`
}

func migratePlatformHA(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=46)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		ALTER TABLE collection_jobs ADD COLUMN lease_replica_id text;
		CREATE TABLE platform_replicas (
			id text PRIMARY KEY, name text NOT NULL, collector boolean NOT NULL,
			completed_cycles bigint NOT NULL, seen_at timestamptz NOT NULL
		);
		CREATE INDEX platform_replicas_seen ON platform_replicas(seen_at);
		CREATE TABLE platform_leadership (
			task text PRIMARY KEY, owner text NOT NULL, generation bigint NOT NULL,
			lease_until timestamptz NOT NULL
		);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(46)`)
	return err
}

const platformLeaseWindow = 12

func (s *Postgres) RegisterPlatformReplica(ctx context.Context, replica PlatformReplica) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO platform_replicas(id,name,collector,completed_cycles,seen_at)
		VALUES($1,$2,$3,$4,clock_timestamp()) ON CONFLICT(id) DO UPDATE
		SET name=excluded.name,collector=excluded.collector,completed_cycles=excluded.completed_cycles,seen_at=excluded.seen_at`,
		replica.ID, replica.Name, replica.Collector, replica.CompletedCycles); err != nil {
		return false, err
	}
	var acquired string
	err = tx.QueryRow(ctx, `INSERT INTO platform_leadership(task,owner,generation,lease_until)
		VALUES('maintenance',$1,1,clock_timestamp()+make_interval(secs=>$2))
		ON CONFLICT(task) DO UPDATE SET owner=excluded.owner,
		generation=platform_leadership.generation+CASE WHEN platform_leadership.owner<>excluded.owner
		  OR platform_leadership.lease_until<=clock_timestamp() THEN 1 ELSE 0 END,
		lease_until=clock_timestamp()+make_interval(secs=>$2)
		WHERE platform_leadership.lease_until<=clock_timestamp() OR platform_leadership.owner=excluded.owner
		RETURNING owner`, replica.ID, platformLeaseWindow).Scan(&acquired)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM platform_replicas WHERE seen_at<now()-interval '1 day'`); err != nil {
		return false, err
	}
	return acquired == replica.ID, tx.Commit(ctx)
}

func (s *Postgres) ReleasePlatformReplica(ctx context.Context, id string) {
	_, _ = s.pool.Exec(ctx, `UPDATE platform_leadership SET lease_until=clock_timestamp()
		WHERE task='maintenance' AND owner=$1`, id)
}

func (s *Postgres) PlatformStatus(ctx context.Context) (PlatformStatus, error) {
	status := PlatformStatus{Replicas: []PlatformReplica{}}
	rows, err := s.pool.Query(ctx, `SELECT id,name,collector,completed_cycles,seen_at,
		seen_at>clock_timestamp()-make_interval(secs=>$1)
		FROM platform_replicas ORDER BY seen_at DESC,id LIMIT 50`, platformLeaseWindow)
	if err != nil {
		return status, err
	}
	status.Replicas, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PlatformReplica])
	if err != nil {
		return status, err
	}
	var lease PlatformLeadership
	err = s.pool.QueryRow(ctx, `SELECT owner,generation,lease_until FROM platform_leadership
		WHERE task='maintenance' AND lease_until>clock_timestamp()`).Scan(&lease.Owner, &lease.Generation, &lease.LeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err == nil {
		status.Maintenance = &lease
	}
	return status, err
}
