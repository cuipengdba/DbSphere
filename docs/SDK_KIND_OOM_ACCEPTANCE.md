# 官方 Go SDK 经 Collector 的 Kubernetes OOM 故障验收

V1.0.11 在[四节点 kind 应用故障夹具](KIND_APP_OOM_ACCEPTANCE.md)中加入官方 OpenTelemetry Go SDK 和 Collector。原 Go 示例应用与 SDK 应用同时访问受管 PG18；SDK 产生真实 HTTP 服务父 Span、PostgreSQL 客户端子 Span，并经独立 Collector 的 OTLP/HTTP JSON Exporter 进入 DbSphere。SDK 应用通过 Downward API 上报 Namespace、Pod 名及 UID，DbSphere 用自己的只读 ServiceAccount 核对身份。

故障注入仍由一次性 CI 集群的管理员 kubeconfig 执行。基线须看到 SDK 的成功父子 Span 和经 Kubernetes API 核对的应用 Pod UID。目标 PG18 Pod 实际 `OOMKilled` 后，两条应用链路都须返回 502；同一个 DbSphere 事件 ID 要同时关联两条错误 Trace、PG/K8S 故障样本、`pod_oom_correlated` 根因候选及复盘报告。恢复目标 Pod 后，两条应用请求都应重新成功，原事件状态变为 `resolved`。

这一验收不使用测试用的进程内 OTLP 转接器。生产 TLS、Collector 持久队列/负载、其他语言 SDK、自动埋点及生产 Kubernetes/Operator 仍待独立验收。实际 CI 结果将在候选作业完成后补充。
