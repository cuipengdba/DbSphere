# PostgreSQL 连接数压力故障演练

> 2026-11-19 开发版验收。验证真实 PG18 连接占用持续超过默认规则时间窗后，告警、根因候选、证据与恢复共用一个事件；不代表已测试完全耗尽连接或生产连接池策略。

隔离 `test/replication-drill/compose.yaml` 的 `maintenance` PG18 容器将 `max_connections` 设为 30，DbSphere 仍使用独立 PG18 长期监控库。测试脚本在容器内通过 `pgbench` 建立 25 个空闲连接，不发压测 SQL；DbSphere 使用另一个只读 `pg_monitor` 账号采集。首次成功采样达到至少 85% 的默认告警阈值后，等待 125 秒，覆盖默认规则的 120 秒持续时间，再做第二次采样。测试只操作一次性夹具，不经过 DbSphere API 执行业务库修改。

首次演练的故障侧已触发容量事件，但脚本清理阶段调用了该 PostgreSQL 镜像中不存在的 `pkill`。使用镜像自带的 `pidof`/`kill` 清理连接后，补采样确认旧事件 `54f52bb4-57a5-4c52-ac80-e4b698ac1a44` 已恢复。修正脚本再次完整演练通过：故障样本 `cf49c4f7-99cc-43e3-872f-a3a339f6cdbe` 观察到 **26/30** 个连接，触发 `pg_connections_high`，并在容量事件 `581ca5bf-55f5-402d-baad-d4eeabe14400` 中形成 `connection_pressure` 候选和含样本 ID 的复盘报告。停止 25 个夹具连接后，恢复样本 `d769487a-9a73-4087-a859-2fd2368eef74` 观察到 **1/30**，在同一事件中解除告警，状态为 `resolved`。测试没有改变默认阈值或持续时间。

复现时先按[复制故障演练](REPLICATION_DRILL.md)启动夹具，再运行：

```bash
node scripts/connection-pressure-drill-acceptance.mjs http://127.0.0.1:18092 /tmp/dbsphere-replication-credentials.json
```

[Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44360581324) 的 29 个作业全部通过，其中隔离主备作业复做了真实连接压力和同事件恢复。还需完全耗尽连接、PgBouncer 排队、应用 Trace 到连接池的归因和生产并发规模验收；空闲连接只证明连接槽压力，不证明请求延迟由此导致。
