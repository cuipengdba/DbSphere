# V0.7 应用 Trace 开发记录

> 2026-11-19。当前是开发能力，尚未达到 V0.7 发布验收。

V0.6.0 正式版之后的 Go 开发增量：数据库客户端 Span 可选择携带 `dbsphere.pg.query_id`（PostgreSQL 有符号 64 位 Query ID）。Go 只保留规范化整数，不保存 SQL 文本；PG18 schema 25 迁移保存该字段。事件页在同一资产、请求前后五分钟的已引用 `pg_stat_statements` 样本中，只有该 Query ID 唯一对应一个 SQL 指纹时，才把数据库 Span 连到 SQL 指纹节点。不同角色或数据库 OID 产生的多个指纹视为歧义，不建立连线。原始脱敏 Trace API 显示 Query ID。本机 PG18 实库验证了旧库升级、Span 存储及一个事件 ID 下的精确连线；此属性需要应用能够可靠取得，普通 SDK 不会自动产生，连线只表示同一语句身份，不证明单次请求造成性能退化。

同一 Trace 中已观测到跨服务父子 Span 时，事件证据图增加服务间 `calls_service` 关系；重复调用只显示一条服务关系，父 Span 缺失时不推断依赖。这反映了已上报的调用拓扑，不能推断未采样服务或故障传播方向。事件页面同时修正了字符串型根因匹配条件显示为 `undefined` 的问题。

SQL 退化事件的服务影响面现在另计“同指纹慢调用”：必须同时有告警中的 Query ID 与指纹、事前平均执行时长、同期唯一样本指纹，以及应用数据库 Span 至少比事前平均多 50 ms；随后沿已观测父 Span 关联请求。正常调用、不同 Query ID 或歧义指纹只算同期 Trace。这是定向排查线索，不表示该次请求已经证明由计划变化导致。

本机 PG18 实库再做一次 SQL 退化演练：真实 `pg_stat_statements` 采样与真实慢查询打开事件 `c9cea449-c75d-4da4-8946-8f873bda9025`，受控埋点的同 Query ID 数据库 Span 在事件图里连到唯一指纹和上游服务，服务影响面计入一条同指纹慢调用；恢复样本 `4db6494e-6c36-4439-9a1b-6f2e8a15fad5` 关闭原事件。Trace Span 由测试程序根据真实调用起止时间写入，尚不代表生产应用自动埋点验收。

Go 迁移进度（2026-11-18）：Go 后端已接入相同的可选 `/v1/traces` OTLP/HTTP JSON 入口，以不少于 32 字符的独立令牌认证；1 MiB/256 Span、唯一 PG 资产映射、白名单字段、PG18 去重和默认 7 天保留期均有代码及测试。PG18 实库演练从 OTLP JSON 经 Go 解析与写入，在**既有同一个事件 ID**的图中显示服务父子 Span → PG 客户端 Span → PG 资产。Node 历史实现仍保留。官方 OpenTelemetry Collector 0.165.0 与 Go SDK 1.47.0 的分批 Trace 转发已在本机 kind 验证；其他 SDK 与完整应用故障矩阵仍待验收。

新增[Go 锁等待示例应用](../examples/trace-lock-app/README.md)：本机 PG14 实库中，实际 HTTP `/checkout` 请求在 advisory lock 后等待，应用按实际请求时间、真实 PG 等待 PID 导出父子 OTLP/HTTP JSON Span。独立测试验证请求、PID、导出契约和 SQL/密码不外泄。新增整链路测试使用**真实 Go DbSphere OTLP 接口 + 独立 PG18 监控库 + PG14–18 目标库**，从锁告警取得事件 ID，检查同一 ID 下的父子 Span、`same_backend_pid`、等待→阻塞会话和恢复关闭；本机 PG14 与[五版 CI 矩阵](https://github.com/cuipengdba/DbSphere/actions/runs/44255593304)均通过。场景 B 在本机 kind 用真实 Go 应用请求连接实际 OOMKilled 的 PG18 Pod，错误 Span、PG 连接失败和 K8S OOM 候选进入同一事件 ID，恢复后原事件关闭；详见[本机 kind 验收](KIND_ACCEPTANCE.md)。官方 Collector 与 Go SDK 的 Trace 转发已实测；生产 K8S 与独立环境复核仍待验收。

SDK 兼容验收使用独立[Go SDK 探针](../examples/otel-sdk-probe/README.md)执行真实 PG18 查询，OTLP HTTP/Protobuf 发至官方 Collector，再由 Collector 转成 JSON 发往 DbSphere。实测发现父/子 Span 可能分批且并发到达；Go 在独立 PG18 暂存只含 Trace/Span ID、服务、时间、状态的待关联服务 Span，10 分钟后不再匹配，并每 5 分钟清理。跨批次按相同 Trace ID、父 Span ID 与时间关系补链，未找到受管 PG 子链的服务 Span 不显示在资产页。PG18 实库测试覆盖两种到达顺序和并发提交后的补链；本机 kind 的真实 SDK Trace `5cab080d463f1b775b5c7e7c11c40fd5` 已读回父子 2 Span 与实际后端 PID `6713`。跨服务自动传播、其他语言 SDK 和负载/丢包率仍待验收。

Go 导入进一步按数据库客户端 Span 的父 ID 逐级回溯，只保存该 PG 资产对应的父链；同一 Trace 中的旁支服务 Span 和不相关请求不再进入该资产的事件图。重复 Span ID 视为歧义，跨其他 PG 资产的数据库父链在边界处停止。单元测试覆盖共享父链、旁支和不相关 Span；真实示例应用仍待验收。

自定义可选属性 `dbsphere.pg.backend_pid` 可由有能力取得当前 PostgreSQL 会话 PID 的应用埋点写在数据库客户端 Span 上。Go 仅保留 1 至 2147483647 的整数，并通过 PG18 schema 版本 14 存储。若同资产锁链样本的等待 PID 相同且采样时间在 Span 前后五秒内，事件根因图增加 `same_backend_pid` 连接与中置信度候选；不匹配时不推断该锁链属于请求。该属性不是 OpenTelemetry 标准 PostgreSQL 字段，也不要求普通 SDK 自动提供；PID 仍可能复用，须结合时间和 SQL/事务证据人工复核。无该属性时保持低置信度同窗分析。

本机真实 PG14 两会话 advisory lock 演练已从 Go 只读采集得到等待 PID → 阻塞 PID，测试用 OTLP Span 携带同一等待 PID，经独立 PG18 存储后在一个事件 ID 的图中出现 `same_backend_pid`；此项也加入 PG14–18 CI 矩阵。测试埋点由受控用例生成，尚未等同生产应用 SDK 自动埋点验收。

同一事件若同时含有多个慢数据库 Span，根因候选现在先查找与 PG 锁样本在五秒内精确匹配的等待 PID，再考虑没有 PID 的低置信度同窗线索，避免较新的无关请求遮住更强的证据。精确候选复用影响面分类的相同 PID 匹配条件，并在事件页和 Markdown/HTML 复盘列出调用耗时、匹配 PID 与原始样本 ID。单元测试覆盖弱线索先出现、精确线索后出现；真实 PG14 锁等待整链路测试核对报告中的匹配条件。PID 复用和业务锁原因仍需人工复核。

Go 同一事件页现在按服务展示**已观测** Span 数、数据库客户端调用数、错误 Span 数及时间范围，并把同期 Trace 与“故障信号关联”Trace 分开统计。后者仅包括数据库客户端错误，或后端 PID 与同一事件 PG 锁等待样本在五秒窗口内精确匹配的 Trace；信号沿同一 Trace 的父 Span 关联到应用请求，提供可点击的脱敏原始 Trace 链接。正常请求即使落入事件时间窗也只计入同期观测，不算故障关联。Markdown/HTML 复盘包含相同区分。单次事件最多读取 100 条 Span，关联 Trace 链接每服务最多展示 10 条；达到 Span 上限时提示计数可能不完整。采样率和时钟偏移仍未知，PID 可能复用，故障关联仍是证据而非已证明因果，不将计数称为全部受影响请求。发送给可选模型的证据包遮蔽服务名和明细影响面。

## 已实现的首段链路

- 可选 `POST /v1/traces` 接收 **OTLP/HTTP JSON** 的 `resourceSpans`。设置不少于 32 字符的 `DBSPHERE_OTLP_TOKEN` 后启用；未设置时接口关闭。请求使用 `Authorization: Bearer <token>`，限制 1 MiB 和 256 个 Span。
- 读取 `service.name` 及 PostgreSQL 客户端 Span 的 `db.system.name=postgresql`、`server.address`、`server.port`、`db.namespace`。只有地址、端口、数据库与唯一纳管资产匹配时才入库；跨多个 PG 资产的同一 Trace 不会把数据库 Span 误连到别的资产。
- 将 Trace ID、Span ID、父 Span ID、服务、开始/结束时间和状态写入独立监控存储。原始 SQL、参数、URL、Span 名称和任意属性均不保存。正式部署的长期存储为 PostgreSQL 18；SQLite 仅作预览。重复导出按资产与 Trace/Span ID 去重。`DBSPHERE_TRACE_RETENTION_DAYS` 默认为 7 天。
- 事件详情以**原有事件 ID**读取同资产同时间窗的 Span，展示服务 → 请求父子 Span → PostgreSQL 客户端 Span → PG 资产与既有 PG/Linux/K8S 证据图。原始脱敏 Trace 仅能经管理员登录的资产接口查看。没有 Trace 时，事件页明确显示链路缺口，数据库侧分析继续工作。

标准依据：[PostgreSQL 语义约定](https://opentelemetry.io/docs/specs/semconv/db/postgresql/)、[OTLP/HTTP JSON 与响应](https://opentelemetry.io/docs/specs/otlp/)、[OTLP Exporter 配置](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)。

## 当前边界与后续任务

- 只接收 JSON 编码的 OTLP/HTTP Trace；Protobuf、gRPC、gzip、日志与指标入口未实现。应用可向 Collector 标准 Receiver 发送，再由其 `otlp_http` Exporter 用 `encoding: json`、`compression: none` 指向 DbSphere 的 `/v1/traces`；[已验证配置](../examples/trace-lock-app/otel-collector.yaml)提供独立写入令牌。不同语言 SDK 的自动埋点和接入方式仍需按各自实现核验。
- 当前按精确的配置地址、端口、数据库身份匹配，不支持代理地址、动态 Service 发现、Pod/Node/PVC 的应用侧身份映射。待关联服务 Span 仅在接入后 10 分钟内参与补链，时钟偏移超过 5 秒可能阻止父子关联。链路时间重叠仅表示相关，不能单凭重叠证明根因。
- 采样率、时钟偏移目前只能标为未知；自定义 Query ID 提供有条件的 SQL 指纹精确连线，但应用自动获取 Query ID、代理和多身份映射仍待开发。资产级访问控制仍待完善。应用锁等待场景 A 已有本机与五版 CI 端到端测试；K8S OOM 场景 B 已有单节点 kind 实测，两者仍需独立环境及生产级复核。
- 默认安装不开启 Trace 接入。接入令牌是独立的写入凭据，生产环境需通过 TLS 终止的入口使用；不要把令牌放进客户端页面。

## 验证

- `test/otel.test.js`：字段脱敏、跨资产 Trace 隔离、令牌校验、重复写入、管理员可见原始数据，以及同一事件 ID 的父子 Span → PG 关系。
- `scripts/pg18-trace-smoke.mjs`：真实 PostgreSQL 18 中保存、去重、读取 Span，并在故障事件的同一事件 ID 下关联；本机及[14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44067441805)已通过。
- `scripts/pg-compat.mjs`：真实 HTTP 请求在 PG14–18 上等待一个 advisory lock；请求/DB Span 由测试程序按实际时间生成 OTLP JSON，PG 只读采集读出等待与阻塞 PID。证据图从同一事件 ID 连到应用请求、DB Span、重叠样本、等待会话和阻塞会话。本机 PG14 已通过；五版 CI 待确认。这里验证的是手工埋点，不等于所有 OpenTelemetry SDK 导出器已兼容。
