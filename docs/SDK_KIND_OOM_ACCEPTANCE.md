# 官方 Go SDK 经 Collector 的 Kubernetes OOM 故障验收

V1.0.11 在[四节点 kind 应用故障夹具](KIND_APP_OOM_ACCEPTANCE.md)中加入官方 OpenTelemetry Go SDK 和 Collector。原 Go 示例应用与 SDK 应用同时访问受管 PG18；SDK 产生真实 HTTP 服务父 Span、PostgreSQL 客户端子 Span，并经独立 Collector 的 OTLP/HTTP JSON Exporter 进入 DbSphere。SDK 应用通过 Downward API 上报 Namespace、Pod 名及 UID，DbSphere 用自己的只读 ServiceAccount 核对身份。

故障注入仍由一次性 CI 集群的管理员 kubeconfig 执行。基线须看到 SDK 的成功父子 Span 和经 Kubernetes API 核对的应用 Pod UID。夹具先删除原 PG18 Pod，再临时覆盖官方 `postgres:18-bookworm` 容器的启动命令，让其主进程在 32 MiB cgroup 限制内申请 128 MiB，要求 Kubernetes 实际记录 `OOMKilled`；这一步是受控的容器级故障注入，PG18 PVC 保留，恢复时撤销命令覆盖并恢复 512 MiB 限制。两条应用链路都须返回 502；同一个 DbSphere 事件 ID 要同时关联两条错误 Trace、PG/K8S 故障样本、`pod_oom_correlated` 根因候选及复盘报告。恢复目标 Pod 后，两条应用请求都应重新成功，原事件状态变为 `resolved`。

分支[四节点 CI 验收](https://github.com/cuipengdba/DbSphere/actions/runs/44551519740)已通过。该次作业的同一事件 ID 是 `85244339-a68a-4fdd-b132-3b817b7fb8df`；普通应用错误 Trace 为 `d0b26c7b7990dd39e8255548d843a1ed`，官方 SDK 错误 Trace 为 `6308da2f8c3dddace46fe5ce3c865e84`，故障样本为 `40a09a9e-4b94-4034-90b8-35a40cf10856`，恢复样本为 `d4c73529-8b50-432a-ac81-f8145cc62641`。OOM Pod `pg18-69578b5f8d-thqv6` 位于 `dbsphere-ci-worker3`，两个应用 Pod 位于 `dbsphere-ci-worker`；SDK Trace 有两条父子 Span，应用 Pod UID 经只读 Kubernetes API 核对。两条应用请求恢复成功，原事件状态为 `resolved`。

这一验收不使用测试用的进程内 OTLP 转接器。受控故障注入验证的是 PG18 **Pod 容器**的 OOM 和服务不可用，并非 PostgreSQL 在正常业务负载中自行耗尽内存。生产 TLS、Collector 持久队列/负载、其他语言 SDK、自动埋点及生产 Kubernetes/Operator 仍待独立验收。
