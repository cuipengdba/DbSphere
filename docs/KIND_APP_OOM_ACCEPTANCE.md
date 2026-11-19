# 四节点 kind 应用连接失败与 PostgreSQL OOM 演练

V1.0.10 将场景 B 加入独立 Linux CI。故障夹具在四节点 kind 中部署 PostgreSQL 18 长期存储、PG14–18 监控目标、Go DbSphere 和独立的 Go HTTP 应用。应用与 PG18 目标位于不同 worker；DbSphere 使用只读 `dbsphere-reader` ServiceAccount。故障注入由 CI 的临时管理员 kubeconfig 执行，应用和 DbSphere 均没有删除 Pod 或修改 Deployment 的权限。

演练先发起健康 `/checkout` 请求，核对 PG18 资产可采集且服务父 Span、PostgreSQL 客户端子 Span 已入独立 PG18 存储。随后将目标 Deployment 缩到零副本，待旧 Pod 删除后以 16 MiB 限制启动新 Pod，直至 Kubernetes 报告容器真实 `OOMKilled`。此时应用请求应返回 502 和错误 Trace；DbSphere 的数据库连接失败样本须同时保存 K8S OOM 状态。验收脚本检查**同一个事件 ID**包含 `pod_oom_correlated` 候选、应用父子错误 Span、脱敏 Trace 链接和复盘报告。

最后恢复原 512 MiB 内存限制和一个副本，核对原事件变为 `resolved`、新采样成功、应用请求重新返回 200。PG18 目标的 PVC 在 Pod 替换期间保留。故障注入发生在一次性 CI 集群；生产 Kubernetes/Operator、其他 PG 版本的 OOM 组合、真实业务应用和自动 SDK 埋点仍需独立验收。

入口：[CI 四节点 kind 作业](https://github.com/cuipengdba/DbSphere/actions/runs/44535966928/job/135601328984)与[可复现验收脚本](../scripts/kind-matrix-app-oom.mjs)。[V1.0.10 候选 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44535966928)的 38 个作业全部通过；故障步骤输出如下：

| 证据 | 实测值 |
| --- | --- |
| 同一事件 ID | `6984d2a0-f672-4bd7-8c57-4dbd7d35a4eb`，最终 `resolved` |
| 应用错误 Trace | `dcf194805d01fe5856b7a3c2a130a450`，服务父 Span 与 PG 客户端子 Span 均为错误 |
| PG/K8S 故障样本 | `af92946e-effa-4d20-b6e0-9f8e42010ba4`，实际 OOM Pod 位于 `dbsphere-ci-worker3` |
| 同事件恢复样本 | `351b848b-b6aa-430d-b014-3079385cfcce`，应用再次返回 200 |
| 根因候选 | `pod_oom_correlated`，报告保留事件和错误 Trace 引用 |

该结果证明一次性四节点 kind 中的真实故障链闭环，不代表生产 Operator、其他 PG 大版本或外部应用的 OOM 组合已验收。
