# V0.7 应用 Trace 开发记录

> 2026-11-17。当前是开发能力，尚未达到 V0.7 发布验收。

Go 迁移进度（2026-11-18）：Go 后端已接入相同的可选 `/v1/traces` OTLP/HTTP JSON 入口，以不少于 32 字符的独立令牌认证；1 MiB/256 Span、唯一 PG 资产映射、白名单字段、PG18 去重和默认 7 天保留期均有代码及测试。PG18 实库演练从 OTLP JSON 经 Go 解析与写入，在**既有同一个事件 ID**的图中显示服务父子 Span → PG 客户端 Span → PG 资产。Node 历史实现仍保留。真实 OpenTelemetry SDK/Collector 导出兼容和完整应用故障矩阵仍待验收。

新增[Go 锁等待示例应用](../examples/trace-lock-app/README.md)：本机 PG14 实库中，实际 HTTP `/checkout` 请求在 advisory lock 后等待，应用按实际请求时间、真实 PG 等待 PID 导出父子 OTLP/HTTP JSON Span。独立测试验证请求、PID、导出契约和 SQL/密码不外泄。新增整链路测试使用**真实 Go DbSphere OTLP 接口 + 独立 PG18 监控库 + PG14–18 目标库**，从锁告警取得事件 ID，检查同一 ID 下的父子 Span、`same_backend_pid`、等待→阻塞会话和恢复关闭；本机 PG14 与[五版 CI 矩阵](https://github.com/cuipengdba/DbSphere/actions/runs/44253585868)均通过。第三方 SDK/Collector 兼容与 K8S OOM 应用场景 B 仍待验收。

Go 导入进一步按数据库客户端 Span 的父 ID 逐级回溯，只保存该 PG 资产对应的父链；同一 Trace 中的旁支服务 Span 和不相关请求不再进入该资产的事件图。重复 Span ID 视为歧义，跨其他 PG 资产的数据库父链在边界处停止。单元测试覆盖共享父链、旁支和不相关 Span；真实示例应用仍待验收。

自定义可选属性 `dbsphere.pg.backend_pid` 可由有能力取得当前 PostgreSQL 会话 PID 的应用埋点写在数据库客户端 Span 上。Go 仅保留 1 至 2147483647 的整数，并通过 PG18 schema 版本 14 存储。若同资产锁链样本的等待 PID 相同且采样时间在 Span 前后五秒内，事件根因图增加 `same_backend_pid` 连接与中置信度候选；不匹配时不推断该锁链属于请求。该属性不是 OpenTelemetry 标准 PostgreSQL 字段，也不要求普通 SDK 自动提供；PID 仍可能复用，须结合时间和 SQL/事务证据人工复核。无该属性时保持低置信度同窗分析。

本机真实 PG14 两会话 advisory lock 演练已从 Go 只读采集得到等待 PID → 阻塞 PID，测试用 OTLP Span 携带同一等待 PID，经独立 PG18 存储后在一个事件 ID 的图中出现 `same_backend_pid`；此项也加入 PG14–18 CI 矩阵。测试埋点由受控用例生成，尚未等同生产应用 SDK 自动埋点验收。

Go 同一事件页现在按服务展示**已观测** Span 数、数据库客户端调用数、错误 Span 数及时间范围，Markdown/HTML 复盘包含相同统计。单次事件最多读取 100 条 Span；达到上限会提示可能不完整。采样率和时钟偏移仍未知，因此不将计数称为全部受影响请求。发送给可选模型的证据包遮蔽服务名和明细影响面。

## 已实现的首段链路

- 可选 `POST /v1/traces` 接收 **OTLP/HTTP JSON** 的 `resourceSpans`。设置不少于 32 字符的 `DBSPHERE_OTLP_TOKEN` 后启用；未设置时接口关闭。请求使用 `Authorization: Bearer <token>`，限制 1 MiB 和 256 个 Span。
- 读取 `service.name` 及 PostgreSQL 客户端 Span 的 `db.system.name=postgresql`、`server.address`、`server.port`、`db.namespace`。只有地址、端口、数据库与唯一纳管资产匹配时才入库；跨多个 PG 资产的同一 Trace 不会把数据库 Span 误连到别的资产。
- 将 Trace ID、Span ID、父 Span ID、服务、开始/结束时间和状态写入独立监控存储。原始 SQL、参数、URL、Span 名称和任意属性均不保存。正式部署的长期存储为 PostgreSQL 18；SQLite 仅作预览。重复导出按资产与 Trace/Span ID 去重。`DBSPHERE_TRACE_RETENTION_DAYS` 默认为 7 天。
- 事件详情以**原有事件 ID**读取同资产同时间窗的 Span，展示服务 → 请求父子 Span → PostgreSQL 客户端 Span → PG 资产与既有 PG/Linux/K8S 证据图。原始脱敏 Trace 仅能经管理员登录的资产接口查看。没有 Trace 时，事件页明确显示链路缺口，数据库侧分析继续工作。

标准依据：[PostgreSQL 语义约定](https://opentelemetry.io/docs/specs/semconv/db/postgresql/)、[OTLP/HTTP JSON 与响应](https://opentelemetry.io/docs/specs/otlp/)、[OTLP Exporter 配置](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)。

## 当前边界与后续任务

- 只接收 JSON 编码的 OTLP/HTTP Trace；Protobuf、gRPC、gzip、日志与指标入口未实现。需显式把应用或 Collector 的 Trace 导出协议设置为 `http/json`，并将目标指向 DbSphere 的 `/v1/traces`。不同语言 SDK 是否支持该模式需按各自实现核验。
- 当前按精确的配置地址、端口、数据库身份匹配，不支持代理地址、动态 Service 发现、Pod/Node/PVC 的应用侧身份映射。链路时间重叠仅表示相关，不能单凭重叠证明根因。
- 采样率、时钟偏移目前只能标为未知；慢请求的 SQL 指纹到 DB Span 的精确绑定、资产级访问控制和 K8S OOM 应用场景 B 仍待开发。应用锁等待场景 A 已有本机真实 Go 应用与 DbSphere 端到端测试，尚需 CI 矩阵结果与独立环境复核。
- 默认安装不开启 Trace 接入。接入令牌是独立的写入凭据，生产环境需通过 TLS 终止的入口使用；不要把令牌放进客户端页面。

## 验证

- `test/otel.test.js`：字段脱敏、跨资产 Trace 隔离、令牌校验、重复写入、管理员可见原始数据，以及同一事件 ID 的父子 Span → PG 关系。
- `scripts/pg18-trace-smoke.mjs`：真实 PostgreSQL 18 中保存、去重、读取 Span，并在故障事件的同一事件 ID 下关联；本机及[14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44067441805)已通过。
- `scripts/pg-compat.mjs`：真实 HTTP 请求在 PG14–18 上等待一个 advisory lock；请求/DB Span 由测试程序按实际时间生成 OTLP JSON，PG 只读采集读出等待与阻塞 PID。证据图从同一事件 ID 连到应用请求、DB Span、重叠样本、等待会话和阻塞会话。本机 PG14 已通过；五版 CI 待确认。这里验证的是手工埋点，不等于所有 OpenTelemetry SDK 导出器已兼容。
