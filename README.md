# DbSphere

**全域数据库智能运维管理平台** · 开源数据库监控、诊断与运维工作台

[![当前版本 v1.19.0](https://img.shields.io/badge/当前版本-v1.19.0-238636?style=flat-square)](https://github.com/cuipengdba/DbSphere/releases/tag/v1.19.0) [![Apache License 2.0](https://img.shields.io/badge/License-Apache--2.0-4C778A?style=flat-square)](LICENSE)

DbSphere 面向已有 PostgreSQL 环境，以**一个问题、一个事件 ID、一条可追溯证据链**为核心，把资产、数据库指标、Linux/Kubernetes 资源、连接池、告警、巡检、SQL 性能和应用 Trace 汇集到浏览器工作台。第一阶段支持 PostgreSQL 14–18；“全域”是产品方向，不表示当前支持所有数据库。

## 功能说明

| 领域 | 功能 |
|---|---|
| 监控工作台 | 集群总览显示纳管实例、观测到的主备角色、复制回放差距及开放事件。共享时间范围贯穿实例指标、等待、SQL、锁与运行环境。等待分析按类型、事件和 Query ID 下钻，并可查看区间锁链、SQL 增量排名、慢日志及计划证据。事件详情可跳回故障时段。支持 Linux CPU/内存趋势及 Kubernetes Pod、Node、PVC 快照。 |
| 资产纳管 | 浏览器登记与管理 Linux、Kubernetes 及托管 PostgreSQL。支持 YAML、CSV 和集群清单批量预览、导入、使用凭据引用导出及分批连接权限检查。重试和重复导入保留资产 ID。可选接入 node_exporter、Kubernetes 只读 API、PgBouncer、Patroni、备份信息和应用 Trace。 |
| 连续监控 | 采集连接、会话、等待、锁链、长事务、复制/WAL、检查点、autovacuum、表与索引容量；从长期样本估计数据库容量增长，并用约 24 小时的 PG18 历史样本触发无模型增长速率告警。可结合 Linux CPU/内存/磁盘、K8S Pod/Node/PVC/事件、连接池状态和 Patroni 主备拓扑。缺失或无权限的数据明确标为未知。管理员可查看当前 Go 进程的自动采集批次、耗时和错误。 |
| 图形可观测性 | 依据集群、实例、数据库及统一时间轴查看趋势。最多五实例同窗比较，支持最近 30 天的持久化小时汇总、有效采样、峰值、原始样本和下钻。系统状态页可查询汇总进度。事件内的关系图及时间轴可追到应用 Trace、数据库、复制、连接池和环境证据。控制台默认浅色，可切换深色。 |
| 问题与根因 | 采集错误、阈值告警和恢复归入事件；事件详情展示时间线、原始脱敏证据、跨层关联及可解释的根因候选，并记录人工确认或驳回。应用 Span、SQL 指纹、锁阻塞和基础设施信号可关联到同一事件 ID；唯一资产匹配时还可沿服务 → PgBouncer → PG 等待 PID → 阻塞会话定位，PgBouncer 的应用业务地址可与只读统计管理地址分开配置。可选 Query ID 唯一匹配时从请求下钻到 SQL；K8S 应用 Pod UID 经只读 API 核对后，可在事件图连接应用 Pod、Node 与 PVC 引用及单独核对的 PVC 阶段。按服务区分同期 Trace 与故障线索，显示可选采样声明与时间异常，证据不足时显示“未定位”。可导出同一事件 ID 的脱敏复盘报告。 |
| 告警与值班 | 受控规则、阈值、持续时间和严重度；告警去重、维护静默、确认、恢复、升级与持久化 Webhook/邮件投递。采集器失联、权限错误和样本陈旧有独立状态，不因指标缺失推断健康。 |
| 巡检与报告 | PostgreSQL、Linux、Kubernetes 和备份模板；手动与定时巡检、阈值版本、异常与未知项、历史差异、Markdown/HTML 报告及关联事件 ID。 |
| 访问与审计 | 管理员、可选只读查看及独立审批账号；Go 版可用资产 UUID 白名单限制查看账号。事件页支持申请、独立审批、一次性执行只读采样工单，并把申请、决定、样本结果和两种账号的 API 操作记录关联同一事件 ID。个人账号、组织/租户隔离及审计防篡改仍在开发。 |
| SQL 性能 | Top SQL 指纹与趋势、退化线索、慢日志和执行计划安全导入；不保存原始 SQL 文本与参数。 |
| 复制与服务 | 核对主备、复制进度、历史切换及路由，展示回放积压、PgBouncer 排队和配置端点的实际后端。 |
| 容量与恢复 | 按数据库、Schema、表、索引和分区核对容量、增长与维护压力。接入 pgBackRest，核对备份依赖、WAL 缺口和 PITR 演练结果。 |
| Linux 与 Kubernetes | 主机指标可下钻块设备、网卡和文件系统。Kubernetes 按对象 UID 追踪 Workload、Pod、Node、PVC、EndpointSlice 和资源覆盖。 |
| 数据与部署 | 独立 PostgreSQL 18 保存长期监控数据；B/S 架构。提供容器化一键部署与离线部署方案，目标机无需安装 Node.js 或 Go 编译器。Go 离线包可对随包 PG18 执行一键备份并生成校验清单。 |
| AI 增强 | Go 版可选接入兼容 Chat Completions 的本地或 HTTPS 模型，管理员可在事件页请求脱敏证据摘要或围绕同一事件提问；回答需引用实际证据 ID 并说明不确定性。可显式开启严重告警后的自动摘要，每日预算和同事件审计受 PG18 约束。提前预测故障的 AI 预警尚未验证。**不接模型时**，采集、告警、巡检和规则分析独立可用；模型不执行生产变更。 |

### 一个问题的全链路定位

1. 从异常告警或资产进入问题事件，先确认影响范围与事件 ID。
2. 按时间查看应用 Trace、PostgreSQL 会话/等待/SQL、PgBouncer、Patroni、Linux 与 Kubernetes 的同窗证据。
3. 按故障阶段与证据判断筛选。对照故障前、触发及恢复快照，区分直接证据、同窗线索、标识不符和证据不足。
4. 记录人工结论与处置，再以新样本和恢复证据关闭同一个事件。

## 当前交付状态

当前正式版 **V1.19.0** 提供 Go 静态服务端和一键离线容器安装包。Web 控制台默认浅色，可在顶部切换深色。监控工作台并排呈现集群身份、成员、事件与时序，可用性时间条可打开同一故障或恢复事件。实例时序、等待和 SQL 排名共用选定时间范围。打开同一个事件即可回放首次发现、直接证据、时间相关信号、根因候选和恢复记录。支持对照 PostgreSQL、SQL、复制、Linux/Kubernetes 和可选应用 Trace，并从原始采样定位同窗趋势。可选 AI 提供证据问答与严重告警解读。[一键离线安装包](https://github.com/cuipengdba/DbSphere/releases/tag/v1.19.0) · [监控工作台](docs/MONITORING_WORKSPACE.md) · [支持矩阵](docs/SUPPORT_MATRIX.md) · [安全审查](docs/V1_SECURITY_REVIEW.md)

## 部署与接入

- [Go 一键部署与离线包](docs/OFFLINE_DEPLOYMENT.md)：Go 静态程序、容器离线包及当前验收范围。
- [PG18 存储](docs/STORAGE_V0.3.md)：长期数据、保留策略与备份恢复。
- [Kubernetes 接入](docs/DEPLOYMENT_V0.2.md)：只读 ServiceAccount、Namespace 与指标权限。
- [Patroni 接入与验收](docs/PATRONI_ACCEPTANCE.md)：只读拓扑、告警、同事件根因候选及本机故障演练。
- [告警通知](docs/NOTIFICATIONS_V0.3.md)、[巡检报告](docs/INSPECTIONS_V0.4.md)、[SQL 性能](docs/SQL_PERFORMANCE_V0.5.md)：各功能的配置与使用。
- [可选 AI 诊断](docs/AI_INTEGRATION.md)：Go 版接入方式、脱敏范围与故障回退。
- [事件工单与审批](docs/V0.9_RELEASE_GATE.md)：双角色只读采样、审批期限、审计和限制。

纳管目标建议使用专用 `pg_monitor` 账号，默认只读。仅有 PostgreSQL 连接串时只提供数据库侧观测；主机、K8S、连接池、备份和应用链路需要相应的数据源。对外访问应使用 HTTPS 入口。

## 项目文档

- [功能与产品边界](docs/PRODUCT_DESIGN.md)
- [V0.1–V1.0 功能规划](docs/ROADMAP.md)
- [开发任务归档](docs/DEVELOPMENT_TASKS.md)

DbSphere 是独立自主研发的开源项目，采用 [Apache License 2.0](LICENSE)。欢迎通过 [GitHub Issues](https://github.com/cuipengdba/DbSphere/issues) 提交 PostgreSQL 故障场景与需求。
