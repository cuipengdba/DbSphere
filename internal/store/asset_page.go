package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

var ErrInvalidAssetPage = errors.New("invalid asset page query")

type AssetPageQuery struct {
	Search         string
	Runtime        string
	Cursor         string
	Limit          int
	OrganizationID string
}

const assetJSON = `jsonb_build_object(
	'id',id,'organizationId',organization_id,'name',name,'clusterName',cluster_name,'host',host,'port',port,'database',database_name,
	'environment',environment,'tags',tags,
	'username',username,'sslMode',ssl_mode,'runtime',runtime,
	'engine',CASE WHEN runtime IN ('mysql','dm8','kingbase','redis','etcd') THEN runtime ELSE 'postgresql' END,
	'linuxHost',linux_host,'linuxExporterPort',linux_exporter_port,
	'poolerConfigured',pooler_url_enc IS NOT NULL,'poolerEndpoint',pooler_endpoint,
	'patroniConfigured',patroni_url_enc IS NOT NULL,
	'k8sCluster',k8s_cluster,'namespace',k8s_namespace,'workload',k8s_workload,
	'pod',k8s_pod,'node',k8s_node,'createdAt',created_at,
	'lastSampleAt',last_sample_at,'lastStatus',last_status,
	'lastError',last_error,'versionNum',version_num,'role',role,
	'pg14Eol',runtime IN ('linux','k8s','managed') AND coalesce(version_num/10000=14,false))`

// AssetPage searches all managed assets with a stable (created_at, ID) cursor.
// A scoped viewer's asset filter is applied before the page limit.
func (s *Postgres) AssetPage(ctx context.Context, query AssetPageQuery, assetIDs []string) (any, error) {
	if query.Limit < 1 || query.Limit > 100 || len(query.Cursor) > 256 ||
		utf8.RuneCountInString(strings.TrimSpace(query.Search)) > 128 {
		return nil, ErrInvalidAssetPage
	}
	if query.Runtime != "" && query.Runtime != "all" && !assets.IsSupportedRuntime(query.Runtime) {
		return nil, ErrInvalidAssetPage
	}
	var beforeTime time.Time
	var beforeID string
	if query.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil {
			return nil, ErrInvalidAssetPage
		}
		parts := strings.Split(string(decoded), "|")
		if len(parts) != 2 || !evidenceUUID.MatchString(parts[1]) {
			return nil, ErrInvalidAssetPage
		}
		beforeTime, err = time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return nil, ErrInvalidAssetPage
		}
		beforeID = parts[1]
	}
	args := []any{}
	conditions := []string{}
	parameter := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if assetIDs != nil {
		conditions = append(conditions, "id=ANY("+parameter(assetIDs)+"::uuid[])")
	}
	if query.OrganizationID != "" {
		conditions = append(conditions, "organization_id="+parameter(query.OrganizationID)+"::uuid")
	}
	if query.Runtime != "" && query.Runtime != "all" {
		conditions = append(conditions, "runtime="+parameter(query.Runtime))
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		p := parameter(strings.ToLower(search))
		conditions = append(conditions, "(strpos(lower(name),"+p+"::text)>0 OR "+
			"strpos(lower(host),"+p+"::text)>0 OR strpos(id::text,"+p+"::text)>0)")
	}
	if !beforeTime.IsZero() {
		t := parameter(beforeTime)
		id := parameter(beforeID)
		conditions = append(conditions, "(created_at,id)<("+t+","+id+"::uuid)")
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	limit := parameter(query.Limit + 1)
	rows, err := s.pool.Query(ctx, "SELECT "+assetJSON+",created_at,id::text FROM assets"+
		where+" ORDER BY created_at DESC,id DESC LIMIT "+limit, args...)
	if err != nil {
		return nil, fmt.Errorf("read asset page: %w", err)
	}
	defer rows.Close()
	type item struct {
		json json.RawMessage
		at   time.Time
		id   string
	}
	items := make([]item, 0, query.Limit+1)
	for rows.Next() {
		var row item
		if err := rows.Scan(&row.json, &row.at, &row.id); err != nil {
			return nil, fmt.Errorf("scan asset page: %w", err)
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read asset page: %w", err)
	}
	hasMore := len(items) > query.Limit
	if hasMore {
		items = items[:query.Limit]
	}
	assets := make([]json.RawMessage, 0, len(items))
	for _, row := range items {
		assets = append(assets, row.json)
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id))
	}
	return map[string]any{"assets": assets, "hasMore": hasMore, "nextCursor": nextCursor}, nil
}
