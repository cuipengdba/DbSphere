# 应用 Trace 经 PgBouncer 的资产映射

> V1.0.1 首次接入，V1.0.4 增加独立业务端点；原规划任务 07-02 的完整身份映射仍未完成。

应用通过 PgBouncer 连接 PostgreSQL 时，OTLP 数据库客户端 Span 的 `server.address`/`server.port` 指向连接池。旧映射只比较 PG 资产的直连地址，因此这类 Span 会被丢弃，事件页不能沿请求进入目标数据库。

资产可填写**应用访问 PgBouncer 的地址** `host:port`，与统计管理连接 URL 分开。设置后，OTLP 的 `server.address`/`server.port` 只与该业务端点匹配；即使管理连接密文不可读，显式业务端点仍可用于 Trace 归属。没有显式业务端点的旧资产，继续从已配置的管理连接 URL 解出主机和端口作为兼容路径。只配置业务端点而不配置管理连接时，应用 Trace 可映射，连接池统计标为未接入。浏览器资产页可查看、编辑或清空业务端点，管理密码不会复制到 Trace 身份或 API。

客户端 Span 必须同时声明 `db.system.name=postgresql`、业务连接池 `server.address`/`server.port` 以及与资产一致的 `db.namespace`。只有唯一资产匹配时才归属该资产。多资产共用同一个业务端点和数据库名、缺少数据库名、端点不一致时拒绝映射，不猜测实例。直连地址仍按原路径匹配，且同一资产的直连地址与池地址重合时优先标为直连。无显式端点、管理连接密文又损坏时该资产仍可按直连地址映射，池路由失效，不阻断其他资产接收 Trace。

PG18 schema 27 为已归属的 Span 存储 `direct` 或 `pgbouncer` 路由类型。事件证据图中，池路由显示“应用数据库 Span → PgBouncer → PG 资产”；采样中 PgBouncer 状态可指向同一连接池节点。原始 Trace 只展示路由类型、服务、时间、可选后端 PID 和 Query ID，不展示 SQL 正文、连接池密码或任意 Span 属性。无模型规则分析仍独立工作。

本机测试覆盖：唯一池端点归属、缺失数据库名与多资产歧义拒绝、直连优先；真实 PG18 中的业务端点存取、Span 存储、同一事件图、原始 Trace 脱敏、管理密文故障与清空显式端点后的兼容路径。真实 PgBouncer → PG14 前另加独立端口的 TCP 代理，应用查询经代理转发，Span 使用代理地址，统计管理连接仍使用原地址；此路径在本机通过。[真实 HTTP 应用锁等待同事件演练](TRACE_POOLER_INCIDENT.md)另验证同事件池路由、精确等待 PID、阻塞会话、根因候选与恢复。[PG14–18 五版旧故障链 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44484348230)已通过。独立业务端点的真实生产代理与 TLS 配置、跨集群应用、PVC 状态及生产权限矩阵仍待验收，07-02 保持未勾选。
