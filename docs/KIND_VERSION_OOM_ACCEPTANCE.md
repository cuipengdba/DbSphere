# PG14–18 Kubernetes OOM 故障矩阵

V1.0.12 将 PG14–17 加入四节点 kind 的真实 Pod OOM 验收，与 V1.0.11 的 [PG18 官方 SDK 故障链](SDK_KIND_OOM_ACCEPTANCE.md)合起来覆盖五个受管 PostgreSQL 大版本。五个目标数据库运行在与 DbSphere 及独立 PG18 监控库不同的 Pod；DbSphere 保持只读 `dbsphere-reader` ServiceAccount，只有一次性 CI 管理员 kubeconfig 执行故障注入。

对 PG14–17，每个版本先要求真实只读采样成功，再删除原目标 Pod，临时将官方 `postgres:<major>-bookworm` 容器主进程改为运行时内存分配程序，使用 32 MiB cgroup 限制触发 Kubernetes 明确记录的 `OOMKilled`。故障时检查 PostgreSQL 连接错误样本和 Kubernetes 容器状态保存在独立 PG18 监控库中，同一事件 ID 出现带证据的 `pod_oom_correlated` 候选。随后撤销测试命令、恢复 512 MiB 限制与原副本，要求采样成功、原事件变为 `resolved`。PG18 另要求官方 Go SDK 应用经 Collector 的错误 Trace 和恢复关联到同一事件。

这是受控的**容器级** OOM 注入，不代表 PostgreSQL 在正常业务负载中自行耗尽内存。PG14–17 夹具使用可重建的临时数据目录；PG18 故障链另保留 PVC。生产 Kubernetes/Operator、云厂商、真实应用负载与其他语言 SDK 仍需独立验收。候选根因需要人工复核。

本版 CI 的事件和样本编号将在作业通过后补充。
