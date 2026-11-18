# 开发状态与验证记录

> 更新于 2026-11-18。此文只记录已实际实现或测试的内容；版本发布以 GitHub Release 为准。

## 当前阶段

Go 应用场景 B 已在本机 kind 实测：真实 HTTP 请求在测试 PG18 Pod `OOMKilled` 后返回 502，错误服务/PG Span 经 Go OTLP 接口存入独立 PG18；PG 连接失败、K8S OOM 和中置信度根因候选汇入事件 `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7`。恢复内存限制后，自动跟随新 Pod 的成功样本 `89e858aa-7593-4ee0-afb1-c2ee87ef9301` 关闭该事件；应用再次请求成功。该演练仅证明单节点 kind 的一条应用故障链，故障前健康 Trace 也落入事件时间窗，影响面需按错误 Span 核对。完整证据及局限见[本机 kind 验收](KIND_ACCEPTANCE.md)。

Go 锁等待示例应用已提供可运行的 HTTP `/checkout` 和直接 OTLP/HTTP JSON 导出。本机 PG14 整链路测试使用真实应用请求、Go DbSphere OTLP 接口和独立 PG18 监控库：请求被 advisory lock 阻塞，数据库采样与锁告警创建一个事件 ID，释放后应用 Span 带实际等待 PID；同一事件图出现服务→PG Span、`same_backend_pid`、等待→阻塞会话，恢复采样关闭原事件。[PG14–18 五版 CI 矩阵](https://github.com/cuipengdba/DbSphere/actions/runs/44255593304)也通过该测试。官方 OpenTelemetry Collector 0.165.0 已在本机 kind 把同一应用的真实 Trace 以 JSON 转发到 Go 接口，PG18 存储中读回服务/PG 父子 Span 及真实后端 PID；第三方 SDK 与生产级场景仍待验收，详见[示例应用](../examples/trace-lock-app/README.md)与[Trace 开发记录](TRACE_V0.7_PROGRESS.md)。

Go 事件页把同一次 OOM 故障的历史 `pod_oom_correlated` 证据与新 `k8s.oom_recent` 告警合并成一条候选，保留已有人工确认/驳回所用的候选代码，并把采集失败、K8S 状态、告警和原始样本全部连到候选；单独出现 OOM 告警时仍生成 `k8s_oom_killed` 候选。乱序证据单元测试、PG18 实库测试和 Go 全量测试通过。本机 kind 更新镜像后短暂发生 API/etcd 超时，恢复后直接复核旧 OOM 事件 `58d989b1-ecaa-4e65-acb6-ff50447fd791`：同一已恢复事件只显示一条 `pod_oom_correlated` 候选，包含 5 条支撑证据和告警匹配条件；见[本机 kind 验收记录](KIND_ACCEPTANCE.md)。

Go K8S 资产现在可只填单 Pod 的 Deployment/StatefulSet Workload；若已配置 Pod 被重建并返回 404，也会按 Workload 标签及 owner UID 只读寻找唯一就绪的新 Pod。多候选、权限缺口和超过 1000 项或 10 页的列表不猜测实例，样本标出原 Pod 和解析 Pod。本机真实 kind 将资产故意改回已删除的旧 Pod 名，再次采样仍正确解析到新 Pod、取得 PG18 和资源指标；Pod 留空的 Workload-only 路径也通过。TLS 模拟 API 验证多就绪 Pod 必须报告歧义，并在 Workload、ReplicaSet 和 Pod 均跨页时只读解析第二页的唯一目标；重复 `continue` 令牌会拒绝映射。多副本主备映射和实际大集群容量验收仍是 V1.0 缺口。

上述 Workload 映射还在真实 OOM 故障中验证了唯一**未就绪**替换 Pod：资产保留旧 Pod 名，Go 自动解析到 `CrashLoopBackOff`/`OOMKilled` 新 Pod，采集失败样本与恢复样本共用事件 `4b72b565-ebcb-4203-9f03-5069bebc93fa`；恢复时再次自动解析健康 Pod。TLS 模拟 API 回归测试新增唯一未就绪 Pod 场景。多 Pod 候选继续报歧义，不推断主备角色。

本机 kind 第三类真实故障演练：测试 PG18 Deployment 指定不存在的 Node 标签后，Pod 一直 `Pending`，真实 `PodScheduled=False/Unschedulable` 与 `FailedScheduling` Event 被 Go 只读采集；Service 无端点导致 PG18 连接失败。三层证据进入事件 `109fcab1-f7c1-456b-aa13-9899a5b23c2f`，并生成有来源的低置信度调度候选。移除错误 Node 选择器后，新 Pod 就绪，采样 `3b396529-04b1-4f3b-aa42-de4955391dd9` 成功连接 PG18；已删除的旧 Pod 映射自动跟随新 Pod，原事件恢复为 `resolved`。本机三类故障演练的环境、证据和局限见[本机 kind 验收记录](KIND_ACCEPTANCE.md)。

本机 kind 第二类真实故障演练：测试 PG18 Pod 内存限制为 16 MiB 后出现实际 `OOMKilled`，Go 把 PG 连接失败、K8S 终止状态和 `k8s.oom_recent` 告警归到事件 `58d989b1-ecaa-4e65-acb6-ff50447fd791`，显示带原始来源的中置信度 OOM 候选。将限制调回 256 MiB、更新新 Pod 映射后，成功样本 `fefc1085-8c94-414e-9fae-811458a9b379` 关闭原事件 ID。该演练只覆盖本机单节点 kind 的一种受控 OOM 故障，不代表生产内存策略或其它故障类别已验收。

Go 版在本机 kind 单节点测试集群完成一次真实 K8S/PG18 故障闭环：Go 静态镜像以现有只读 ServiceAccount 在 Pod 内运行，另有独立 PG18 监控库；先从既有 PG18 Pod 成功采到数据库、Pod、Service、PVC/PV、Node 和 metrics-server Pod/Node CPU/内存。ServiceAccount 的 `get secrets` 与 `delete pods` 均为 `no`。随后创建探针始终失败的测试 PG18 Pod，Service 无就绪端点，Go 把 PG 连接失败、Pod `Ready=False`、`Unhealthy` 事件与 Service→Pod→Node 关系归入事件 `37821d0d-9309-4582-a37d-90314c49abef`；持续时间达到规则要求后出现有来源的低置信度 `k8s_pod_not_ready` 候选。修复探针并更新映射后，成功 PG18/K8S 样本 `6b60df49-f5a0-4cb5-8bf6-3ba6b8dbaf3e` 关闭**同一事件 ID**；人工恢复验证引用该样本，操作审计也关联该 ID。浏览器查看了候选、时间线与审计，并修复了“容器运行中”掩盖 `Ready=False` 的文案。此演练为本机 kind 测试，不代表多节点、生产 K8S、真实应用 Trace 或八类故障矩阵通过；旧外部只读 kubeconfig 仍已过期，本次使用 Pod 自动挂载的 ServiceAccount 凭据。

Go 离线包加入 `--backup /absolute/path`：在目标 Linux 机上备份随包部署的独立 PG18，并输出 SHA-256 清单；外部 PG18 由其自身备份系统负责。本机临时 PG18 归档与清单生成成功，[20 项 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44243312616) 已从离线包执行备份、在另一 PG18 容器恢复并核对 schema 版本；这不是生产恢复演练。

Go 管理员 API 写入审计已接入 PG18 schema 版本 16/17：已认证且通过 CSRF 的 POST/PUT/PATCH/DELETE 在执行业务逻辑前写入操作尝试；记录失败时拒绝运行请求。完成后更新 HTTP 状态，管理员可在系统状态页看最近 100 条；事件处理路径自动关联原事件 ID，并在事件页折叠显示。查看账号不能读取。只存管理员角色、方法、路径和可解析的事件 ID，不存请求正文、查询参数、Cookie 或凭据。本机 PG18、HTTP 权限、浏览器角色隔离、真实 kind 事件操作和 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44242609766) 均通过。当前只有共享管理员身份，没有具体操作人、审批或审计防篡改能力。

Go 事件分析新增应用服务的已观测影响面：在原事件 ID 的 Trace 时间窗内按服务汇总 Span、PostgreSQL 客户端 Span、错误 Span 与首末时间，工作台折叠显示，复盘报告列出统计。达到 100 条查询上限会提示可能不完整；采样率未知，数字不代表全部受影响请求。PG18 实库验证父子 Span 的汇总与 AI 证据包对服务名的遮蔽；生产追踪采样策略仍待验收。

Go 可选 AI 新增默认关闭的严重事件自动摘要。显式设置 `DBSPHERE_AI_AUTO_SUMMARY=true` 且配置模型后，后台每分钟扫描最近 24 小时的严重开放事件，同一事件只请求一次，沿用 PG18 每日预算；经证据 ID 校验的限长建议保存为同事件 `ai_advisory`，页面与复盘报告均标记待人工核验。模型失败只留审计，不改变规则告警；这不是提前预测故障。PG18 逻辑与模拟模型工作器测试通过，真实生产模型和预警效果仍待验收。

K8S Pod `OOMKilled` 根因候选现在可由同一事件中的 `k8s.oom_recent` 告警独立触发，不再依赖同时出现的重启增量告警。候选仍要求同资产样本确认容器终止原因和十分钟内的时间关系；非 OOM 终止不会生成该候选。单元测试覆盖独立 OOM 告警与反例，真实集群故障演练仍待补齐。

Go 可选 AI 的手动事件分析新增 PG18 每 UTC 日预算：默认 20 次，可配置 1–1000；请求前在事务锁下写入 `pending` 审计记录，成功或失败后更新同一记录，重启和多 Go 实例共享上限。超额返回 429，原有确定性监控和根因候选保持可用。PG18 schema 版本 15 新增按时间检索调用审计的索引；本机 PG18 和 HTTP 拒绝路径测试已通过，真实外部模型及自动预警仍待验收。

Go OTLP 数据库客户端 Span 可选携带 DbSphere 自定义属性 `dbsphere.pg.backend_pid`。仅接受正整数 PID，并写入 PG18 的版本 14 迁移列；事件页仅在同资产、Span 前后五秒内的锁链样本中找到相同等待 PID 时，给出中置信度候选及 Span → 原始样本直接边。PID 不匹配或时间偏离不提升结论；无 PID 时保留原有低置信度同窗候选。本机真实 PG14 advisory lock 等待 PID、OTLP 埋点、PG18 样本与同事件 ID 候选已通过；五版 CI 和真实示例应用仍待验收。

Go OTLP 关联现在只将唯一匹配 PG 资产的数据库客户端 Span 及其真实父 Span 链保存到该资产。同一 Trace 的无关服务旁支不会进入事件根因图；重复 Span ID、跨另一数据库资产的父链会在边界处停止。单元测试覆盖两个数据库资产共享父 Span、独立旁支和不相关服务 Span；还需真实示例应用的端到端验收。

Go 控制台新增可选的全局只读查看账号。配置 `DBSPHERE_VIEWER_PASSWORD` 后将 scrypt 哈希存入 PG18，登录会话带签名角色；查看账号可打开资产、事件、报告与监控数据，所有受浏览器会话保护的写 API 在服务端返回 403，页面隐藏写入表单和按钮。重启时更换该环境变量可轮换密码，移除可停用账号，旧查看会话随哈希变化失效。真实 PG18 与浏览器验证了只读登录、可查看同一事件及写入入口隐藏；API 和 PG18 测试覆盖管理员仍可写、查看账号带 CSRF 也不能写、密码轮换和停用。当前尚无多用户、资产范围或审批流，不能将这段能力标为完整 RBAC。

Go 事件页现可在原事件 ID 下记录人工诊断、处置和恢复验证。恢复验证要求事件已结束，并引用同资产在结束后采集的成功 PG18 样本；失败样本、开放事件或错误资产不能用于验证。Markdown/HTML 报告列出处理记录与采样 ID，AI 摘要遮蔽人工备注。实库与 HTTP 鉴权测试通过，浏览器完成处置填写和时间线验收；完整工单审批与生产恢复演练仍待开发。

Go 离线安装新增 Linux CI 升级/回滚演练脚本：从固定的前一 Go 开发基线离线安装，创建故障资产、采样和事件，再用当前 Go 包升级并回退；三个阶段核对同一资产/样本/事件 ID 与管理员配置哈希。脚本通过 Bash 语法检查和 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44232205949)；该演练不能代替未来 schema 变更和生产数据恢复验证。

Go 资产页现可直接导入 PostgreSQL csvlog/jsonlog 片段与已有 `EXPLAIN (FORMAT JSON)` 计划。日志按资产和原始记录哈希去重，只存时间、耗时、Query ID 与格式；计划只存节点类型和数值，并比较相同 Query ID 的结构与执行时间。PG18 实库、HTTP 鉴权/CSRF/大小上限测试通过；真实浏览器上传两种文件后，在同一资产页显示脱敏记录，并链接到原有事件 ID。仍需 PG14–18 的真实日志样例矩阵和生产使用验收。

同一事件的 SQL 根因候选现要求三段证据同时存在：事件前 30 天内同 Query ID 计划基线、事件时间窗内的新计划，以及与新计划相差不超过五分钟且耗时至少 1000 ms 的同 Query ID 慢日志。计划形状变化或已有执行时间至少翻倍时，候选以低置信度出现，证据图连接基线、新计划与慢日志，人工结论写回原事件 ID。移去慢日志后候选不再生成；这只是排查假设，仍需应用请求与 Query ID 的直接关联和真实故障演练。

应用到数据库链路新增低置信度候选：同一资产的应用父 Span、耗时至少 500 ms 的 PostgreSQL 客户端 Span，以及相差一分钟内的真实锁链样本同时存在时，在同一事件 ID 下给出父子 Span 和原始样本链接。缺父 Span、缺锁链或时间不重叠时不生成；PG18/PG14 实库事件与边界测试通过。OTel Span 未记录 PostgreSQL 后端 PID，候选明确说明仍需人工核对具体 SQL/会话。

Go 资产页已从独立 PG18 的每日容量样本计算中位数增长与 30 天估计，样本不足或无权限时不显示虚假的预测。本机 PG18 实库和边界测试通过；这只是数据库大小趋势，不代表磁盘剩余空间或备份可恢复性。

Go 可选 AI 模型接口已接入兼容 Chat Completions 的本机/HTTPS 端点，默认关闭，仅在管理员手动点击事件分析时发送脱敏摘要；模型引用必须是同一事件的真实证据 ID。PG18 同事件审计与本机模拟模型测试覆盖成功、超时、虚构引用和未授权请求；真实浏览器验收了建议展示与同事件 ID 的 `ai_invocation` 时间线。尚未完成真实模型、AI 预警与预算验收。

Go 问题事件现支持按同一事件 ID 导出 Markdown/HTML 复盘报告，包含状态、证据时间线、根因候选、人工作出的结论和信号缺口。PG18 实库测试验证事件/样本 ID 与凭据不外泄，HTTP 测试验证未登录不能下载，浏览器完成事件页下载。报告使用有上限的证据集；工单、完整处置和恢复验证仍未迁移。

Go Linux `node_exporter` 适配器现解析 CPU、内存、文件系统、磁盘和网络计数；读取限制为 5 MiB、4 秒且不跟随重定向。本机真实 PG14 + node_exporter + PG18 演练验证：PG 连接失败时主机指标仍写入同一事件 ID 的样本和证据图。Go 调度现可包含已配置 exporter 的 Linux PG 资产，K8S 和 PgBouncer 仍待迁移；Linux CI 联合演练待本轮运行。

Go SQL 性能采集与 Top SQL 接口已接入：检测 `pg_stat_statements` 扩展和列兼容性，只保存 Query ID、指纹与聚合计数，不读取 SQL 正文；无扩展时显式显示 `not_configured`。资产页按 PG18 历史样本计算相邻采样增量和双窗口退化提示。PG18 真扩展与 PG18 三样本退化用例本地通过，PG14–18 扩展矩阵已加入 CI，结果待本轮验证。

Go PG14/PG18 本机实库只读诊断已覆盖 14 个分区：会话/等待、长事务、锁数量与 PID 锁链、数据库 I/O、WAL、归档、检查点、vacuum、表与索引大小、复制槽、冻结年龄、槽明细和关键参数。真实 advisory lock 等待演练确认 Go 采集的等待 PID → 阻塞 PID 关系；每个诊断区独立标记 `ok` 或 `unavailable`，不会把单个权限/版本缺口伪装成健康。PG15–17 及完整 CI 尚待本轮流水线。

Go 自动采集调度已在本机真实 PG18 开发库演练：15 秒间隔自动发现新 Linux PG 资产，连接失败写入样本与同一事件 ID；API 查询看到了 1 条样本和 1 个开放事件。调度仅选用 Go 已具备完整适配器的纯 PostgreSQL 资产，K8S 和 PgBouncer 资产不会被误采。隔离的 Go Compose 新库默认启用调度；连接历史/外部 PG18 时默认关闭，需显式设置 `DBSPHERE_ENABLE_GO_COLLECTOR=true`。尚未通过本轮 CI。

Go 独立部署开发路径已通过 [21 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44088758026)：`Dockerfile-go` 将后端静态编译并嵌入前端，Compose 默认启用独立 PG18，Go `.run` 包在删除本机镜像后从包内导入应用与 PG18，再次运行复用配置和数据卷。Go 单容器本地测试验证健康检查、页面与首次管理员登录。该路径仍是开发版；功能对等、跨层故障、升级/回滚未验收。

Go 同一事件证据图新增 PG18 慢日志、导入的执行计划与 SQL 指纹 Query ID 关联，原始脱敏记录可从图中打开。PG18 实库测试覆盖这两类链接，仍需本轮 CI 验证；SQL 趋势计算和自动慢日志采集尚未迁移。

Go 事件分析继续迁移：基于 PG18 既有事件和采样，证据图现可串起应用服务/父子 Span、PostgreSQL 锁链与 SQL 指纹、Linux 主机、K8S Pod/Node/PVC；两个有证据的初始根因规则是连接数压力和锁阻塞，已有 Pod OOMKilled 关联证据也能作为候选显示。候选附证据来源、时间顺序、限制和缺失信号，支持管理员确认/驳回并写回同一个事件 ID。PG18 实库测试覆盖事件闭环、证据图和人工复核；[20 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44084839406)已通过前一批 Go 事件/资产详情与 PG14–18 矩阵。新规则仍待本轮 CI 和真实跨层故障验收，不代表完整根因分析已发布。

2026-11-17 新增 Go 后端与静态前端强制架构，以及分层后台页面。仓库已建立 Go 入口、嵌入静态资源、配置校验、PG18 schema 迁移、健康接口和管理员登录；Node 格式的密码哈希、凭据密文及 Session/CSRF 已用兼容向量验证。十个工作台只读接口已从 PG18 返回真实数据。Go 资产增删改与基础只读 PG 采集已通过实库测试，连接失败与恢复使用同一事件 ID；事件详情可读该 ID 的证据、原始样本和同窗 Trace，资产详情可读真实历史数据。Go 浏览器已完成登录、总览进入事件及资产详情的冒烟检查，控制台无错误；跨层适配器尚未迁移的资产不会被 Go 误判为健康。空 PG18 实库建表和月分区、Linux amd64 静态编译及 Go 测试通过。**Go 深度采集、完整根因候选与 SQL/容量分析仍在迁移，现有完整监控功能仍由 Node 历史实现提供**。界面从超长单页拆成六视图，总览和事件双栏在桌面与 390px 窄屏真实浏览器可见，未接入信号明确显示缺口。完整清单见[Go 与工作台迁移](GO_UI_MIGRATION.md)。

V0.7 Trace 首段能力正在开发：可选令牌保护的 OTLP/HTTP JSON 接口接收 PostgreSQL 客户端 Span，将服务及父子 Span 与唯一匹配的 PG 资产关联，并在原事件 ID 的证据图展示；敏感 SQL/参数/URL 不入库。真实 PG18 持久化冒烟及[14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44067441805)已通过。PG14–18 的应用锁场景 A 已通过 CI，本机 kind 的应用 OOM 场景 B 已实测；标准 SDK 协议兼容、精确映射、采样率、时钟偏移及生产影响面仍待完成，详见[开发记录](TRACE_V0.7_PROGRESS.md)。V0.6 正式版也尚未通过全部故障演练，因此目前不可宣称 V0.7 已发布。

**[V0.1.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.1.0)、[V0.2.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.2.0)、[V0.3.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.3.0)、[V0.4.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.4.0)、[V0.5.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.5.0)，V0.6 开发中**。已实现 B/S 服务端与浏览器页面、管理员登录和 CSRF 校验、PostgreSQL 资产增删改查、凭据加密、基础采集、采集失败与恢复共用一个事件 ID 的证据记录。PG14–18 已能读取会话/等待、长事务、锁链、WAL/归档、检查点、autovacuum、表/索引容量和复制槽 11 个诊断分区，并显示不可用原因。V0.2 新增独立 PG18 后端、SQLite 数据迁移、Docker Compose 一键部署、Linux node_exporter 资源采集；K8S 适配器支持按 Namespace 发现并关联 Pod/Workload/Service/Node/PVC/PV，读取容器重启/OOMKilled、事件与可用的 metrics.k8s.io 数据。当 PG 连接失败与近期 Pod OOMKilled 同时出现时，在**同一个事件 ID** 中给出中等置信度、可查看原始证据的根因候选。

V0.1 功能验收样例覆盖 Linux 原生 Node 进程及 kind 中的 K8S PostgreSQL。V0.2 的 PG18 存储、默认及外部 PG18 一键 Compose、Linux 主机采集和 PgBouncer 适配均通过 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/43722385125)。K8S Namespace 资源发现和 Pod → Workload/Service/Node/PVC/PV 关联已通过真实 kind 只读演练；Pod/Node 使用量也通过实际 metrics-server 验证。真实 kind 的探针失败 Pod 已由只读采集器读取 `Ready=False` 与 `Unhealthy` 事件，采集后已清理测试 Pod。PVC 实际用量可选从 Prometheus kubelet 指标读取，该路径目前只通过模拟响应测试。V0.1 SQLite 仅用于早期样例。应用 Trace 及通用跨层根因引擎属后续任务。当前只支持“采集连接失败 + 近期 Pod OOMKilled”的有限候选，**不能将其宣称为完整的全链路根因分析**。

## V0.6 开发中

Go 告警迁移已加入 PG 连接使用率、锁等待、长事务和 Linux 内存/磁盘五条默认规则。采样事务写入规则状态与触发、信号缺失、恢复、静默状态变化证据；同一资产同一问题组的 PG 与 Linux 告警共用事件 ID，全部恢复后关闭。本机 PG18 测试验证规则修改持久化、连接压力根因候选及同 ID 恢复；Go API 的规则编辑受管理员登录和 CSRF 保护。旧版未移植规则在 Go 页面只读，通知投递、K8S/PgBouncer 规则和完整巡检写入仍待迁移。本段是开发进度，尚非正式版验收。

Go K8S 只读采集第一段已覆盖指定 Namespace/Pod 的容器状态、OOMKilled、Pod 条件与事件、Node 条件及 PVC 绑定信息；TLS 模拟 API 验证了 bearer token、Pod UID 事件过滤和只读请求。PG18 实库合成样本验证了 PG 连接失败 + 10 分钟内同 Pod OOM 的一个事件 ID、带来源的中等置信度候选和恢复闭环。Go Compose/离线包增加可选的只读 kubeconfig 挂载。旧外部只读 kubeconfig 已过期，临时 token 签发曾被自动审批拒绝；随后改用 Pod 自动挂载的现有只读 ServiceAccount，在本机 kind 完成探针故障、Service/PVC/Node 与恢复闭环。生产集群和真实 OOM 故障仍未验收。

Go K8S PVC 详情现继续以只读请求读取绑定的 PV 名称、阶段和容量；权限缺失时保留 `volumeError`。B/S 资产页按既有 PVC/PV 结构显示，不再把 PV 名称字符串当作对象。metrics.k8s.io 暂不可用时在样本中明确标记缺口。

Go K8S 采集现可选读取 `metrics.k8s.io/v1beta1` 的指定 Pod 容器和 Node CPU/内存；TLS 模拟 API 验证了有指标时显示来源与时间，以及 403 时保留 Pod 健康状态并标记资源指标缺失。本机 kind 起初因只读 RBAC 缺少 metrics `get` 返回 403，补充最小只读权限后，Go Pod 采集到真实 Pod/Node 资源指标。

Go 只读采集现按 Pod 标签关联同 Namespace 中的 Service，并在 B/S 资产页及同一事件 ID 的证据图添加 Service → Pod 关系；列表限制 100 条，权限缺失或截断显式标记。TLS 模拟 API、PG18 实库和本机 kind 探针故障事件图均验证了该关系；生产多节点仍待验收。

Go 每次资产采样现并行读取 PG、Linux/K8S 和 PgBouncer，K8S API 的总时间预算为 12 秒；后台采集和 HTTP 响应上限相应调整为 45 秒，避免多个可选适配器的超时直接累加。PG18/PG14/PgBouncer 本机联合采样和全量 Go 测试通过，生产规模采集调度仍待负载验收。

Go 可选从 Prometheus `kubelet_volume_stats_used_bytes` 按 Namespace/PVC 读取实际使用字节，并与 Kubernetes PVC 容量组合触发使用率规则。查询 URL、Bearer 令牌、响应大小和超时均受限制；模拟 Prometheus API 已验证真实值、空向量与未配置状态，未配置/缺信号不触发“健康”结论。Go Compose 可选传入 Prometheus URL/令牌；生产 Prometheus 与 PVC 高水位事件仍待验收。

Go K8S 告警新增 Pod 未就绪、调度失败、近期 OOM、Node 压力和可选 PVC 使用率规则；Pod UID 事件中近期 FailedScheduling/FailedMount/FailedAttachVolume 也可触发。PG18 实库测试确认数据库恢复时，只要同事件的 OOM 告警仍在触发，事件保持开放；两侧都恢复后才在原事件 ID 关闭。缺失 K8S 信号不当作健康，PVC 用量的生产指标、真实容器重启和通知通道仍待验收。

Go 告警静默/维护窗已接入创建与撤销 API，窗口最长 30 天，按资产和规则限定；PG18 实库验证锁等待告警的抑制和解除抑制证据均写入同一事件 ID。Go 通知工作器已接入 PG18 持久化队列、Webhook/SMTP、严重度路由、失败退避和 B/S 重试；PG18 实库验证最多五次失败后的手动重试仍使用原投递 ID，本地 TLS Webhook 签名与环回 SMTP 已通过模拟收件端。未确认严重告警超过 15 分钟仅在同一事件 ID 升级一次；PG18 实库已验证未到期、静默和解除静默的边界。生产通知通道仍待验收。

Go RCA 在 PG 连接失败的样本上继续读取 K8S/Linux/PgBouncer 的有效告警证据；新增 Pod 未就绪/OOM/调度、Node/PVC 压力、Linux 内存/磁盘和连接池等待候选。候选仍标为假设，附原始样本、告警证据、时间顺序和未验证条件。PG18 实库验证 PG 连接失败 + K8S OOM 同一事件 ID 下同时保留关联候选和 K8S 告警候选；其余候选经合成证据测试。复制/WAL、SQL 性能根因和真实 K8S 故障矩阵仍待开发验收。

Go 规则包继续增加复制延迟、备库 WAL 接收中断、autovacuum 关闭、表死元组积压和 Linux CPU，共十七条默认规则；对应 RCA 候选附上复制/WAL 与表维护的原始采样证据及不确定性。本机 PG14 只读实库验证复制状态查询，五版矩阵需以本轮 CI 确认；真实复制拓扑和 autovacuum 故障演练仍待完成。

Go 新增 WAL 归档失败计数和同一 Pod 容器重启的相邻采样增量规则，共十九条。首次样本、计数回退或 Pod UID 变更保持未知；PG18 实库验证第二次采样触发、后续零增量在原事件 ID 关闭。RCA 对应候选只把时间相关性当作线索。真实归档错误与 Pod 重启故障演练仍待完成。

Go 默认规则扩至二十四条，与 Node 历史规则包的指标范围对齐：增加 PG 不可连接、PG 采集失败、K8S/Linux 采集失败和调度周期内采样陈旧。PG 凭据/权限问题不推断为实例不可用；未配置的 K8S/Linux 指标保持未知。陈旧告警在采集前评估，采集写入后在同一事件 ID 恢复。PG18 实库验证 PG 连接故障、陈旧状态及恢复的事件归并；规则范围对齐不代表 Go 全功能或生产验收完成。

事件页改为分层事件卡片，并持久展示当前选中项；Go + PG18 实库的浏览器流程验证了桌面、390px 手机、事件详情与列表刷新后选中状态。旧版慢日志测试曾把 SHA-256 哈希中碰巧出现的短字符串误认为敏感参数，现改为检查完整敏感片段，避免随机误报。

Go PgBouncer 适配器已从加密管理员 URL 用简单查询协议只读读取 `SHOW POOLS/STATS`，真实本机 PgBouncer 1.26.1 与 PG14/独立 PG18 联合采样通过，新增的 [PgBouncer CI 作业](https://github.com/cuipengdba/DbSphere/actions/runs/44106034937)已通过。客户端等待规则可触发事件；未映射池或连接错误不会被视作健康。

PG18 实库进一步验证了 PostgreSQL 锁等待与 PgBouncer 客户端等待在同一资产的 `latency` 问题组中只开一个事件 ID，两条规则都恢复后在该 ID 关闭。

[PgBouncer 新增作业](https://github.com/cuipengdba/DbSphere/actions/runs/44106034937)通过，但同一轮 Go foundation 因不同 Go 测试包并行共用 PG18 库、重复启动执行建表语句而遇到一次死锁，故该轮整体 CI **失败**。Go schema 迁移现于版本 13 已应用时跳过建表和已存在分区的 DDL，共享库的 CI 测试包改为串行执行；本地连续五次并发启动/事件读取与完整 Go 测试通过，[修复后的 21 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44107307127)全部通过。

Linux x86_64 自解压一键离线包已实现：联网构建机将预构建应用镜像与 PG18 镜像、Compose 文件及校验清单打入 `.run`；目标机在已有 Docker Engine/Compose 前提下，一条命令校验、导入镜像并以 `--no-build --pull never` 启动 B/S 和独立 PG18。[13 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44056141672)验证删除本机镜像后的纯本地安装与重复执行。另有 Node.js 24 + 外部 PG18 的轻量离线包，CI 验证解压后不运行包管理器即可启动。两种产物均由[独立工作流](https://github.com/cuipengdba/DbSphere/actions/runs/44056371453)构建并校验；单文件静态服务端尚未实现。跨层根因分析仍按 V0.6 任务推进。详见[离线部署说明](OFFLINE_DEPLOYMENT.md)。

[V0.6 开发预览](https://github.com/cuipengdba/DbSphere/releases/tag/v0.6.0-dev)已附上 `.run`、Node 轻量包及各自 SHA-256 文件；该预览不代表 V0.6 正式版验收。事件详情现从已存证据和原始样本派生同一事件 ID 的证据关系与首批无模型根因候选；证据不足显示“未定位”。本地 51 项测试和 PG18 实库事件读取通过。范围及未完成项见[根因分析开发记录](RCA_V0.6_PROGRESS.md)。

[14 作业 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44058038127)还验证了真实 V0.5 应用镜像 → 当前开发版 → V0.5 应用的离线安装与应用回滚，三阶段复用同一 PG18 数据卷并读回同一资产。未来 schema 变更的回滚和正式版故障恢复仍需额外演练。

同一事件的根因候选新增人工确认/驳回，需填写依据并经过登录、CSRF 与候选归属校验；决定作为新证据写入事件时间线。SQLite API 测试与真实 PG18 存储重启后读取已通过。现阶段仍是有限规则候选与人工判断记录，不能宣称已经完成全链路根因分析。

[最新 14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44059040861)全部通过。真实浏览器也已打开 PG18 存储中的已恢复事件，验证证据关系、Pod OOM 候选、缺失说明和人工确认理由在 B/S 页面显示。

V0.6 后续开发将 SQL 指纹、慢日志、计划、锁链、等待、WAL/复制及 autovacuum 信号纳入同一事件的时间窗证据图，所有新节点提供原始数据入口；新增相应候选规则。当前为代码与本地 PG18 验证阶段，尚未完成完整故障演练或 V0.6 正式发布。

本机 kind 的临时 Pending Pod 已由只读采集器读取 `PodScheduled=False` 与真实 `FailedScheduling` 事件，并与注入的 PG 连接失败归入同一事件 ID；演练 Pod 已删除。该混合演练不替代真实业务 PG 调度故障与恢复测试。

PG14–18 的真实锁等待 → 只读锁链采集 → 同一事件 ID 的候选和 PID 证据图已通过 [14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44064849447)。十类注入式跨层故障矩阵通过本地测试；真实故障恢复矩阵仍是 V0.6 发布前缺口。

V0.7 开发中的真实 HTTP 请求 → PostgreSQL 锁等待 → 只读锁链样本 → 同一事件 ID 证据图已在本机 PG14 验证；请求和 DB Span 由测试程序按实际起止时间生成 OTLP JSON。PG14–18 CI 与真实 OpenTelemetry SDK 导出仍待验收。

## V0.5 已发布能力

PG14–18 真实容器的 pg_stat_statements 聚合采集已通过；Top SQL 只保留指纹与计数，显示采样增量、退化门槛、Linux/K8S 同期状态及已有事件入口。管理员可导入 PostgreSQL CSV/JSON 慢日志和已有 EXPLAIN (FORMAT JSON) 计划，原始 SQL、参数、过滤表达式和对象名不入库。SQLite → PG18 演练已覆盖巡检、慢日志和计划记录，真实 PG18 查询延迟演练产生退化提示。真实 PG18 浏览器验收已覆盖 Top SQL、慢日志和计划导入；[发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44053161066) 11 个作业全部通过，含五版 SQL 采集与无扩展降级矩阵。V0.5.0 已发布。详见 [SQL 性能开发记录](SQL_PERFORMANCE_V0.5.md)。

## V0.4 已发布能力

PG14–18 的只读采集新增冻结年龄、复制槽及关键参数；浏览器可管理四类版本化巡检模板、计划、重试和历史差异。备份适配器只读读取 pgBackRest 状态文件；未配置或缺少尝试记录时明确显示未知。PG18 容量粗估和 Markdown/HTML 报告关联资产、样本、事件 ID。一键 Compose 内置 PG18 备份脚本、清单只读挂载和独立 PG18 还原演练已通过 [Linux 六作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44047383649)。本地测试 42 项通过，浏览器接入真实 PG18 存储和模拟 pgBackRest 状态，验证了备份过期及最近失败风险。真实 pgBackRest 服务导出、生产定期备份和规模恢复时间仍需环境验收。

## V0.3 已发布能力

受控规则表达式、阈值、持续时间、严重度、启停与规则版本已写入 SQLite 预览和正式 PG18 存储；浏览器可编辑规则并查看当前告警。PG、Linux、K8S、PVC、PgBouncer 信号都保留来源和缺失状态。采样事务在同一事件 ID 中归并 PG 连接故障与同一 Pod 的 OOM，并让独立的 PVC 风险保留另一事件；触发、确认、恢复和信号缺失写入证据时间线。采集器超过两个计划采样周期未写入样本时产生独立的陈旧数据告警；K8S API 与 node_exporter 采集错误也单独报警，缺少配置保持未知。管理员可经 B/S 页面创建最长 30 天、限定资产和规则的静默或维护窗，并撤销；静默期间事件和证据继续记录，触发、抑制、解除抑制、恢复均在同一事件 ID 下。按通道级别路由的 Webhook/SMTP 邮件、持久化队列、重试和未确认严重事件的一次性升级已实现。PG18 采样与事件表均按 UTC 月份分区，支持已有普通表搬迁、趋势聚合、存储占用及可选保留期；未结事件引用的样本、未结事件及待投递通知不清理。生产规模升级/备份和完整故障演练仍待开发。

本地 `pnpm test` 通过 33 项，包含真实本机 SMTP 会话与管理员手动重试接口。PG18 实库验证规则修改持久化、事件归并、事件确认、静默抑制/撤销、通知队列和一次性升级，以及 SQLite 规则/状态/静默/通知迁移。Webhook HMAC 和单通道失败重试通过注入式传输测试；生产 SMTP 服务仍未验收。真实 PG14 + node_exporter + PG18 联合采样通过。浏览器验收覆盖登录、规则编辑、PG 不可达告警、事件证据、维护窗创建/撤销、同一事件内的抑制/解除抑制证据、未配置通道状态及 PG18 容量。V0.3 的 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44036215638) 六个作业已通过。

## 已通过的验证

| 验证 | 结果 | 范围 |
| --- | --- | --- |
| [V0.6 离线升级/回滚 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44058038127) | 通过，14 个作业 | 真实 V0.5 应用 → V0.6 开发版 → V0.5 应用，PG18 资产持久；其余离线包、PG14–18、PG18 存储、Compose 等回归 |
| [V0.6 开发 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44056141672) | 通过，13 个作业 | 自解压 `.run` 在删除镜像后导入应用与 PG18 并重复启动；Node 轻量包解压后直接启动；其余 PG14–18、PG18 存储、Compose、Linux 与 PgBouncer 回归 |
| [V0.5 发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44053161066) | 通过，11 个作业；49 项自动化测试 | PG14–18 `pg_stat_statements` 已配置/未配置矩阵、PG18 真实 SQL 退化、计划解析、慢日志脱敏、PG18 迁移、Compose、Linux、PgBouncer 与依赖审计 |
| [V0.4 发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44047383649) | 通过，6 个作业；42 项自动化测试 | PG18 巡检持久化、真实复制槽 WAL 风险、监控存储备份恢复、一键 Compose 清单挂载与接口、PG14/node_exporter、PgBouncer、Linux 启动及依赖审计 |
| `pnpm test` | 通过，33 项 | 登录、CSRF、资产与凭据、事件归并、告警/静默/通知、Webhook HMAC、本机 SMTP 会话、Linux/K8S/PgBouncer 采集及数据缺口 |
| [V0.3 发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44036215638) | 通过，6 个作业 | PG18 规则/事件/通知/分区/保留期及迁移、单元测试、依赖审计、Linux 进程、Compose 和 PG14/node_exporter |
| [V0.2 发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44020986662) | 通过，6 个作业 | Linux 原生启动、依赖审计、PG18 存储与 SQLite 迁移、PG17 拒绝、默认/外部 PG18 一键 Compose、PG14 目标、K8S 配置挂载、错误路径拒绝、存储故障与更新、node_exporter、PgBouncer |
| [V0.2 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/43722385125) | 通过 | PG18 资产/样本/事件存储、SQLite 迁移、默认和外部 PG18 一键 Compose、Linux PG14 + node_exporter、PgBouncer、重启持久化、存储断连与恢复 |
| [GitHub Actions Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/43687609396) | 通过 | Node 24 安装、自动化测试与依赖审计 |
| K8S 适配器单元测试 | 通过 | 按 Pod UID 限定事件，读取容器 OOMKilled、PVC/PV、Node 条件、Service 与 metrics.k8s.io；发现权限缺失与 API 鉴权 |
| Linux 容器构建与启动 | 通过 | `docker build`；容器 `/healthz` 返回 `{"status":"ok"}` |
| Linux 原生进程冒烟测试 | 本地 Linux 容器与 GitHub Ubuntu CI 均通过 | `scripts/linux-smoke.mjs` 验证启动、健康检查、PG 断连、重启后资产/样本/事件持久化 |
| kind 中 PostgreSQL 18 | 通过 | `scripts/k8s-smoke.mjs`：真实 Pod + 已绑定 PVC + PG18 只读采集，B/S 资产采样与服务重启持久化 |
| K8S 只读 ServiceAccount | 通过 | 同一脚本使用命名空间只读 token；可读 Pod/PVC/事件，不能读取 Secret 或删除 Pod |
| K8S 容器重启信号 | 通过 | 测试容器强制重启后，采集器读取 `restartCount` 和终止时间，PG 数据与监控账号仍可用 |
| K8S PG18 + 独立 PG18 存储 | 本地通过 | 同一 B/S 资产的 PG/K8S 联合采样写入独立 PG18，重启后数据仍可读 |
| K8S 发现与资源关联 | 本地通过 | 只读 ServiceAccount 在真实 kind 集群发现 PG18 Pod、Deployment、Service、Node、PVC/PV；Pod 资源 API 缺失显式报错，Secret 读取被拒绝 |
| K8S metrics-server | 本地通过 | 官方 metrics-server 0.9.1 在测试 kind 集群提供真实 Pod/Node CPU 和内存；只读 ServiceAccount 采样成功 |
| K8S 探针故障 | 本地通过 | kind 中运行独立测试 Pod，真实 readiness probe 失败使 `Ready=False`，只读采集器读取 `Unhealthy` 事件；`scripts/k8s-probe-smoke.mjs` 复核 |
| PG17 长期存储拒绝 | 本地通过 | 独立 PG17 容器作错误目标，`scripts/reject-non18-store.mjs` 验证 DbSphere 明确拒绝启动 |
| Linux PG14 + node_exporter + 独立 PG18 存储 | 本地通过 | 真实 node_exporter 1.12.1 输出；CPU、内存、文件系统、IO、网络采集，连续样本速率与服务重启持久化 |
| Linux PG14 + PgBouncer 1.26.1 + 独立 PG18 存储 | 本地及 Linux CI 通过 | `stats_users` 只读管理控制台；真实连接池查询、凭据加密、PG18 采样和服务重启持久化 |
| PostgreSQL 14 | 通过 | 14.24，`pg_monitor` 账号读取基础指标、11 个诊断分区及真实锁阻塞链 |
| PostgreSQL 15 | 通过 | 15.20，同上 |
| PostgreSQL 16 | 通过 | 16.15，同上 |
| PostgreSQL 17 | 通过 | 17.11，同上 |
| PostgreSQL 18 | 通过 | 18.6，同上；官方容器使用 `/var/lib/postgresql/18/docker` 数据目录 |

兼容性测试使用临时 PostgreSQL 容器与 `scripts/test-pg-compat.ps1`，五版均验证 11 个诊断分区查询和真实锁等待的阻塞 PID 关系。上述结果尚不等于所有指标、扩展、Linux/K8S 运行形态或云服务兼容性通过。V1.0 需要完整支持矩阵和故障演练。

## 下一步

Go 巡检开发预览已支持内置模板、阈值版本、手动运行、定时领取、重试以及 Markdown/HTML 报告。PG18 实库验证了断连巡检与开放问题事件共用同一事件 ID，以及采集内部失败的巡检记录；浏览器完成手动巡检与历史结果到事件证据页的验收。定时运行的长周期稳定性仍待验证，当前不作为正式版发布依据。

1. 推进 V0.6 跨层根因分析，并实现 Linux 一键离线部署包；V1.0 对断网安装、升级和回滚作正式验收。
2. 补充 Prometheus PVC 真实指标与真实驱逐故障演练，维护可复现安装、故障演练和存储迁移文档。
3. 按 [开发任务清单](DEVELOPMENT_TASKS.md) 推进巡检、SQL 性能、跨层证据化根因和应用 Trace，逐版验收。
