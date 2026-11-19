# 官方 SDK 与 Collector 锁故障验收

V1.0.9 将官方 OpenTelemetry Go SDK 的应用 Span 放进真实的 PostgreSQL 锁故障中。目标是检查**一个 DbSphere 事件 ID**从应用慢请求到数据库等待、阻塞会话、根因候选和恢复的证据，而非仅验证 Span 能写入。

验收程序在独立 PostgreSQL 18 存储中创建临时库，纳管一台测试 PostgreSQL；HTTP `/checkout` 请求生成服务父 Span 和数据库客户端子 Span，并在真实 advisory lock 上等待。DbSphere 采集时锁仍被阻塞，因而同一事件须包含精确等待 PID、阻塞 PID 和 `pg_lock_wait` 告警。解锁后 SDK 将结束的 Span 发往 OTLP，验收程序检查 `trace_lock_pid_matched` 根因候选、父子和等待 PID 图边、显式采样声明、脱敏原始 Trace、报告及事件恢复。测试结束删除临时存储库。

本机已在 PG14 目标和 PG18 存储的直连测试模式通过。此模式中的进程内转接器仅把 SDK 的 OTLP/HTTP protobuf 转成 DbSphere 要求的 OTLP/HTTP JSON。[Linux CI 验收](https://github.com/cuipengdba/DbSphere/actions/runs/44529047475)的 38 个作业全部通过：PG14–18 各运行一次直连故障演练；PG18 还在[独立作业步骤](https://github.com/cuipengdba/DbSphere/actions/runs/44529047475/job/135577222021)用官方 Collector `0.165.0` 完整重跑，`TestSDKLockIncidentUsesOneDbSphereEventID` 返回 PASS。Collector 模式不使用测试转接器。

验收命令在 `examples/otel-sdk-probe` 模块内执行：

```bash
DBSPHERE_TEST_SDK_LOCK_MODE=direct \
DBSPHERE_TEST_PG18_URL='postgres://postgres:PASSWORD@127.0.0.1:15434/postgres' \
DBSPHERE_TEST_PG_TARGET_PORT=15435 \
DBSPHERE_TEST_PG_TARGET_PASSWORD='TARGET_PASSWORD' \
go test -run '^TestSDKLockIncidentUsesOneDbSphereEventID$' -count=1 -timeout 4m -v
```

在 Linux Docker 主机将 `DBSPHERE_TEST_SDK_LOCK_MODE` 设为 `collector`，同一测试会启动官方 Collector 容器并走 `examples/trace-lock-app/otel-collector.yaml`。Collector 使用主机网络，需要空闲的 TCP 4318 端口。生产 TLS、其他 SDK/语言、自动埋点和真实业务高并发仍未覆盖；根因候选是带证据的判断，不代表已证明唯一根因。
