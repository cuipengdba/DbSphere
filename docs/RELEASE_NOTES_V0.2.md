# DbSphere V0.2.0 发行说明

V0.2 将监控数据长期存储迁至独立 PostgreSQL 18，并提供 Linux Docker Compose 一键部署。[发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44020986662) 的 6 个作业已通过。目标 PostgreSQL 仍支持 14–18；PG14 已结束上游维护，页面提示升级。

## 新增能力

- PostgreSQL 深度只读诊断：会话与等待、阻塞链、长事务、WAL/归档、检查点、autovacuum、表与索引容量、复制槽；五个大版本均完成真实锁链查询。
- Linux 主机通过只读 node_exporter 获取 CPU、内存、文件系统、IO 和网络数据；可选 PgBouncer `SHOW POOLS/STATS`。未配置或无权限时明确显示数据缺口。
- Kubernetes 按 Namespace 发现 Pod、Workload、Service、Node、PVC/PV；采集容器状态/重启、Pod/Node 条件与事件、可用的 metrics-server 资源数据。PVC 实际使用量可选择从 Prometheus kubelet 指标查询。
- 资产详情中的 PostgreSQL、Linux 或 Pod/Node/PVC、PgBouncer 层级跳转；故障和恢复继续归入同一事件 ID，并可查看原始证据。当前根因规则仅覆盖“PG 连接失败且同一 Pod 近期 OOMKilled”的有限场景。
- Docker Compose 一键启动 DbSphere 和独立 PG18，生成随机凭据并持久化；支持外部 PG18 与 V0.1 SQLite 数据迁移，拒绝使用 PG17 作为长期存储。

## 验证范围

- Linux CI：单元测试、PG18 存储与 SQLite 迁移、默认及外部 PG18 Compose、PG14 目标纳管、重启与存储故障、node_exporter、PgBouncer。
- 本地：PG14–18 `pg_monitor` 账号和真实 advisory lock 阻塞链；kind 中 PG18 Pod、PVC/PV、只读 ServiceAccount、metrics-server、容器重启、探针失败与 Kubernetes 事件。
- 浏览器：登录、资产详情分层查看、Kubernetes 发现与映射、数据缺口提示。

## 已知限制

- 应用 Trace、通用告警、巡检、SQL 指纹、完整跨层根因分析和 AI 接口仍在后续版本开发计划中。**一个事件 ID 下的证据归并已存在，但尚不能宣称完整全链路根因分析。**
- Prometheus PVC 实际使用量的查询路径经模拟 Prometheus 响应验证；测试用 kind 存储插件未提供 `kubelet_volume_stats_used_bytes`，真实 Prometheus 数据源仍待验证。没有该指标时显示未采集，不推断 PVC 健康。
- 自动备份、长期样本分区与保留期、HA、生产负载性能和全面安全验收尚未完成。外部 PG18 应配置 TLS、最小权限和独立备份。

部署与迁移见 [V0.2 部署说明](DEPLOYMENT_V0.2.md)，后续工作见 [十版本任务清单](DEVELOPMENT_TASKS.md)。
