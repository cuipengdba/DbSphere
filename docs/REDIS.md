# Redis 接入

DbSphere 支持 Redis 6.2、7.2 和 8.4，使用 Go 采集单节点、主从、Sentinel 与 Cluster。Redis 的监控数据写入独立 PostgreSQL 18。

## 纳管与权限

- 从资产页选择 Redis，填写授权的监控地址。普通实例或 Cluster 节点的实例范围填 `0`。Sentinel 填要监控的主节点组名，只有一个组时可填 `0`，系统从该端点识别该组。
- 每个资产只连接所填端点。纳管 Cluster 的任意节点可获取它看到的分片和槽位，其他节点的内存、OPS 与复制细节需为相应节点添加资产。
- 使用独立 Redis ACL 用户。目标支持 TLS 时选择「验证服务器证书」。私有 CA 可配置 `DBSPHERE_REDIS_CA_FILE`。

只读 ACL 用户按下列命令权限配置。给 Redis 节点授予 `+hello +ping +info`，启用慢命令时增加 `+slowlog|get`。Redis 6.2 Cluster 节点增加 `+cluster|info +cluster|slots`，7.2 和 8.4 增加 `+cluster|info +cluster|shards`。Sentinel 账号授予 `+hello +ping +info +sentinel|master`，自动识别唯一主节点组时再授予 `+sentinel|masters`。授权口令通过部署方的凭据管理设置。

| 观测内容 | 数据来源 |
| --- | --- |
| OPS、会话、内存、拒绝连接 | INFO。OPS 为节点报告的每秒操作数，连接拒绝采用相邻采样的计数差。 |
| 命中率和淘汰 | 同一 Redis 进程的相邻采样差。进程重启、计数重置和采样间隔过长时显示缺口。 |
| 慢命令 | SLOWLOG GET，保留时间、服务端耗时和允许列表中的命令名称。 |
| RDB、AOF、复制 | INFO，按实际配置展示持久化结果、角色、连接副本、复制链路和位置。 |
| Sentinel | SENTINEL MASTER 或 MASTERS，展示监听组、当前主节点、状态、副本及其他 Sentinel 数量。 |
| Cluster | `CLUSTER INFO` 提供正常、疑似故障和故障槽位。6.2 使用 `CLUSTER SLOTS` 读取槽范围及主从节点身份，逐节点健康字段为未知。7.2 和 8.4 从 `CLUSTER SHARDS` 获取分片节点与状态。 |

从实时观测打开异常事件，可查看淘汰计数或 Sentinel 主节点的故障采样、根因证据与恢复采样。Grafana 增强包沿用 DbSphere 登录态和所选时间窗。慢命令显示服务端执行耗时。观测往返包含采集连接与 INFO 请求，适合排查采集链路。
