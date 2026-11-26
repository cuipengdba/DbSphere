package collector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/redis/go-redis/v9"
)

var (
	ErrRedisVersion   = errors.New("Redis monitoring requires Redis 6.2, 7.2 or 8.4")
	ErrRedisConfig    = errors.New("Redis TLS configuration is invalid")
	redisVersion      = regexp.MustCompile(`^(6\.2|7\.2|8\.4)\.([0-9]{1,2})$`)
	redisNodeID       = regexp.MustCompile(`^[a-f0-9]{40}$`)
	redisTopologyName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
	errRedisGroup     = errors.New("specify a Sentinel master group")
)

type RedisSlowCommand struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	Command    string    `json:"command"`
	DurationUS int64     `json:"durationUs"`
}

type RedisSlotRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type RedisShardNode struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Health string `json:"health"`
}

type RedisShard struct {
	Slots []RedisSlotRange `json:"slots"`
	Nodes []RedisShardNode `json:"nodes"`
}

type RedisSentinel struct {
	Group    string `json:"group"`
	Master   string `json:"master"`
	RunID    string `json:"runId,omitempty"`
	State    string `json:"state"`
	Replicas *int64 `json:"replicas"`
	Peers    *int64 `json:"peers"`
	Quorum   *int64 `json:"quorum"`
}

type RedisMetrics struct {
	Version          string             `json:"version"`
	RunID            string             `json:"runId,omitempty"`
	Mode             string             `json:"mode"`
	Role             string             `json:"role"`
	LatencyMS        float64            `json:"latencyMs"`
	Ops              *int64             `json:"ops"`
	Clients          *int64             `json:"clients"`
	MaxClients       *int64             `json:"maxClients"`
	Blocked          *int64             `json:"blocked"`
	Rejected         *int64             `json:"rejected"`
	Memory           *int64             `json:"memoryBytes"`
	MaxMemory        *int64             `json:"maxMemoryBytes"`
	Evicted          *int64             `json:"evicted"`
	Hits             *int64             `json:"hits"`
	Misses           *int64             `json:"misses"`
	ConnectedReplica *int64             `json:"connectedReplica"`
	ReplicationLink  string             `json:"replicationLink,omitempty"`
	ReplicationOff   *int64             `json:"replicationOffset"`
	RDBStatus        string             `json:"rdbStatus,omitempty"`
	RDBLastSave      *int64             `json:"rdbLastSave"`
	AOFEnabled       *int64             `json:"aofEnabled"`
	AOFWriteStatus   string             `json:"aofWriteStatus,omitempty"`
	AOFRewriteStatus string             `json:"aofRewriteStatus,omitempty"`
	ClusterState     string             `json:"clusterState,omitempty"`
	SlotsOK          *int64             `json:"slotsOk"`
	SlotsFail        *int64             `json:"slotsFail"`
	SlotsPFail       *int64             `json:"slotsPfail"`
	Shards           []RedisShard       `json:"shards"`
	ShardsTruncated  bool               `json:"shardsTruncated"`
	Sentinel         *RedisSentinel     `json:"sentinel,omitempty"`
	SlowCommands     []RedisSlowCommand `json:"slowCommands"`
}

type RedisAdapter struct{}

var _ collectoradapter.Adapter = RedisAdapter{}

func (RedisAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	ctx, stop := context.WithTimeout(ctx, 11*time.Second)
	defer stop()
	opts := &redis.Options{Addr: net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Username: target.Username, Password: password, Protocol: 2, DisableIdentity: true,
		DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		PoolSize: 1, MaxActiveConns: 1, MaxRetries: -1, ContextTimeoutEnabled: true}
	tlsOn := target.SSLMode == "require"
	if tlsOn {
		opts.TLSConfig = &tls.Config{ServerName: target.Host, MinVersion: tls.VersionTLS12}
		if path := os.Getenv("DBSPHERE_REDIS_CA_FILE"); path != "" {
			data, err := os.ReadFile(path)
			pool := x509.NewCertPool()
			if err != nil || !pool.AppendCertsFromPEM(data) {
				return collectoradapter.Snapshot{}, ErrRedisConfig
			}
			opts.TLSConfig.RootCAs = pool
		}
	}
	client := redis.NewClient(opts)
	defer client.Close()
	started := time.Now()
	server, err := client.Info(ctx, "server").Result()
	latency := float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	info := redisInfo(server)
	details := RedisMetrics{Version: info["redis_version"], Mode: info["redis_mode"],
		RunID: redisEnum(info["run_id"], redisNodeID), LatencyMS: latency}
	if !redisVersion.MatchString(details.Version) {
		return collectoradapter.Snapshot{}, ErrRedisVersion
	}
	switch details.Mode {
	case "standalone", "cluster", "sentinel":
	default:
		return collectoradapter.Snapshot{}, ErrRedisVersion
	}
	major, _ := strconv.Atoi(details.Version[:1])
	minor, _ := strconv.Atoi(details.Version[2:3])
	patch, _ := strconv.Atoi(details.Version[4:])
	version := major*10000 + minor*100 + patch
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "redis", Product: "Redis OSS",
		Profile: "redis-" + details.Mode, Version: version, Signals: []collectoradapter.Signal{},
		Transport: collectoradapter.Transport{Encrypted: tlsOn, Verified: tlsOn, ReadOnly: true, Protocol: "RESP2"}}
	record := func(key, source string, err error) {
		entry := collectoradapter.Signal{Key: key, Source: source, Status: "ok"}
		switch {
		case errors.Is(err, errRedisGroup):
			entry.Status, entry.Reason = "not_configured", "请填写 Sentinel 主节点组"
		case redis.HasErrorPrefix(err, "NOPERM"):
			entry.Status, entry.Reason = "permission_denied", "监控账号缺少该命令权限"
		case err != nil:
			entry.Status, entry.Reason = "unavailable", "该命令未返回可用采样"
		}
		report.Signals = append(report.Signals, entry)
	}
	record("server", "INFO server", nil)
	if details.Mode == "sentinel" {
		details.Role = "sentinel"
		details.Sentinel, err = readRedisSentinel(ctx, client, target.Database)
		record("sentinel", "SENTINEL MASTER / MASTERS", err)
	} else {
		values, err := readRedisCounters(ctx, client, major == 6)
		if err != nil {
			record("redisCounters", "INFO clients / memory / stats / replication / persistence", err)
		} else {
			details.Ops, details.Clients, details.MaxClients = redisInt(values, "instantaneous_ops_per_sec"), redisInt(values, "connected_clients"), redisInt(values, "maxclients")
			details.Blocked, details.Rejected = redisInt(values, "blocked_clients"), redisInt(values, "rejected_connections")
			details.Memory, details.MaxMemory = redisInt(values, "used_memory"), redisInt(values, "maxmemory")
			details.Evicted, details.Hits, details.Misses = redisInt(values, "evicted_keys"), redisInt(values, "keyspace_hits"), redisInt(values, "keyspace_misses")
			details.Role = map[string]string{"master": "primary", "slave": "replica", "replica": "replica"}[values["role"]]
			details.ConnectedReplica = redisInt(values, "connected_slaves")
			details.ReplicationLink = redisChoice(values["master_link_status"], "up", "down")
			details.ReplicationOff = redisInt(values, "master_repl_offset")
			if details.Role == "replica" {
				details.ReplicationOff = redisInt(values, "slave_repl_offset")
			}
			details.RDBStatus = redisChoice(values["rdb_last_bgsave_status"], "ok", "err")
			details.RDBLastSave = redisInt(values, "rdb_last_save_time")
			details.AOFEnabled = redisInt(values, "aof_enabled")
			if details.AOFEnabled != nil && *details.AOFEnabled == 1 {
				details.AOFWriteStatus = redisChoice(values["aof_last_write_status"], "ok", "err")
				details.AOFRewriteStatus = redisChoice(values["aof_last_bgrewrite_status"], "ok", "err")
			}
			if details.Ops == nil || details.Evicted == nil || details.Role == "" {
				err = errors.New("incomplete INFO fields")
			}
			record("redisCounters", "INFO clients / memory / stats / replication / persistence", err)
		}
		slow, err := client.SlowLogGet(ctx, 20).Result()
		if err == nil {
			details.SlowCommands = []RedisSlowCommand{}
			for _, entry := range slow[:min(len(slow), 20)] {
				command := "OTHER"
				if len(entry.Args) != 0 {
					switch strings.ToUpper(entry.Args[0]) {
					case "GET", "SET", "MGET", "MSET", "HGET", "HSET", "HMGET", "HGETALL", "DEL", "UNLINK", "EXPIRE",
						"SCAN", "HSCAN", "ZSCAN", "SSCAN", "LPUSH", "RPUSH", "LPOP", "RPOP", "LRANGE", "SADD", "SMEMBERS",
						"ZADD", "ZRANGE", "XADD", "XREAD", "XREADGROUP", "EVAL", "EVALSHA", "FCALL", "SORT", "BITOP", "INCR":
						command = strings.ToUpper(entry.Args[0])
					}
				}
				details.SlowCommands = append(details.SlowCommands, RedisSlowCommand{ID: entry.ID, At: entry.Time.UTC(), Command: command, DurationUS: entry.Duration.Microseconds()})
			}
		}
		record("slowCommands", "SLOWLOG GET", err)
		if details.Mode == "cluster" {
			body, err := client.ClusterInfo(ctx).Result()
			if err == nil {
				state := redisInfo(body)
				details.ClusterState = redisChoice(state["cluster_state"], "ok", "fail")
				details.SlotsOK, details.SlotsFail, details.SlotsPFail = redisInt(state, "cluster_slots_ok"), redisInt(state, "cluster_slots_fail"), redisInt(state, "cluster_slots_pfail")
			}
			record("slots", "CLUSTER INFO", err)
			if major == 6 {
				slots, err := client.ClusterSlots(ctx).Result()
				if err == nil {
					details.Shards, details.ShardsTruncated = redisLegacyShards(slots)
				}
				record("shards", "CLUSTER SLOTS", err)
			} else {
				shards, err := client.ClusterShards(ctx).Result()
				if err == nil {
					details.Shards = []RedisShard{}
					details.ShardsTruncated = len(shards) > 64
					for _, shard := range shards[:min(len(shards), 64)] {
						item := RedisShard{Slots: []RedisSlotRange{}, Nodes: []RedisShardNode{}}
						for _, slots := range shard.Slots[:min(len(shard.Slots), 64)] {
							if slots.Start >= 0 && slots.Start <= slots.End && slots.End < 16384 {
								item.Slots = append(item.Slots, RedisSlotRange{Start: slots.Start, End: slots.End})
							}
						}
						for _, node := range shard.Nodes[:min(len(shard.Nodes), 32)] {
							if redisNodeID.MatchString(node.ID) {
								item.Nodes = append(item.Nodes, RedisShardNode{ID: node.ID,
									Role: redisChoice(node.Role, "master", "replica"), Health: redisChoice(node.Health, "online", "fail", "loading")})
							}
						}
						details.ShardsTruncated = details.ShardsTruncated || len(shard.Slots) > 64 || len(shard.Nodes) > 32
						details.Shards = append(details.Shards, item)
					}
				}
				record("shards", "CLUSTER SHARDS", err)
			}
		}
	}
	report.Role = details.Role
	return collectoradapter.Snapshot{Report: report, Metrics: map[string]any{
		"redis": details, "role": nullableCollectorRole(details.Role), "version_num": version,
	}}, nil
}

func readRedisCounters(ctx context.Context, client *redis.Client, legacy bool) (map[string]string, error) {
	sections := []string{"clients", "memory", "stats", "replication", "persistence"}
	if !legacy {
		body, err := client.Info(ctx, sections...).Result()
		return redisInfo(body), err
	}
	// Redis 6.2 accepts only one section per INFO command.
	pipe := client.Pipeline()
	commands := make([]*redis.StringCmd, 0, len(sections))
	for _, section := range sections {
		commands = append(commands, pipe.Info(ctx, section))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, command := range commands {
		for key, value := range redisInfo(command.Val()) {
			result[key] = value
		}
	}
	return result, nil
}

func redisLegacyShards(slots []redis.ClusterSlot) ([]RedisShard, bool) {
	result := make([]RedisShard, 0, min(len(slots), 64))
	truncated := len(slots) > 64
	for _, slot := range slots[:min(len(slots), 64)] {
		if slot.Start < 0 || slot.Start > slot.End || slot.End >= 16384 {
			continue
		}
		shard := RedisShard{Slots: []RedisSlotRange{{Start: int64(slot.Start), End: int64(slot.End)}}, Nodes: []RedisShardNode{}}
		truncated = truncated || len(slot.Nodes) > 32
		for i, node := range slot.Nodes[:min(len(slot.Nodes), 32)] {
			if !redisNodeID.MatchString(node.ID) {
				continue
			}
			role := "replica"
			if i == 0 {
				role = "master"
			}
			shard.Nodes = append(shard.Nodes, RedisShardNode{ID: node.ID, Role: role})
		}
		result = append(result, shard)
	}
	return result, truncated
}

func nullableCollectorRole(role string) any {
	if role == "" {
		return nil
	}
	return role
}

func redisInfo(value string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		key, val, ok := strings.Cut(strings.TrimSuffix(line, "\r"), ":")
		if ok && key != "" && key[0] != '#' {
			result[key] = val
		}
	}
	return result
}

func redisInt(fields map[string]string, key string) *int64 {
	value, err := strconv.ParseInt(fields[key], 10, 64)
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func redisChoice(raw string, choices ...string) string {
	for _, choice := range choices {
		if raw == choice {
			return choice
		}
	}
	return ""
}

func redisEnum(raw string, validate *regexp.Regexp) string {
	if validate.MatchString(raw) {
		return raw
	}
	return ""
}

func readRedisSentinel(ctx context.Context, client *redis.Client, group string) (*RedisSentinel, error) {
	var values []any
	var err error
	if group == "0" {
		masters, err := client.Do(ctx, "SENTINEL", "MASTERS").Slice()
		if err != nil {
			return nil, err
		}
		if len(masters) != 1 {
			return nil, errRedisGroup
		}
		values, _ = masters[0].([]any)
	} else {
		values, err = client.Do(ctx, "SENTINEL", "MASTER", group).Slice()
		if err != nil {
			return nil, err
		}
	}
	fields := map[string]string{}
	for i := 0; i+1 < len(values); i += 2 {
		key, _ := values[i].(string)
		fields[key], _ = values[i+1].(string)
	}
	name, host := fields["name"], fields["ip"]
	port := redisInt(fields, "port")
	if !redisTopologyName.MatchString(name) || !redisTopologyName.MatchString(host) || port == nil || *port == 0 || *port > 65535 {
		return nil, errors.New("Sentinel did not identify the monitored master")
	}
	state := "unknown"
	for _, flag := range strings.Split(fields["flags"], ",") {
		switch flag {
		case "master":
			if state == "unknown" {
				state = "ok"
			}
		case "s_down", "o_down", "disconnected", "failover_in_progress":
			state = "unavailable"
		}
	}
	return &RedisSentinel{Group: name, Master: net.JoinHostPort(host, strconv.FormatInt(*port, 10)),
		RunID: redisEnum(fields["runid"], redisNodeID), State: state, Replicas: redisInt(fields, "num-slaves"),
		Peers: redisInt(fields, "num-other-sentinels"), Quorum: redisInt(fields, "quorum")}, nil
}
