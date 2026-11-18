# 本机 Kubernetes 验收记录（开发版）

> 2026-11-18，单节点 kind `dbsphere-test`，Namespace `dbsphere-smoke`，PostgreSQL 18 测试数据库。此记录只证明列出的本机故障路径，不是生产 Kubernetes 或 V1.0 正式版验收。

## 环境和边界

- Go 后端与静态前端运行在 Linux 容器中，通过本机 `127.0.0.1:18090` 端口转发访问；采集目标和长期数据存储分别是两个 PG18 Deployment。测试存储用 `emptyDir`，Pod 被替换会丢失数据，不能据此声称持久化验收。
- Go Pod 使用 `dbsphere-reader` ServiceAccount 的 Pod 内投射凭据；它可只读获取目标 Namespace 的 Pod、Event、Service、PVC、PV、Node 及 Pod/Node 资源指标。实际 impersonation 检查 `get secrets`、`delete pods` 均返回 `no`。故障注入由测试集群管理员进行，不授予 DbSphere 采集器写入集群的权限。
- 已从正常目标采到 PG18、Pod、Service、PVC/PV、Node 和 metrics-server 的 Pod/Node CPU/内存。Kubernetes 资源证据与 PostgreSQL 采样写入独立 PG18 存储。

| 故障注入和实际观测 | 同一事件 ID | 根因候选与恢复证据 |
| --- | --- | --- |
| PG18 探针持续失败：Service 无就绪端点；PG 连接失败，Pod `Ready=False`，出现 `Unhealthy` Event。 | `37821d0d-9309-4582-a37d-90314c49abef` | `k8s_pod_not_ready` 低置信度；修复探针并采到成功样本 `6b60df49-f5a0-4cb5-8bf6-3ba6b8dbaf3e` 后，原事件关闭，人工恢复验证和管理审计关联该事件。 |
| PG18 Pod 内存限制 16 MiB：容器实际 `OOMKilled`，PG 连接失败，K8S 终止状态与 OOM 告警进入同一事件。 | `58d989b1-ecaa-4e65-acb6-ff50447fd791` | `k8s_oom_killed` 中置信度；内存限制调回 256 MiB 后，成功样本 `fefc1085-8c94-414e-9fae-811458a9b379` 关闭原事件。 |
| PG18 Pod 指向不存在的 Node 标签：实际 `Pending`、`PodScheduled=False/Unschedulable` 和 `FailedScheduling` Event；Service 无端点，PG 连接失败。 | `109fcab1-f7c1-456b-aa13-9899a5b23c2f` | `k8s_scheduling_event`、`k8s_scheduling` 均为低置信度；移除错误 Node 选择器后，成功样本 `3b396529-04b1-4f3b-aa42-de4955391dd9` 关闭原事件，已删除的旧 Pod 名自动解析到新 Pod。 |

## 判定

三种故障均在**一个问题、一个事件 ID**下记录数据库故障、K8S 状态、规则告警和恢复采样。根因输出是带证据与不确定性的候选，不把关联当作已证实的唯一原因。浏览器已查看探针故障的事件时间线、候选与审计；调度故障的 API 证据及同 ID 恢复已核对。

尚未验收多节点和生产集群、PVC 压力等其余故障、真实应用 Trace、持续负载、持久存储故障及完整 PG14–18 组合。V1.0 的八类故障和两类应用链路门槛仍未完成；见[开发任务清单](DEVELOPMENT_TASKS.md)。
