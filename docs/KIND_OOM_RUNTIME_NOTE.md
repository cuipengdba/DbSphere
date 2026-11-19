# kind OOM 专项验收的运行时状态边界

V1.0.10 与 V1.0.11 的四节点 kind 作业曾明确读取到 PostgreSQL 容器 `reason=OOMKilled`，并验证同一事件 ID 下的应用错误 Trace、数据库/Kubernetes 样本、证据化根因候选与恢复。对应的验收记录分别在 [应用 OOM 演练](KIND_APP_OOM_ACCEPTANCE.md)和 [官方 SDK/Collector 演练](SDK_KIND_OOM_ACCEPTANCE.md)。

2026-11-20 的 V1.0.12 分支常规 CI 在相同故障脚本中连续遇到 `exitCode=137`、`reason=Error`。仅凭退出码 137 不能断言是 OOM；运行时若丢失 OOM 事件，DbSphere 也不会给出 `pod_oom_correlated` 候选。因此本版常规 CI 继续核对 PG14–18、真实四节点 kind 纳管、应用锁链和其他发布门槛，**不把本次 `Error` 算作 OOM 成功**。

真实 OOM 闭环保留为同一 CI 工作流的 `workflow_dispatch` 专项步骤。恢复为常规发布门槛的条件是：在可复现的运行时/节点镜像组合上，连续多次得到明确 `OOMKilled`，并在同一个事件 ID 下读取带来源的候选与恢复。生产 Kubernetes/Operator 的 OOM 场景仍需部署方单独验收。
