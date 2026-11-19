# 经 PgBouncer 的真实应用锁等待事件验收

> V1.0.2 首次完成同端点故障链；V1.0.5 增加业务代理端口与统计管理端口不同的全链路演练。生产身份映射仍按原规划任务 07-02 跟踪。

目标是让一个真实 HTTP 请求经 PgBouncer 进入 PostgreSQL，发生锁等待后，在 DbSphere 的**同一事件 ID**下沿应用 Span、连接池、PG 等待后端 PID、阻塞后端 PID、告警和根因候选回溯，并在解除锁后观察原事件恢复。该演练无需 AI 模型。

验收程序 `examples/trace-lock-app` 的 `/checkout` 在一个事务中读取 `pg_backend_pid()` 并执行 `pg_advisory_xact_lock()`。事务期间 PgBouncer 会保持该请求的 PG 后端身份；请求结束后导出父服务 Span 和子数据库 Span。测试使用另一条直连 PG 会话持有同一键的锁，等待真实阻塞出现后由 Go DbSphere 采样、创建事件，再释放锁。测试检查事件图的 `via_pooler`、`forwards_to`、`same_backend_pid`、`blocked_by` 关系及 `trace_lock_pid_matched` 根因候选；从同一资产打开原始 Trace，确认路由为 `pgbouncer` 且不含连接密码与 SQL 正文，复盘报告保留 PID 匹配条件。恢复采样必须关闭**原事件 ID**。

本机用真实 PgBouncer → PostgreSQL 14 和独立 PG18 存储运行 `TestApplicationLockTraceThroughPgBouncerUsesOneDbSphereEventID`，通过；原直连版本 `TestApplicationLockTraceUsesOneDbSphereEventID` 同时通过。[Linux CI 的 PG14–18 五版 PgBouncer 作业](https://github.com/cuipengdba/DbSphere/actions/runs/44484348230)均通过同一真实应用故障链演练。

独立业务地址演练 `TestApplicationLockTraceThroughSeparatePgBouncerEndpointUsesOneDbSphereEventID` 在实际 PgBouncer 前放置另一个 loopback TCP 端口，保持原管理 URL 不变，让真实 HTTP 应用通过新业务端口进入 PgBouncer。资产显式保存业务端点，应用数据库 Span 上报该端口。本机 PG14 + 独立 PG18 通过同一事件 ID 的等待 PID、阻塞 PID、`via_pooler`/`forwards_to`、`trace_lock_pid_matched`、脱敏原始 Trace、复盘报告与解除锁后的原事件恢复。五版 PgBouncer Linux CI 的同一作业已纳入此演练；这验证隔离代理端口与事件关联，不代表生产代理、高可用或 TLS 配置。

此演练覆盖受控应用中的锁等待，不代表任意生产应用已自动埋点。经官方 OTel SDK/Collector 的**池路由故障**、生产代理配置、应用 Pod/Node/PVC 的故障相关性、生产 K8S/Operator 及真实业务负载仍需单独验收。
