# V0.6 跨层根因分析开发记录

事件详情 API 现在用**同一个事件 ID**读取已持久化的告警/采集证据，并按证据中的 `sampleId` 查回原始 PG18 样本。服务端在请求时构造事件 → 证据 → 样本 → PG 资产、Pod/Node/PVC 或 Linux 主机的有向关系；浏览器可以展开关系与时间线。不存在或已被保留期清理的样本会列为信号缺口。图是从持久化证据派生的视图，尚未单独持久化为图数据库。

首批无模型规则只在同一事件的告警与对应样本同时满足时给出**候选**：真实 `pg_blocking_pids` 锁链、连接数达到上限的 85%、复制延迟达到 64 MiB、Linux 文件系统使用率达到 90%、PVC 使用率达到 85%。已有的 PostgreSQL 连接失败与同 Pod 最近 OOMKilled 候选沿用原有证据。每个候选列出证据 ID、缺失条件与推断边界；没有足够证据时返回“未定位”。高使用率或时间相关性不能自动证明业务故障原因，当前均不输出“已证实根因”。

管理员可以在事件页对现有候选填写理由后确认或驳回。服务端重新核对候选是否属于此事件，再把决定、理由、证据引用和时间追加到同一事件 ID 的证据表；登录与 CSRF 保护覆盖写操作。后续读取会显示最新人工决定，原有决定仍留在事件时间线。人工确认是运维判断记录，不能把规则推断自动升级为已证明的因果关系。

已通过锁链配对、证据缺失时不编造结论、未授权和无 CSRF 决定拒绝的测试；真实 PG18 长期存储的人工决定与重启后读取也通过。V0.6 任务 06-01 至 06-06 仍未完成：需补充完整锁/会话与 WAL 图、SQL 退化及 autovacuum 规则、更多 K8S 故障、反证检查和八类故障演练。V0.7 仍需 OTel 应用请求/Trace 到 DB Span 的链路。

[14 作业 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44059040861)全部通过，包括 PG18 事件存储、人工决定重启后读取、五个 PG 大版本和离线升级/回滚。真实浏览器登录 PG18 测试实例后打开已恢复的连接故障事件，看到同一事件 ID 下的 15 个证据节点、24 条关系、9 条时间线记录、Pod OOM 候选、缺失信号和持久化的人工确认理由。

后续开发已把同一事件起点附近的原始样本纳入证据图：等待分布、阻塞 PID 关系、WAL/复制、autovacuum 表统计和 SQL 指纹均链接到原始采样 API；同时间窗的慢日志、人工导入计划以精确 Query ID 关联指纹并提供原始脱敏记录 API。链接只说明同一时间窗或 Query ID 匹配，不证明因果。规则新增 WAL 接收中断/归档失败、Pod 重启、Node 压力、autovacuum 关闭与死元组积压候选，并能在已有延迟事件中提示重置安全的 SQL 退化和计划结构变化。针对 SQL 指纹、日志、计划、锁链及缺失信号的单元测试与 PG18 事件读取已通过；完整故障实验和正式 V0.6 验收仍在进行。

根因候选现在列出来自告警的实际指标值/阈值、首次证据时间与事件开始时间，并展示可用反证和缺失信号。Pod OOMKilled 的容器终止时间必须早于 PostgreSQL 连接失败且位于十分钟窗口内；未来时间戳不再产生连接故障的 OOM 候选。历史候选若缺少容器终止时间，时间顺序标记为未知。

Go 版现对**观测时间晚于事件开始超过 5 分钟**的候选信号补充时间反证，并把原本为高/中置信度的候选降为低置信度。该信号仍保留在同一事件中，供运维人员判断是否为后续加重因素；若早期采集有缺口，仍需补齐先发证据。真实 OOM 终止时间优先于较晚的告警记录时间；单元测试覆盖“事件先发生、OOM 七分钟后出现”的顺序。报告和事件页已有反证及时间顺序字段。此规则只处理明确的时间冲突，不表示其他候选已完成反证检查。

新增 Kubernetes 调度失败信号：`PodScheduled=False` 和同 Pod 近 15 分钟 `FailedScheduling`、`FailedMount` 或 `FailedAttachVolume` 事件独立标记来源；权限不足或未来时间戳保持未知/正常观测，不当作根因。真实 kind 演练使用[临时 Pod 清单](../test/fixtures/k8s-rca-pending.yaml)的不可满足 Node 选择条件，只读 ServiceAccount 采集到 `Pending`、`PodScheduled=False` 与 `FailedScheduling`。`scripts/k8s-scheduling-rca-smoke.mjs` 将该真实 K8S 证据与**注入的** PG 连接失败写入一个事件 ID，检查调度候选、事件图和 PG 告警；演练后已删除 Pod。此实验验证跨层归并与真实 K8S 信号，不等于真实业务 PG Pod 调度失败的完整故障演练。

复现时在已有的 `dbsphere-smoke` 测试 Namespace 中运行：

```bash
kubectl --kubeconfig <kind-admin-kubeconfig> apply -f test/fixtures/k8s-rca-pending.yaml
DBSPHERE_KUBECONFIG=<readonly-kubeconfig> node scripts/k8s-scheduling-rca-smoke.mjs
kubectl --kubeconfig <kind-admin-kubeconfig> -n dbsphere-smoke delete pod dbsphere-rca-pending
```

真实 PostgreSQL 锁演练也已接入 `scripts/pg-compat.mjs`：一个会话持有 advisory lock，另一个会话等待；只读监控账号读取真实 `pg_blocking_pids`，即时规则创建一个事件 ID，事件图连到等待 PID、阻塞 PID 与原始采样。PG14 本机演练通过，CI 已对 PG14–18 五个大版本运行同样步骤。

Go 版增加 `TestRealLockWaitOpensAndRecoversOneIncident`：真实 PG18 阻塞持续超过默认 60 秒后，`CheckAsset` 以同一事件 ID 写入告警、阻塞 PID 图和 `lock_chain` 候选；解除锁后再次采样，`alert_recovered` 引用新样本并关闭原事件，Markdown 复盘仍能追溯故障与恢复。本机 PG18 实测通过，Linux CI 复跑待验收。

[五版 PG 锁链 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44064849447)已全部通过，且离线安装、PG18 存储和其余作业共 14 项均为绿色。另有十类**注入式**故障矩阵测试，逐资产检查连接压力、锁、复制延迟、归档失败、autovacuum、Linux 磁盘、K8S OOM/PVC/Node/调度候选各自关联一个事件 ID。注入式矩阵用于规则回归；正式 06-06 仍需真实故障的恢复过程与影响面演练。
