package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Postgres) AssetByID(ctx context.Context, id string) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT "+assetJSON+" FROM assets WHERE id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	return json.RawMessage(raw), nil
}

func (s *Postgres) CreateAsset(ctx context.Context, input assets.Input, key []byte) (json.RawMessage, error) {
	password, err := security.Encrypt(key, input.Password)
	if err != nil {
		return nil, err
	}
	var pooler any
	if input.PoolerURL != "" {
		encoded, err := security.Encrypt(key, input.PoolerURL)
		if err != nil {
			return nil, err
		}
		pooler = encoded
	}
	var patroni any
	if input.PatroniURL != "" {
		encoded, err := security.Encrypt(key, input.PatroniURL)
		if err != nil {
			return nil, err
		}
		patroni = encoded
	}
	var id string
	err = s.pool.QueryRow(ctx, `INSERT INTO assets
		(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,
		linux_host,linux_exporter_port,pooler_url_enc,patroni_url_enc,k8s_cluster,k8s_namespace,
		k8s_workload,k8s_pod,k8s_node,pooler_endpoint,cluster_name,created_at)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,nullif($19,''),now())
		RETURNING id`, input.Name, input.Host, input.Port, input.Database, input.Username, password,
		input.SSLMode, input.Runtime, nullable(input.LinuxHost), nullablePort(input.LinuxExporterPort),
		pooler, patroni, nullable(input.K8sCluster), nullable(input.Namespace), nullable(input.Workload),
		nullable(input.Pod), nullable(input.Node), nullable(input.PoolerEndpoint), input.MonitoringClusterName()).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL asset: %w", err)
	}
	return s.AssetByID(ctx, id)
}

func nullablePort(value assets.OptionalPort) any {
	if value == 0 {
		return nil
	}
	return int(value)
}

func (s *Postgres) UpdateAsset(ctx context.Context, id string, input assets.Input, key []byte) (json.RawMessage, error) {
	var password any
	if input.Password != "" {
		encoded, err := security.Encrypt(key, input.Password)
		if err != nil {
			return nil, err
		}
		password = encoded
	}
	var pooler any
	if input.PoolerURL != "" {
		encoded, err := security.Encrypt(key, input.PoolerURL)
		if err != nil {
			return nil, err
		}
		pooler = encoded
	}
	var patroni any
	if input.PatroniURL != "" {
		encoded, err := security.Encrypt(key, input.PatroniURL)
		if err != nil {
			return nil, err
		}
		patroni = encoded
	}
	var updated string
	err := s.pool.QueryRow(ctx, `UPDATE assets SET
		name=$1,host=$2,port=$3,database_name=$4,username=$5,
		password_enc=coalesce($6,password_enc),ssl_mode=$7,runtime=$8,
		linux_host=$9,linux_exporter_port=$10,
		pooler_url_enc=CASE WHEN $11 THEN NULL ELSE coalesce($12,pooler_url_enc) END,
		patroni_url_enc=CASE WHEN $13 THEN NULL ELSE coalesce($14,patroni_url_enc) END,
		k8s_cluster=$15,k8s_namespace=$16,k8s_workload=$17,k8s_pod=$18,k8s_node=$19,
		pooler_endpoint=$21,cluster_name=CASE WHEN $22::text IS NULL THEN cluster_name ELSE nullif($22,'') END,
		last_sample_at=NULL,last_status='pending',last_error=NULL,version_num=NULL,role=NULL
		WHERE id=$20 RETURNING id`, input.Name, input.Host, input.Port, input.Database, input.Username,
		password, input.SSLMode, input.Runtime, nullable(input.LinuxHost), nullablePort(input.LinuxExporterPort),
		input.PoolerClear, pooler, input.PatroniClear, patroni, nullable(input.K8sCluster), nullable(input.Namespace),
		nullable(input.Workload), nullable(input.Pod), nullable(input.Node), id,
		nullable(input.PoolerEndpoint), input.MonitoringClusterName()).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update PostgreSQL asset: %w", err)
	}
	return s.AssetByID(ctx, updated)
}

func (s *Postgres) DeleteAsset(ctx context.Context, id string) (bool, error) {
	command, err := s.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", id)
	if err != nil {
		return false, fmt.Errorf("delete PostgreSQL asset: %w", err)
	}
	return command.RowsAffected() != 0, nil
}
