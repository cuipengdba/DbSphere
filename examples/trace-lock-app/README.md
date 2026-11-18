# Go 应用锁等待演练

此独立测试服务用于 V0.7/V1.0 的“HTTP 请求 → PostgreSQL 等待会话 → 阻塞会话 → 同一事件 ID”验收。它只有两个入口：`GET /healthz` 和 `GET /checkout`。`/checkout` 在目标 PG14–18 上执行一次 advisory lock 查询，并把真实请求和数据库调用的父子 Span 发送到 DbSphere 的 `/v1/traces`。应用用 Go 标准库直接编码 OTLP/HTTP JSON；这验证了 OTLP JSON 契约，**不代表第三方 OpenTelemetry SDK/Collector 的兼容性已验收**。

## 运行

先在 DbSphere 纳管目标 PostgreSQL，启用独立的 `DBSPHERE_OTLP_TOKEN`（至少 32 字符）。资产地址、端口、数据库必须与下方连接串解析出的值一致，才能唯一映射客户端 Span。DbSphere 与应用之间应使用可信 TLS 入口；本地隔离测试可用 loopback HTTP。

```bash
export DBSPHERE_DEMO_PG_URL='postgres://USER:PASSWORD@127.0.0.1:5432/postgres?sslmode=disable'
export DBSPHERE_DEMO_OTLP_ENDPOINT='http://127.0.0.1:8080/v1/traces'
export DBSPHERE_DEMO_OTLP_TOKEN='替换成与DbSphere一致的独立32位以上令牌'
go run ./examples/trace-lock-app
```

默认只监听 `127.0.0.1:18092`。可设置 `DBSPHERE_DEMO_LISTEN` 和正整数 `DBSPHERE_DEMO_LOCK_KEY`，默认锁键是 `1742026`。示例应用只用于受控测试，不应接入业务流量。

在一个保持打开的 `psql` 会话执行 `SELECT pg_advisory_lock(1742026);`，随后在另一终端请求 `curl http://127.0.0.1:18092/checkout`。请求会等待。此时在 DbSphere 对该资产采样，核对锁链包含等待 PID → 阻塞 PID，并产生/进入同一事件。回到**原 `psql` 会话**执行 `SELECT pg_advisory_unlock(1742026);`，HTTP 请求完成并导出 Span。事件图应出现服务 Span → PG 客户端 Span → 资产，以及匹配等待 PID 与原始锁链样本的边。锁告警有持续时间门槛，测试时须等规则实际触发；不能因单次锁样本就宣称告警已触发。

导出的字段只有服务名、Trace/Span 标识、时间、状态、PG 主机/端口/数据库和 `dbsphere.pg.backend_pid`。不发送 SQL 文本、参数、数据库密码或请求 URL。PID 可复用，匹配仍须满足同资产和紧邻时间窗，并由操作人员复核。

`go test ./examples/trace-lock-app` 在设置 `DBSPHERE_TEST_PG_TARGET_PORT`、`DBSPHERE_TEST_PG_TARGET_PASSWORD` 时，启动真实 HTTP 服务和临时 OTLP 接收器，对实际 PG 锁等待、请求完成、父子 Span、等待 PID 与脱敏作集成验证；未配置测试 PG 时跳过。
