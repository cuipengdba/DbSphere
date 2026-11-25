package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

var ErrAssetImportConflict = errors.New("asset import plan changed or has conflicts")
var importEnvReference = regexp.MustCompile(`^DBSPHERE_IMPORT_[A-Z0-9_]{1,64}$`)

type importAsset struct {
	assets.Declaration
	encrypted string
}

type importRow struct {
	Index     int            `json:"index"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name"`
	Endpoint  string         `json:"endpoint"`
	Runtime   string         `json:"runtime"`
	Action    string         `json:"action"`
	Reason    string         `json:"reason,omitempty"`
	Changes   []string       `json:"changes,omitempty"`
	Discovery map[string]any `json:"discovery,omitempty"`
}

type importPlan struct {
	Rows     []importRow `json:"rows"`
	Revision string      `json:"revision"`
	CanApply bool        `json:"canApply"`
}

func (s *Postgres) ImportAssets(ctx context.Context, request assets.ImportRequest, apply bool, key []byte) (any, error) {
	items, err := assets.ParseImport(request)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if apply {
		if _, err := tx.Exec(ctx, "LOCK TABLE assets IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return nil, err
		}
	}
	known, err := importAssetRows(ctx, tx, items)
	if err != nil {
		return nil, err
	}
	profiles := make([]assets.Input, len(items))
	plan := makeImportPlan(items, known, profiles, key)
	if apply {
		if !plan.CanApply || request.Revision != plan.Revision {
			return plan, ErrAssetImportConflict
		}
		for i, row := range plan.Rows {
			var raw json.RawMessage
			switch row.Action {
			case "create":
				raw, err = createAsset(ctx, tx, profiles[i], key, row.ID)
			case "update":
				raw, err = updateAsset(ctx, tx, row.ID, profiles[i], key)
			}
			if err != nil {
				return nil, err
			}
			if row.Action == "create" || row.Action == "update" {
				var placed struct{ ID string }
				if json.Unmarshal(raw, &placed) != nil || placed.ID == "" {
					return nil, ErrAssetImportConflict
				}
				plan.Rows[i].ID = placed.ID
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if !apply && request.Discover {
		readImportDiscovery(ctx, plan.Rows, profiles)
	}
	return plan, nil
}

func readImportDiscovery(ctx context.Context, rows []importRow, profiles []assets.Input) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for i := range jobs {
				input := profiles[i]
				rows[i].Discovery = collector.DiscoverAssetPod(ctx, collector.KubernetesTarget{Cluster: input.K8sCluster,
					Namespace: input.Namespace, Pod: input.Pod, Workload: input.Workload}, input.Port)
			}
		})
	}
	for i, row := range rows {
		if row.Runtime == "k8s" && row.Action != "conflict" && row.Action != "invalid" {
			jobs <- i
		}
	}
	close(jobs)
	workers.Wait()
}

func importAssetRows(ctx context.Context, tx pgx.Tx, declarations []assets.Declaration) ([]importAsset, error) {
	hosts, ids := []string{}, []string{}
	for _, item := range declarations {
		if item.Host != "" {
			hosts = append(hosts, strings.ToLower(item.Host))
		}
		for _, id := range []string{item.ID, strings.TrimPrefix(item.CredentialRef, "asset:")} {
			if evidenceUUID.MatchString(id) {
				ids = append(ids, id)
			}
		}
	}
	rows, err := tx.Query(ctx, "SELECT "+assetJSON+",password_enc FROM assets WHERE lower(host)=ANY($1::text[]) OR id=ANY($2::uuid[])", hosts, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := []importAsset{}
	for rows.Next() {
		var raw []byte
		var entry importAsset
		if err := rows.Scan(&raw, &entry.encrypted); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &entry.Declaration); err != nil {
			return nil, err
		}
		known = append(known, entry)
	}
	return known, rows.Err()
}

func importEndpoint(input assets.Input) string {
	return fmt.Sprintf("%s:%d/%s", strings.ToLower(input.Host), input.Port, input.Database)
}

func makeImportPlan(items []assets.Declaration, known []importAsset, profiles []assets.Input, key []byte) importPlan {
	byID := map[string]importAsset{}
	byEndpoint := map[string][]importAsset{}
	for _, entry := range known {
		byID[entry.ID] = entry
		endpoint := importEndpoint(entry.Input)
		byEndpoint[endpoint] = append(byEndpoint[endpoint], entry)
	}
	plan := importPlan{CanApply: true, Rows: make([]importRow, len(items))}
	fingerprints := make([]any, len(items))
	used := map[string]bool{}
	for i, item := range items {
		row := importRow{Index: i + 1, ID: item.ID, Name: item.Name, Endpoint: importEndpoint(item.Input), Runtime: item.Runtime}
		current := byID[item.ID]
		matched := byEndpoint[row.Endpoint]
		if item.ID == "" && len(matched) == 1 {
			current = matched[0]
			row.ID = current.ID
		}
		sameExplicitTarget := item.ID != "" && current.ID != "" && importEndpoint(current.Input) == row.Endpoint
		switch {
		case item.Issue != "":
			row.Action, row.Reason = "invalid", item.Issue
		case item.Input.Runtime == assets.RuntimeMySQL:
			row.Action, row.Reason = "invalid", "MySQL 预览资产使用单实例纳管入口"
		case item.Input.Runtime == assets.RuntimeDM8:
			row.Action, row.Reason = "invalid", "达梦 DM8 资产使用单实例纳管入口"
		case item.Input.Runtime == assets.RuntimeKingbase:
			row.Action, row.Reason = "invalid", "金仓 V9 资产使用单实例纳管入口"
		case item.Input.Runtime == assets.RuntimeRedis:
			row.Action, row.Reason = "invalid", "Redis 资产使用单实例纳管入口"
		case item.ID != "" && !evidenceUUID.MatchString(item.ID) || item.Input.Validate() != nil:
			row.Action, row.Reason = "invalid", "资产标识或连接参数无效"
		case !sameExplicitTarget && (len(matched) > 1 || len(matched) == 1 && matched[0].ID != row.ID):
			row.Action, row.Reason = "conflict", "目标连接已对应其他资产或有多个匹配"
		case !sameExplicitTarget && used["endpoint/"+row.Endpoint] || row.ID != "" && used["id/"+row.ID]:
			row.Action, row.Reason = "conflict", "清单中重复声明同一连接或资产 ID"
		default:
			profile, changes, err := importProfile(item, current, byID, key)
			profiles[i] = profile
			if err != nil {
				row.Action, row.Reason = "invalid", err.Error()
			} else if current.ID == "" {
				row.Action = "create"
			} else if len(changes) != 0 {
				row.Action, row.Changes = "update", changes
			} else {
				row.Action = "unchanged"
			}
		}
		plan.CanApply = plan.CanApply && row.Action != "conflict" && row.Action != "invalid"
		plan.Rows[i] = row
		used["endpoint/"+row.Endpoint] = true
		if row.ID != "" {
			used["id/"+row.ID] = true
		}
		fingerprints[i] = []any{row, canonicalProfile(current.Input), canonicalProfile(profiles[i]), item.CredentialRef}
	}
	raw, _ := json.Marshal(fingerprints)
	digest := sha256.Sum256(raw)
	plan.Revision = hex.EncodeToString(digest[:])
	return plan
}

func importProfile(item assets.Declaration, current importAsset, byID map[string]importAsset, key []byte) (assets.Input, []string, error) {
	profile := item.Input
	if current.ID != "" {
		if profile.ClusterName == nil {
			profile.ClusterName = current.ClusterName
		}
		if profile.Environment == nil {
			profile.Environment = current.Environment
		}
		if profile.Tags == nil {
			profile.Tags = current.Tags
		}
		if profile.PoolerEndpoint == "" {
			profile.PoolerEndpoint = current.PoolerEndpoint
		}
	}
	invalid := errors.New("凭据引用不可用或监控用户名不匹配")
	switch ref := item.CredentialRef; {
	case ref == "":
		if current.ID == "" || current.Username != profile.Username {
			return profile, nil, invalid
		}
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		if !importEnvReference.MatchString(name) {
			return profile, nil, invalid
		}
		profile.Password = os.Getenv(name)
		if profile.Password == "" || len(profile.Password) > 1024 {
			return profile, nil, invalid
		}
	case strings.HasPrefix(ref, "asset:"):
		source := byID[strings.TrimPrefix(ref, "asset:")]
		if source.ID == "" || source.Username != profile.Username {
			return profile, nil, invalid
		}
		if source.ID != current.ID {
			password, err := security.Decrypt(key, source.encrypted)
			if err != nil {
				return profile, nil, invalid
			}
			profile.Password = password
		}
	default:
		return profile, nil, invalid
	}
	previous, desired := canonicalProfile(current.Input), canonicalProfile(profile)
	changes := []string{}
	for field, value := range desired {
		if !reflect.DeepEqual(previous[field], value) {
			changes = append(changes, field)
		}
	}
	if profile.Password != "" && current.ID != "" {
		password, err := security.Decrypt(key, current.encrypted)
		if err != nil || password != profile.Password {
			changes = append(changes, "credentialRef")
		} else {
			profile.Password = ""
		}
	}
	sort.Strings(changes)
	return profile, changes, nil
}

func canonicalProfile(profile assets.Input) map[string]any {
	profile.Password = ""
	profile.ClusterName, profile.Environment = profile.MonitoringClusterName(), profile.MonitoringEnvironment()
	if profile.ClusterName != nil && *profile.ClusterName == "" {
		profile.ClusterName = nil
	}
	if profile.Environment != nil && *profile.Environment == "" {
		profile.Environment = nil
	}
	tags := profile.MonitoringTags()
	profile.Tags = &tags
	if len(tags) == 0 {
		profile.Tags = nil
	}
	raw, _ := json.Marshal(profile)
	var canonical map[string]any
	_ = json.Unmarshal(raw, &canonical)
	return canonical
}

func (s *Postgres) ExportAssets(ctx context.Context, after string) (any, error) {
	if after != "" && !evidenceUUID.MatchString(after) {
		return nil, assets.ErrInvalidImport
	}
	values, err := s.queryJSON(ctx, "SELECT "+assetJSON+" FROM assets WHERE runtime IN ('linux','k8s','managed') AND ($1::uuid IS NULL OR id>$1) ORDER BY id LIMIT 101", nullable(after))
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, min(100, len(values)))
	var next string
	for _, raw := range values[:min(100, len(values))] {
		var item assets.Declaration
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		profile := canonicalProfile(item.Input)
		for _, hidden := range []string{"password", "poolerUrl", "poolerClear", "patroniUrl", "patroniClear"} {
			delete(profile, hidden)
		}
		profile["id"], profile["credentialRef"] = item.ID, "asset:"+item.ID
		items = append(items, profile)
		next = item.ID
	}
	if len(values) <= 100 {
		next = ""
	}
	return map[string]any{"assets": items, "next": next}, nil
}

func (s *Postgres) ProbeImportedAssets(ctx context.Context, ids []string, key []byte) (any, error) {
	if len(ids) < 1 || len(ids) > 5 {
		return nil, assets.ErrInvalidImport
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !evidenceUUID.MatchString(id) || seen[id] {
			return nil, assets.ErrInvalidImport
		}
		seen[id] = true
	}
	results := make([]map[string]any, len(ids))
	var workers sync.WaitGroup
	for i, id := range ids {
		workers.Go(func() {
			timeout, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			result, err := s.CheckAsset(timeout, id, key)
			row := map[string]any{"id": id, "status": result.Status, "sampleId": result.SampleID}
			if err != nil {
				row["status"] = "unavailable"
			} else {
				if report, err := s.assetReadiness(timeout, id, &result.CollectedAt); err == nil && report.SampleID == result.SampleID {
					row["readiness"] = report
				}
				var check json.RawMessage
				if err := s.pool.QueryRow(timeout, `SELECT jsonb_build_object('pgStatus',metrics_json#>'{collector,pgStatus}',
					'versionNum',metrics_json->'version_num','monitorRole',metrics_json->'monitor_role',
					'privilegedRole',metrics_json->'privileged_role','reason',error)
					FROM samples WHERE asset_id=$1 AND collected_at=$2 AND id=$3`, id, result.CollectedAt, result.SampleID).Scan(&check); err == nil {
					row["postgres"] = check
				}
			}
			results[i] = row
		})
	}
	workers.Wait()
	return map[string]any{"results": results}, nil
}

func (s *Postgres) CollectionCoverage(ctx context.Context, interval int) (any, error) {
	var report json.RawMessage
	err := s.pool.QueryRow(ctx, `WITH target AS (
		SELECT count(*) AS total,count(*) FILTER (WHERE last_sample_at IS NULL OR last_sample_at<=now()-make_interval(secs=>$1)) AS due,
		count(*) FILTER (WHERE last_status='ok' AND last_sample_at>now()-make_interval(secs=>2*$1)) AS fresh
		FROM assets WHERE runtime IN ('linux','k8s','managed')
	), days AS (
		SELECT date_trunc('day', collected_at,'UTC') AS day,count(*) AS total,count(*) FILTER (
			WHERE coalesce(metrics_json#>>'{collector,pgStatus}',status)='ok') AS ok,
			count(*) FILTER (WHERE status<>'ok') AS failed,
			count(DISTINCT asset_id) AS observed,sum(pg_column_size(metrics_json)) AS bytes
		FROM samples WHERE collected_at>=date_trunc('day',now(),'UTC')-interval '6 days'
		GROUP BY 1
	), bucket AS (
		SELECT generate_series(date_trunc('day',now(),'UTC')-interval '6 days',date_trunc('day',now(),'UTC'),interval '1 day') AS day
	) SELECT jsonb_build_object('targets',target.total,'due',target.due,'fresh',target.fresh,'days',(
		SELECT jsonb_agg(jsonb_build_object('day',bucket.day,'samples',coalesce(days.total,0),
			'postgresOK',coalesce(days.ok,0),'failed',coalesce(days.failed,0),'observed',coalesce(days.observed,0),
			'sampleBytes',coalesce(days.bytes,0)) ORDER BY bucket.day) FROM bucket LEFT JOIN days USING(day)))
	FROM target`, interval).Scan(&report)
	return report, err
}
