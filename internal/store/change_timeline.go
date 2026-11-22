package store

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidChange = errors.New("变更类型、发生时间或标题无效")

type ChangeInput struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	OccurredAt string `json:"occurredAt"`
}

type changeField struct {
	Name   string `json:"name"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func migrateChangeTimeline(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS asset_changes (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		kind text NOT NULL, label text NOT NULL, source text NOT NULL,
		occurred_at timestamptz NOT NULL, recorded_at timestamptz NOT NULL DEFAULT now(),
		before_sample uuid, before_at timestamptz, after_sample uuid, after_at timestamptz,
		fields jsonb NOT NULL DEFAULT '[]'::jsonb,
		UNIQUE (asset_id,occurred_at,kind,source,label)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(40) ON CONFLICT DO NOTHING`)
	return err
}

func (s *Postgres) AddChange(ctx context.Context, id string, input ChangeInput) (any, error) {
	label := strings.TrimSpace(input.Label)
	at, err := time.Parse(time.RFC3339Nano, input.OccurredAt)
	if err != nil || strings.ContainsAny(label, "\r\n\x00") || label == "" || len(label) > 480 ||
		at.IsZero() || at.After(time.Now().Add(2*time.Minute)) {
		return nil, ErrInvalidChange
	}
	switch input.Kind {
	case "configuration", "image", "deployment", "maintenance", "route", "topology":
	default:
		return nil, ErrInvalidChange
	}
	var changeID string
	var recorded time.Time
	err = s.pool.QueryRow(ctx, `INSERT INTO asset_changes(asset_id,kind,label,source,occurred_at)
		SELECT id,$2,$3,'operator',$4 FROM assets WHERE id=$1
		ON CONFLICT(asset_id,occurred_at,kind,source,label) DO UPDATE SET label=excluded.label
		RETURNING id::text,recorded_at`, id, input.Kind, label, at.UTC()).Scan(&changeID, &recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": changeID, "assetId": id, "kind": input.Kind, "label": label,
		"source": "operator", "occurredAt": at.UTC(), "recordedAt": recorded}, nil
}

func (s *Postgres) changeTimeline(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 24*time.Hour, 31*24*time.Hour)
	if err != nil {
		return nil, err
	}
	values, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',c.id,'assetId',c.asset_id,'kind',c.kind,
		'label',c.label,'source',c.source,'occurredAt',c.occurred_at,'recordedAt',c.recorded_at,'fields',c.fields,
		'before',prior.sample,'after',following.sample)
		FROM (SELECT * FROM asset_changes WHERE asset_id=$1 AND occurred_at >= $2 AND occurred_at < $3
			ORDER BY occurred_at DESC,id DESC LIMIT 101) c
		LEFT JOIN LATERAL (
			SELECT jsonb_build_object('id',s.id,'collectedAt',s.collected_at,'status',s.status) AS sample
			FROM samples s WHERE s.asset_id=c.asset_id
			AND s.collected_at >= coalesce(c.before_at,c.occurred_at-interval '30 minutes')
			AND s.collected_at < c.occurred_at AND (c.before_sample IS NULL OR s.id=c.before_sample)
			ORDER BY s.collected_at DESC,s.id DESC LIMIT 1
		) prior ON true
		LEFT JOIN LATERAL (
			SELECT jsonb_build_object('id',s.id,'collectedAt',s.collected_at,'status',s.status) AS sample
			FROM samples s WHERE s.asset_id=c.asset_id
			AND s.collected_at >= c.occurred_at
			AND s.collected_at <= coalesce(c.after_at,c.occurred_at+interval '30 minutes')
			AND (c.after_sample IS NULL OR s.id=c.after_sample)
			ORDER BY s.collected_at,s.id LIMIT 1
		) following ON true
		ORDER BY c.occurred_at DESC,c.id DESC`, id, from, to)
	if err != nil {
		return nil, err
	}
	truncated := len(values) > 100
	if truncated {
		values = values[:100]
	}
	return map[string]any{"changes": values, "truncated": truncated}, nil
}

type changeState map[string]map[string]string

func (state changeState) add(category, field, value string) {
	if value == "" {
		return
	}
	if state[category] == nil {
		state[category] = map[string]string{}
	}
	state[category][field] = value
}

func observeChangeState(metrics map[string]any) changeState {
	state := changeState{}
	settings := object(object(metrics["diagnostics"])["settings"])
	if settings["status"] == "ok" {
		for _, raw := range list(settings["data"]) {
			row := object(raw)
			name := stringValue(row["name"])
			switch name {
			case "archive_mode", "autovacuum", "log_min_duration_statement", "max_connections", "max_slot_wal_keep_size", "wal_level":
				value := stringValue(row["value"])
				if unit := stringValue(row["unit"]); unit != "" && value != "" {
					value += " " + unit
				}
				state.add("configuration", name, value)
			}
		}
	}
	state.add("topology", "PostgreSQL 角色", stringValue(metrics["role"]))
	kube := object(metrics["k8s"])
	if kube["status"] == "ok" {
		pod := object(kube["pod"])
		state.add("topology", "Pod UID", stringValue(pod["uid"]))
		state.add("topology", "Node", stringValue(pod["node"]))
		for _, raw := range list(kube["containers"]) {
			container := object(raw)
			name := stringValue(container["name"])
			if name != "" {
				state.add("image", name+" 镜像", stringValue(container["image"]))
				state.add("image", name+" 镜像 ID", stringValue(container["imageId"]))
			}
		}
	}
	if patroni := object(metrics["patroni"]); patroni["status"] == "ok" {
		for _, raw := range list(patroni["members"]) {
			member := object(raw)
			if name := stringValue(member["name"]); name != "" {
				state.add("topology", "Patroni "+name, stringValue(member["role"]))
			}
		}
	}
	route := object(metrics["routing"])
	state.add("route", "访问入口", stringValue(route["entry"]))
	probe := object(route["probe"])
	if probe["status"] == "ok" {
		state.add("route", "入口观测后端", stringValue(probe["backend"]))
		state.add("route", "入口观测角色", stringValue(probe["role"]))
	}
	port := object(object(route["service"])["portMapping"])
	if target, ok := port["targetPort"].(string); ok {
		state.add("route", "Service targetPort", target)
	} else if target, ok := numeric(port["targetPort"]); ok && target > 0 {
		state.add("route", "Service targetPort", strconv.Itoa(int(target)))
	}
	return state
}

func changedFields(before, after map[string]string) []changeField {
	changes := []changeField{}
	for name, value := range after {
		if prior, found := before[name]; found && prior != value {
			changes = append(changes, changeField{Name: name, Before: prior, After: value})
		}
	}
	slices.SortFunc(changes, func(a, b changeField) int { return strings.Compare(a.Name, b.Name) })
	return changes
}

func recordObservedChanges(ctx context.Context, tx pgx.Tx, asset, beforeID, afterID string, beforeAt, at time.Time,
	previous, observed map[string]any) error {
	if beforeID == "" || beforeAt.IsZero() {
		return nil
	}
	before, after := observeChangeState(previous), observeChangeState(observed)
	for _, category := range []struct{ kind, source, label string }{
		{"configuration", "pg_settings", "PostgreSQL 参数变化"},
		{"image", "kubernetes", "Kubernetes 容器镜像变化"},
		{"route", "routing", "访问入口或观测后端变化"},
		{"topology", "topology", "实例拓扑变化"},
	} {
		fields := changedFields(before[category.kind], after[category.kind])
		if len(fields) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO asset_changes(asset_id,kind,label,source,occurred_at,
			before_sample,before_at,after_sample,after_at,fields) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$5,$9)
			ON CONFLICT(asset_id,occurred_at,kind,source,label) DO NOTHING`, asset, category.kind,
			category.label, category.source, at, beforeID, beforeAt, afterID, fields); err != nil {
			return err
		}
	}
	return nil
}
