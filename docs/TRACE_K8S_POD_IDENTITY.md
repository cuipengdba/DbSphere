# 应用 Trace 到 Kubernetes Pod 的身份关联

> V1.0.3 建立应用 Pod 身份链；V1.0.6 加入 PVC API 状态快照。此能力只补充原规划 07-02，未完成全环境身份映射。

OTLP/HTTP JSON 的 PostgreSQL 客户端 Span 可以携带资源属性 `k8s.namespace.name`、`k8s.pod.name`、`k8s.pod.uid`。DbSphere 只接收三个完整、长度受限且格式合法的值；任意其他资源属性和 SQL 正文不写入 Trace。数据库 Span 仍须按原有的唯一端点与数据库名规则归属一个 PostgreSQL 资产。

若该资产登记为 Kubernetes 运行环境，且有明确的集群 context，DbSphere 使用配置的只读 Kubernetes 凭据，对上报的 Namespace/Pod 执行单个定向 GET。只有 API 返回的 Namespace、名称、UID 与上报值全部一致，且 Pod 创建时间不晚于 Span 结束时间，才把该身份标为已核对。UID 不一致可阻止 Pod 名称复用导致的错误关联。每批最多核对八组不同 Pod，最多四个并发请求；成功结果短时缓存，失败结果短时缓存。历史 Span 不反查当前 Pod，避免将已删除 Pod 错配到后来的同名对象。

已核对的 Span 在同一个事件 ID 证据图中连接应用 Pod，并展示该 Pod 当时 API 对象上的 Node 名称和 PVC **引用名称**。`references_pvc` 只表示 Pod spec 引用了该 PVC。V1.0.6 对最多十个引用的 PVC 逐个执行同 Namespace 的只读定向 GET，记录 API 观测时的 `Bound`、`Pending`、`Lost` 或未知阶段；证据图另有“核对 PVC 状态”关系，原始 Trace 保留状态快照。PVC 无权限、已删除、API 不可达或返回的身份不符时，状态为“未核对”，Pod UID 已核对的事实仍保留。PVC `spec.volumeName` 仅作为 PVC 属性保留，未核对 PV 实际状态、容量、挂载成功与数据库数据目录映射。API 观测时间可能晚于 Span 时间，PVC 状态不能直接证明该状态在请求当时存在，更不能单独推断根因。

Pod API 不可达、无权限、Pod 已删除、UID 不符或超出时间窗时，Trace 仍保存，事件页把应用 Pod 身份显示为未核对，不生成应用 Pod/Node/PVC 边。规则诊断与无模型监控不依赖此关联。

本机 PG18 集成测试覆盖迁移、OTLP 解析、只读 API 核对、事件图、原始 Trace 脱敏、UID 不符的链路缺口，以及同一 UID 重复上报的请求缓存。V1.0.6 增加 Bound PVC 状态在原始 Trace 和同一事件图的读回，以及 PVC GET 被拒绝时保留 Pod 身份、不编造 Bound 的回归。2026-11-19 在既有单节点 kind `dbsphere-test` 中，使用 `dbsphere-reader` 服务账号运行一次性 Linux 测试 Pod；当时的代码从集群内 Kubernetes API 核对运行中的 `dbsphere-trace-app` Pod 的 Namespace、名称、UID、Node 和创建时间，错误 UID 被拒绝。测试日志为 `TestRealKubernetesApplicationPodIdentity PASS`、Pod `Succeeded`、退出码 0。该测试未运行真实应用的完整 OTel → PG18 故障链；PG18 事件图部分由独立集成测试覆盖。V1.0.6 的 PVC 新路径仍需实际 K8S 集群复核；跨集群应用与数据库、PVC 容量/挂载状态、其他语言 SDK 属性注入、生产 Operator 与完整采样覆盖率尚未验收。
