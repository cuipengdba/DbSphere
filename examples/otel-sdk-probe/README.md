# 官方 Go OpenTelemetry SDK 接入验收程序

此独立 Go 模块使用 OpenTelemetry Go SDK 1.47.0 的 OTLP/HTTP Exporter，经[Collector JSON 转发配置](../trace-lock-app/otel-collector.yaml)测试 DbSphere 的应用 Trace 接入。它对 PostgreSQL 执行 `SELECT pg_backend_pid()`，产生一个服务父 Span 和一个数据库客户端子 Span，输出 Trace ID 与真实 PG 后端 PID。不会把 SQL 文本、参数或连接密码放入 Span。此模块不参与 DbSphere 后端构建；正式部署不需要它的依赖或 Go 运行时。

先在 DbSphere 纳管该 PostgreSQL，并启用独立 Trace 写入令牌。Collector 的 Receiver 使用可信网络，Exporter 配置 `encoding: json`、`compression: none`，将令牌通过环境 Secret 注入。SDK 程序连接 Collector 的 `/v1/traces`；`ASSET_*` 必须与 DbSphere 中受管资产的地址、端口和数据库一致。若通过本机端口转发访问 PG，`PG_URL` 可以是 `127.0.0.1`，`ASSET_HOST` 仍填 DbSphere 记录中的原始地址。

```bash
export DBSPHERE_SDK_PROBE_PG_URL='postgres://USER:PASSWORD@127.0.0.1:5432/postgres?sslmode=disable'
export DBSPHERE_SDK_PROBE_OTLP_ENDPOINT='http://127.0.0.1:4318/v1/traces'
export DBSPHERE_SDK_PROBE_ASSET_HOST='pg.example.internal'
export DBSPHERE_SDK_PROBE_ASSET_PORT='5432'
export DBSPHERE_SDK_PROBE_ASSET_DATABASE='postgres'
export DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY='1.0'
cd examples/otel-sdk-probe
go run .
```

输出 `traceId` 和 `backendPid`。登录 DbSphere 后按该资产的 `/api/assets/{assetId}/traces/{traceId}` 查询，应看到同一资产的 `service` 父 Span、`postgresql_client` 子 Span，后者的 `parentSpanId` 指向前者 `spanId`，`backendPid` 与程序输出相同。`DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY` 是可选的显式采样概率声明，合法范围为 `0 < p <= 1`；未声明时 DbSphere 不推算实际覆盖率。此验收程序使用同步 Exporter，使两个 Span 可由 Collector 分批转发，验证父先子后、子先父后及并发到达时的补链逻辑。待关联的服务 Span 只以脱敏字段暂存在独立 PG18；超过 10 分钟后不可再匹配，清理任务每 5 分钟运行。未映射资产的服务 Span 不在资产 Trace API 中显示。

V1.0.9 的故障验收还会让真实 HTTP 请求等待 PostgreSQL advisory lock，并核对应用父子 Span、等待 PID、阻塞会话、根因候选和恢复是否属于同一个 DbSphere 事件 ID。PG14–18 的 CI 使用进程内 OTLP protobuf→JSON 测试转接器；PG18 另经官方 Collector 的 `otlp_http` JSON Exporter 完整重跑该故障。转接器仅在测试中使用，正式接入仍按上述 Collector 配置。

本机 kind 验收记录和实际 Trace ID 见[验收文档](../../docs/KIND_ACCEPTANCE.md)；V1.0.9 故障验收见[专项记录](../../docs/SDK_COLLECTOR_LOCK_ACCEPTANCE.md)。此程序验证 Go SDK 的手工埋点和官方 Collector；其他语言 SDK、自动埋点、生产 TLS/负载和复杂跨服务传播仍须独立验收。SDK 用法依据[OpenTelemetry Go 官方文档](https://opentelemetry.io/docs/languages/go/instrumentation/)与[OTLP HTTP Exporter 文档](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp)。
