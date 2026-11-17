# 开发状态与验证记录

> 更新于 2026-11-17。此文只记录已实际实现或测试的内容；版本发布以 GitHub Release 为准。

## 当前阶段

2026-11-17 新增 Go 后端与静态前端强制架构，以及分层后台页面。仓库已建立 Go 入口、嵌入静态资源、配置校验、PG18 schema 迁移、健康接口和管理员登录；Node 格式的密码哈希、凭据密文及 Session/CSRF 已用兼容向量验证。空 PG18 实库建表和月分区、Linux amd64 静态编译及 Go 测试通过。**Go 业务 API 仍在迁移，现有可用监控功能仍由 Node 历史实现提供**。界面从超长单页拆成六视图，总览和事件双栏在桌面与 390px 窄屏真实浏览器可见，未接入信号明确显示缺口。完整清单见[Go 与工作台迁移](GO_UI_MIGRATION.md)。

V0.7 Trace 首段能力正在开发：可选令牌保护的 OTLP/HTTP JSON 接口接收 PostgreSQL 客户端 Span，将服务及父子 Span 与唯一匹配的 PG 资产关联，并在原事件 ID 的证据图展示；敏感 SQL/参数/URL 不入库。真实 PG18 持久化冒烟及[14 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44067441805)已通过。协议、映射、采样率、时钟偏移、影响面和真实应用故障演练仍待完成，详见[开发记录](TRACE_V0.7_PROGRESS.md)。V0.6 正式版也尚未通过全部故障演练，因此目前不可宣称 V0.7 已发布。

**[V0.1.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.1.0)、[V0.2.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.2.0)、[V0.3.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.3.0)、[V0.4.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.4.0)、[V0.5.0 已发布](https://github.com/cuipengdba/DbSphere/releases/tag/v0.5.0)，V0.6 开发中**。已实现 B/S 服务端与浏览器页面、管理员登录和 CSRF 校验、PostgreSQL 资产增删改查、凭据加密、基础采集、采集失败与恢复共用一个事件 ID 的证据记录。PG14–18 已能读取会话/等待、长事务、锁链、WAL/归档、检查点、autovacuum、表/索引容量和复制槽 11 个诊断分区，并显示不可用原因。V0.2 新增独立 PG18 后端、SQLite 数据迁移、Docker Compose 一键部署、Linux node_exporter 资源采集；K8S 适配器支持按 Namespace 发现并关联 Pod/Workload/Service/Node/PVC/PV，读取容器重启/OOMKilled、事件与可用的 metrics.k8s.io 数据。当 PG 连接失败与近期 Pod OOMKilled 同时出现时，在**同一个事件 ID** 中给出中等置信度、可查看原始证据的根因候选。

V0.1 功能验收样例覆盖 Linux 原生 Node 进程及 kind 中的 K8S PostgreSQL。V0.2 的 PG18 存储、默认及外部 PG18 一键 Compose、Linux 主机采集和 PgBouncer 适配均通过 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/43722385125)。K8S Namespace 资源发现和 Pod → Workload/Service/Node/PVC/PV 关联已通过真实 kind 只读演练；Pod/Node 使用量也通过实际 metrics-server 验证。真实 kind 的探针失败 Pod 已由只读采集器读取 `Ready=False` 与 `Unhealthy` 事件，采集后已清理测试 Pod。PVC 实际用量可选从 Prometheus kubelet 指标读取，该路径目前只通过模拟响应测试。V0.1 SQLite 仅用于早期样例。应用 Trace 及通用跨层根因引擎属后续任务。当前只支持“采集连接失败 + 近期 Pod OOMKilled”的有限候选，**不能将其宣称为完整的全链路根因分析**。

## V0.6 开发中

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

1. 推进 V0.6 跨层根因分析，并实现 Linux 一键离线部署包；V1.0 对断网安装、升级和回滚作正式验收。
2. 补充 Prometheus PVC 真实指标与真实驱逐故障演练，维护可复现安装、故障演练和存储迁移文档。
3. 按 [开发任务清单](DEVELOPMENT_TASKS.md) 推进巡检、SQL 性能、跨层证据化根因和应用 Trace，逐版验收。
