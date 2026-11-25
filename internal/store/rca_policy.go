package store

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRCARuleNotFound = errors.New("根因规则不存在")
var ErrInvalidRCARule = errors.New("根因规则的版本、证据条件或变更原因无效")
var ErrRCARuleConflict = errors.New("规则已更新，请读取最新版本")

var rcaRuleTitles = map[string]string{
	"pod_oom_correlated":         "Pod OOM 与数据库不可用",
	"k8s_pod_not_ready":          "Kubernetes Pod 未就绪",
	"connection_pressure":        "PostgreSQL 连接压力",
	"lock_chain":                 "PostgreSQL 锁阻塞链",
	"replication_lag":            "复制回放延迟",
	"logical_slot_wal_retention": "逻辑复制槽保留 WAL",
	"wal_receiver_down":          "WAL 接收器中断",
	"patroni_leader_anomaly":     "Patroni 主库状态异常",
	"wal_archive_failures":       "WAL 归档失败",
	"autovacuum_disabled":        "Autovacuum 已关闭",
	"vacuum_backlog":             "表清理积压",
	"linux_cpu_pressure":         "Linux CPU 压力",
	"linux_memory_pressure":      "Linux 内存压力",
	"linux_disk_pressure":        "Linux 文件系统空间压力",
	"pooler_exhausted":           "PgBouncer 池占满",
	"pooler_queue":               "PgBouncer 连接排队",
	"route_backend_mismatch":     "代理入口转发错位",
	"service_entry_unreachable":  "Kubernetes Service 入口不可达",
	"route_entry_unreachable":    "数据库入口探测失败",
	"pvc_pressure":               "Kubernetes PVC 空间压力",
	"k8s_node_pressure":          "Kubernetes 节点压力",
	"k8s_scheduling":             "Kubernetes Pod 无法调度",
	"k8s_pod_restart":            "Pod 容器重启",
	"k8s_oom_killed":             "Pod 容器近期 OOMKilled",
	"k8s_scheduling_event":       "Pod 调度或挂载失败",
	"storage_growth_pressure":    "数据增长与存储压力",
	"sql_plan_regression":        "执行计划退化",
	"trace_lock_correlated":      "请求与锁等待同窗",
	"trace_lock_pid_matched":     "请求匹配锁等待后端 PID",
}

type rcaPolicy struct {
	Code         string    `json:"code"`
	Title        string    `json:"title"`
	Version      int       `json:"version"`
	Enabled      bool      `json:"enabled"`
	MinEvidence  int       `json:"minEvidence"`
	RequireOnset bool      `json:"requireOnset"`
	EffectiveAt  time.Time `json:"effectiveAt"`
	Reason       string    `json:"reason"`
}

type RCARuleInput struct {
	ExpectedVersion int    `json:"expectedVersion"`
	Enabled         *bool  `json:"enabled"`
	MinEvidence     *int   `json:"minEvidence"`
	RequireOnset    *bool  `json:"requireOnset"`
	Reason          string `json:"reason"`
}

func migrateRCAPolicies(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS rca_rule_versions (
		code text NOT NULL, version integer NOT NULL, enabled boolean NOT NULL, min_evidence integer NOT NULL,
		require_onset boolean NOT NULL, effective_at timestamptz NOT NULL, reason text NOT NULL,
		PRIMARY KEY(code,version)
	);
	CREATE INDEX IF NOT EXISTS rca_rule_effective ON rca_rule_versions(code,effective_at DESC,version DESC)`)
	if err != nil {
		return err
	}
	codes := make([]string, 0, len(rcaRuleTitles))
	for code := range rcaRuleTitles {
		codes = append(codes, code)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO rca_rule_versions(code,version,enabled,min_evidence,require_onset,effective_at,reason)
		SELECT unnest($1::text[]),1,true,0,false,'1970-01-01'::timestamptz,'初始规则'
		ON CONFLICT(code,version) DO NOTHING`, codes); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO dbsphere_schema_migrations(version) VALUES(42) ON CONFLICT DO NOTHING`)
	return err
}

const rcaPolicyColumns = "code,version,enabled,min_evidence,require_onset,effective_at,reason"

func scanRCAPolicy(row pgx.Row) (rcaPolicy, error) {
	var p rcaPolicy
	err := row.Scan(&p.Code, &p.Version, &p.Enabled, &p.MinEvidence, &p.RequireOnset, &p.EffectiveAt, &p.Reason)
	p.Title = rcaRuleTitles[p.Code]
	return p, err
}

func (s *Postgres) rcaPoliciesAt(ctx context.Context, at *time.Time) ([]rcaPolicy, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (code) `+rcaPolicyColumns+` FROM rca_rule_versions
		WHERE effective_at<=COALESCE($1::timestamptz, statement_timestamp()) ORDER BY code,effective_at DESC,version DESC`, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []rcaPolicy{}
	for rows.Next() {
		policy, err := scanRCAPolicy(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, policy)
	}
	return result, rows.Err()
}

func (s *Postgres) RCARules(ctx context.Context, code string) (any, error) {
	if code == "" {
		rules, err := s.rcaPoliciesAt(ctx, nil)
		return map[string]any{"rules": rules}, err
	}
	if rcaRuleTitles[code] == "" {
		return nil, ErrRCARuleNotFound
	}
	rows, err := s.pool.Query(ctx, "SELECT "+rcaPolicyColumns+
		" FROM rca_rule_versions WHERE code=$1 ORDER BY version DESC LIMIT 30", code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []rcaPolicy{}
	for rows.Next() {
		policy, err := scanRCAPolicy(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, policy)
	}
	return map[string]any{"versions": versions}, rows.Err()
}

func (s *Postgres) UpdateRCARule(ctx context.Context, code string, input RCARuleInput) (any, error) {
	if rcaRuleTitles[code] == "" {
		return nil, ErrRCARuleNotFound
	}
	reason := strings.TrimSpace(input.Reason)
	if input.ExpectedVersion < 1 || input.Enabled == nil || input.MinEvidence == nil || input.RequireOnset == nil ||
		*input.MinEvidence < 0 || *input.MinEvidence > 10 || reason == "" || len(reason) > 600 ||
		strings.ContainsAny(reason, "\r\n\x00") {
		return nil, ErrInvalidRCARule
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,42))", code); err != nil {
		return nil, err
	}
	prior, err := scanRCAPolicy(tx.QueryRow(ctx, "SELECT "+rcaPolicyColumns+
		" FROM rca_rule_versions WHERE code=$1 ORDER BY version DESC LIMIT 1", code))
	if err != nil {
		return nil, err
	}
	if prior.Version != input.ExpectedVersion {
		return nil, ErrRCARuleConflict
	}
	policy, err := scanRCAPolicy(tx.QueryRow(ctx, `INSERT INTO rca_rule_versions(code,version,enabled,min_evidence,
		require_onset,effective_at,reason) VALUES($1,$2,$3,$4,$5,clock_timestamp(),$6)
		RETURNING `+rcaPolicyColumns, code, prior.Version+1, *input.Enabled, *input.MinEvidence, *input.RequireOnset, reason))
	if err != nil {
		return nil, err
	}
	return policy, tx.Commit(ctx)
}

func policiesByCode(rows []rcaPolicy) map[string]rcaPolicy {
	result := make(map[string]rcaPolicy, len(rows))
	for _, row := range rows {
		result[row.Code] = row
	}
	return result
}

type rcaEvaluation struct {
	Version   int            `json:"version"`
	Outcome   string         `json:"outcome"`
	Candidate map[string]any `json:"candidate,omitempty"`
}

func evaluateRCACandidate(candidate map[string]any, policy rcaPolicy, startedAt time.Time) rcaEvaluation {
	result := rcaEvaluation{Version: policy.Version, Outcome: "no_candidate"}
	if !policy.Enabled {
		result.Outcome = "disabled"
		return result
	}
	if candidate == nil {
		return result
	}
	references := map[string]bool{}
	supports, _ := candidate["supports"].([]string)
	for _, id := range supports {
		if id != "" {
			references[id] = true
		}
	}
	against, _ := candidate["against"].([]string)
	missing, _ := candidate["missing"].([]string)
	result.Candidate = maps.Clone(candidate)
	result.Candidate["rule"] = map[string]any{"code": policy.Code, "version": policy.Version,
		"minEvidence": policy.MinEvidence, "requireOnset": policy.RequireOnset}
	result.Candidate["coverage"] = map[string]int{"supports": len(references), "against": len(against), "missing": len(missing)}
	if len(references) < policy.MinEvidence {
		result.Outcome = "evidence_below_minimum"
		return result
	}
	first, _ := object(candidate["timeOrder"])["firstObservedAt"].(time.Time)
	if policy.RequireOnset && (first.IsZero() || first.After(startedAt)) {
		result.Outcome = "onset_not_established"
		return result
	}
	result.Outcome = "matched"
	return result
}

func selectRCACandidates(raw []map[string]any, policies map[string]rcaPolicy, at time.Time) ([]map[string]any, map[string]bool) {
	active := make([]map[string]any, 0, len(raw))
	hidden := map[string]bool{}
	for _, candidate := range raw {
		code := stringValue(candidate["code"])
		policy, found := policies[code]
		if !found {
			active = append(active, candidate)
			continue
		}
		result := evaluateRCACandidate(candidate, policy, at)
		if result.Outcome == "matched" {
			active = append(active, result.Candidate)
		} else {
			hidden["candidate:"+code] = true
		}
	}
	return active, hidden
}

func (s *Postgres) ReplayRCARule(ctx context.Context, id, code string) (any, error) {
	if rcaRuleTitles[code] == "" {
		return nil, ErrRCARuleNotFound
	}
	value, err := s.eventWithRCAPolicy(ctx, id, code)
	if value == nil && err == nil {
		return nil, ErrEventNotFound
	}
	return value, err
}
