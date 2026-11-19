# 应用 Trace 经 PgBouncer 的资产映射

> V1.0.1 补丁范围；对应原规划任务 07-02 中更广的身份映射仍未完成。

应用通过 PgBouncer 连接 PostgreSQL 时，OTLP 数据库客户端 Span 的 `server.address`/`server.port` 指向连接池。旧映射只比较 PG 资产的直连地址，因此这类 Span 会被丢弃，事件页不能沿请求进入目标数据库。

现在仅在资产已配置 PgBouncer 管理连接串时，从加密连接串解出**主机和端口**用于服务端内部映射；密码不会复制到 Trace 身份或 API。客户端 Span 必须同时声明 `db.system.name=postgresql`、连接池 `server.address`/`server.port` 以及与资产一致的 `db.namespace`。只有唯一资产匹配时才归属该资产。多资产共用同一个连接池端点和数据库名、缺少数据库名、端点不一致时拒绝映射，不猜测实例。直连地址仍按原路径匹配，且同一资产的直连地址与池地址重合时优先标为直连。单个资产的连接池密文损坏时该资产仍可按直连地址映射，池路由失效，不阻断其他资产接收 Trace。

PG18 schema 27 为已归属的 Span 存储 `direct` 或 `pgbouncer` 路由类型。事件证据图中，池路由显示“应用数据库 Span → PgBouncer → PG 资产”；采样中 PgBouncer 状态可指向同一连接池节点。原始 Trace 只展示路由类型、服务、时间、可选后端 PID 和 Query ID，不展示 SQL 正文、连接池密码或任意 Span 属性。无模型规则分析仍独立工作。

本机测试覆盖：唯一池端点归属、缺失数据库名与多资产歧义拒绝、直连优先；真实 PG18 中的加密连接串读取、Span 存储、同一事件图及原始 Trace 脱敏。另用本机真实 PgBouncer → PG14 执行应用查询，并将该实际连接池端点的客户端 Span 归入唯一 PG18 资产，网络与映射联动测试通过。完整 Go 实库回归通过。真实应用经 PgBouncer 的**故障**闭环、连接池代理地址与管理地址不同的部署、Pod/Node/PVC 应用身份映射仍需验收，07-02 保持未勾选。
