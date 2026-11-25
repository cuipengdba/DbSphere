# etcd 和 Kubernetes 控制面观测

DbSphere 支持 etcd 3.5 和 3.6。每个资产配置一个已授权的观测端点，可分别纳管各成员。原生采集与 Grafana 使用同一批数据，历史写入独立 PostgreSQL 18。

## 接入

- 从资产页选择 etcd，填写地址和端口。带有成员和 Raft 详情的客户端端点通常使用 2379。配置独立指标端口时可采集其 `/metrics` 和 `/health`，成员与 Raft 依实际端点的读取权限显示。
- TLS 模式默认验证服务端证书。通过 `DBSPHERE_ETCD_CA_FILE` 配置私有 CA。需要双向 TLS 时，同时配置 `DBSPHERE_ETCD_CERT_FILE` 和 `DBSPHERE_ETCD_KEY_FILE`。由部署环境提供只读证书文件，并将这些文件挂载到容器或 systemd 可读的路径。
- 观测端点要求认证时可填写令牌，请求会使用该值作为 `Authorization`。令牌仅随 HTTPS 请求发送。对于通过代理授权的端点，填入该代理接受的完整授权值。
- 可填现有 kubeconfig 中的 Kubernetes Context，或在集群内使用已有 ServiceAccount。采集 API Server 指标所需的额外授权是 `nonResourceURLs: ["/metrics"]` 的 `get`。页面所显示的关联以配置的 Context 为依据。

| 信号 | 来源 |
| --- | --- |
| 健康状态 | 成员端点 `/health` |
| 成员、Leader、Raft | `/v3/cluster/member/list`、`/v3/maintenance/status` 和 `/metrics` |
| 选主计数、提案、存储 | 原生 `/metrics`，保留成员身份和采样时间 |
| WAL fsync 和后端提交耗时 | 相同成员、相同进程的相邻直方图增量，显示窗口均值和可观测的 P95 桶上界 |
| API Server 请求 | Kubernetes `/metrics` 的 etcd 请求次数、错误次数和请求耗时 |
| 证书到期 | TLS 握手验证通过的服务端证书和本地加载的客户端证书 |

Leader 中断或切换、成员报告健康失败、观测端点不可达、磁盘延迟分别依据原生观测生成事件。事件保持相同 ID，可回查故障与恢复采样。在当前采样窗口至少有三次测量时，将 WAL 平均耗时达到 25 ms 或后端提交平均耗时达到 50 ms 标记为延迟事件。直方图窗口的缺口保留为空。
