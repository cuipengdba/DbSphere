# DbSphere

**全域数据库智能运维管理平台** · 开源数据库监控、诊断与运维工作台

[![当前版本 v2.0.0](https://img.shields.io/badge/当前版本-v2.0.0-238636?style=flat-square)](https://github.com/cuipengdba/DbSphere/releases/tag/v2.0.0) [![Apache License 2.0](https://img.shields.io/badge/License-Apache--2.0-4C778A?style=flat-square)](LICENSE)

DbSphere 面向已有 PostgreSQL 环境，以**一个问题、一个事件 ID、一条可追溯证据链**为核心，把资产、数据库指标、Linux/Kubernetes 资源、连接池、告警、巡检、SQL 性能和应用 Trace 汇集到浏览器工作台。监控目标支持 PostgreSQL 14–19，长期监控存储使用独立 PostgreSQL 18。

## 功能说明

| 领域 | 功能 |
|---|---|
| 监控工作台 | 集群首屏显示故障成员、主备角色、异常起点及可用性时间带。连接压力、锁等待、WAL 和复制曲线共用游标，可查看当时的原始采样。分享地址保留成员和时间窗。实例等待分析按类型、事件和 Query ID 下钻，并可核对锁链、SQL 增量排名、慢日志及计划证据。 |
| 实时观测大盘 | 支持全屏值班、夜览模式和组织、集群、引擎筛选。同步显示采样新鲜度、主备与 Pod 拓扑、连接使用率、TPS、活跃等待、阻塞、复制延迟及 Top SQL。趋势标注异常时段，进入同一事件 ID 查看检测、根因与恢复证据。 |
| 增强可观测性 | 增强离线包内置 Prometheus 和 Grafana OSS，自动发现已登记的 Linux Exporter。内置环境总览、PostgreSQL 集群、等待与 SQL、主备复制、Linux/Kubernetes 和 MySQL 8.4 专题。平台管理员可从实时大盘或原事件进入专页，保留资产和时间窗。原生监控、根因证据和长期数据继续使用 PostgreSQL 18。 |
| 容器存储观测 | Docker 和 Kubernetes PostgreSQL 的磁盘吞吐、IOPS、I/O PSI、`active_file`、`inactive_file` 与缓存回收。支持全屏实时、历史同窗对比、事件回放和四个 Grafana 专项。[部署与指标口径](docs/CONTAINER_STORAGE.md) |
| 首次初始化 | 全新安装从本机完成管理员首登改密，也可在安装前预设管理员密码。旧版升级沿用已有账号、凭据与事件。 |
| 资产纳管 | 浏览器登记与管理 Linux、Kubernetes 及托管 PostgreSQL。支持 YAML、CSV 和集群清单批量预览、导入、使用凭据引用导出及分批连接权限检查。重试和重复导入保留资产 ID。可选接入 node_exporter、Kubernetes 只读 API、PgBouncer、Patroni、备份信息和应用 Trace。 |
| 托管数据库采集 | 通过统一 Go 采集契约记录 PostgreSQL 主库、只读副本、TLS 证书与主机名校验、会话只读状态及权限缺口。支持配置独立 CA。[托管 PostgreSQL 接入](docs/MANAGED_POSTGRESQL.md) |
| MySQL 8.4 只读预览 | 浏览器纳管与定时采集，展示连接、InnoDB 锁等待、Top SQL 指纹和基础复制状态。阻塞线程关系、原始样本和恢复证据归入同一事件 ID。[MySQL 预览接入](docs/MYSQL_PREVIEW.md) |
| 连续监控 | 采集连接、会话、等待、锁链、长事务、复制/WAL、检查点、autovacuum、表与索引容量；从长期样本估计数据库容量增长，并用约 24 小时的 PG18 历史样本触发无模型增长速率告警。可结合 Linux CPU/内存/磁盘、K8S Pod/Node/PVC/事件、连接池状态和 Patroni 主备拓扑。缺失或无权限的数据明确标为未知。管理员可查看当前 Go 进程的自动采集批次、耗时和错误。 |
| 图形可观测性 | 依据集群、实例、数据库及统一时间轴查看趋势。最多五实例同窗比较，支持最近 30 天的持久化小时汇总、有效采样、峰值、原始样本和下钻。系统状态页可查询汇总进度。事件内的关系图及时间轴可追到应用 Trace、数据库、复制、连接池和环境证据。控制台默认浅色，可切换深色。 |
| 问题与根因 | 采集错误、阈值告警和恢复归入事件；事件详情展示时间线、原始脱敏证据、跨层关联及可解释的根因候选，并记录人工确认或驳回。应用 Span、SQL 指纹、锁阻塞和基础设施信号可关联到同一事件 ID；唯一资产匹配时还可沿服务 → PgBouncer → PG 等待 PID → 阻塞会话定位，PgBouncer 的应用业务地址可与只读统计管理地址分开配置。可选 Query ID 唯一匹配时从请求下钻到 SQL；K8S 应用 Pod UID 经只读 API 核对后，可在事件图连接应用 Pod、Node 与 PVC 引用及单独核对的 PVC 阶段。按服务区分同期 Trace 与故障线索，显示可选采样声明与时间异常，证据不足时显示“未定位”。可导出同一事件 ID 的脱敏复盘报告。 |
| 告警与值班 | 受控规则、阈值、持续时间和严重度；告警去重、维护静默、确认、恢复、升级与持久化 Webhook/邮件投递。采集器失联、权限错误和样本陈旧有独立状态，不因指标缺失推断健康。 |
| 无模型风险预警 | 使用周期基线和计划变更识别连接、容量和 WAL 趋势，并提示同类故障重复出现。支持影子复核、可选通知和历史回测，沿用原事件 ID 及采样证据。 |
| 事件证据助手 | 可选本地或 HTTPS 模型逐条引用原事件证据，给出候选解释、反证与核查建议。组织授权、输入上限和审计适用于每次审核；关闭模型时可继续用规则核查。 |
| 运维系统对接 | 提供组织级版本化资产和事件 API、签名 Webhook、接收端限流重试与 Prometheus 平台指标。外部告警和工单回执与同一事件 ID 及证据时间线关联。 |
| 巡检与报告 | PostgreSQL、Linux、Kubernetes 和备份模板；手动与定时巡检、阈值版本、异常与未知项、历史差异、Markdown/HTML 报告及关联事件 ID。 |
| 组织与审计 | 支持组织级资产和账号隔离，按项目团队授予资产访问范围。事件分享、时序、原始证据和 AI 分析沿用资产授权。支持 OIDC 与本地账号、按组织轮换凭据和撤销会话，并可导出签名审计进行离线核验。 |
| 受控运维作业 | 在事件内预检、预演、申请、独立审批和执行一次只读采样或巡检。支持幂等键、审批有效期、执行期限和结果回写。管理员可配置隔离恢复核验或试点单表 ANALYZE，使用单独的目标数据库账号。 |
| 平台高可用 | 支持一键离线启动两个控制台和采集副本。采集任务共享 PG18 租约，维护任务由选出的主节点运行。副本切换后可沿用会话和事件链接。存储不可用时显示状态，恢复后自动刷新。 |
| SQL 性能 | Top SQL 指纹与趋势、退化线索、慢日志和执行计划安全导入。采集能力表显示系统视图来源、扩展安装状态与当前权限，采集仅保存 SQL 指纹与统计。 |
| 复制与服务 | 核对主备、回放积压和历史切换。独立探测 PgBouncer、HAProxy 和 Kubernetes Service 的实际入口，对照实际后端、期望角色、连接池队列、Service 端口与 EndpointSlice。告警可回查原始采样和恢复证据。 |
| 变更时间线 | 在同一实例时间窗查看配置、容器镜像、路由和拓扑的实测变化，登记发布、维护及其他运维变更。区分发生、首次观测和入库时间，关联前后采样，并通过分享链接定位变更或返回原事件。 |
| 跨实例影响 | 同一代理或 Kubernetes Service 的共同入口故障可关联多个实例事件。按主子事件核对恢复范围、原始探测证据及同窗失败服务，支持人工关联、拆分及查看历史。 |
| PostgreSQL 高可用 | 查看 Patroni 成员、DCS 最近可见时间、心跳、同步资格和切换时间线。独立探测主、备库入口，并在同一事件中对照切换前后的角色、后端及回放差距。 |
| 逻辑复制 | 按纳管数据库查看 publication、subscription 和逻辑槽。独立显示 WAL 保留、消费者确认位置、订阅同步和应用位置。积压告警可定位到原事件和具体槽的采样。 |
| 根因规则工作台 | 维护支持引用下限与首次观测条件，记录每次规则发布。按事件开始时版本与当前版本回放同一组证据，查看候选、反证、缺失项及恢复后复盘。 |
| 容量与恢复 | 按数据库、Schema、表、索引和分区核对容量、增长与维护压力。接入 pgBackRest，核对恢复链与 WAL 逐段缺口。分别显示业务库 PITR 和监控 PG18 隔离还原的实测 RPO、RTO 与验收证据。 |
| 对象维护 | 核对表的主分叉死元组估算、XID 与 MultiXact 冻结、有效维护阈值、分区覆盖和索引状态。按同一对象和采样证据对照 Vacuum、Analyze 前后变化。 |
| Linux 与 Kubernetes | 主机指标可下钻块设备、网卡和文件系统。Kubernetes 核对对象身份、就绪和卷绑定历史。CloudNativePG 可预览纳管候选，核对主备角色、服务入口、切换与备份状态。 |
| 数据与部署 | 独立 PostgreSQL 18 保存长期监控数据与采集队列，采集进程重启可接续未完成任务。B/S 架构，提供容器化一键部署、离线部署和随包 PG18 备份。目标机无需安装 Node.js 或 Go 编译器。 |
| 历史数据分层 | 按组织和指标设置原始保留周期与小时汇总周期。资产历史趋势支持 90 天查询，并显示原始、归档和混合来源。事件或巡检引用的原始采样可继续下钻。 |
| AI 增强 | Go 版可选接入兼容 Chat Completions 的本地或 HTTPS 模型，管理员可在事件页请求脱敏证据摘要或围绕同一事件提问；回答需引用实际证据 ID 并说明不确定性。可显式开启严重告警后的自动摘要，每日预算和同事件审计受 PG18 约束。提前预测故障的 AI 预警尚未验证。**不接模型时**，采集、告警、巡检和规则分析独立可用；模型不执行生产变更。 |

### 一个问题的全链路定位

1. 从异常告警或资产进入问题事件，先确认影响范围与事件 ID。
2. 按时间查看应用 Trace、PostgreSQL 会话/等待/SQL、PgBouncer、Patroni、Linux 与 Kubernetes 的同窗证据。
3. 按故障阶段与证据判断筛选。对照故障前、触发及恢复快照，区分直接证据、同窗线索、标识不符和证据不足。
4. 记录人工结论与处置，再以新样本和恢复证据关闭同一个事件。

## 当前交付状态

当前正式版 **V2.0.0** 为 Linux amd64 和 arm64 提供 Go 静态服务端、一键离线容器包和原生 systemd 安装包。原生安装可连接已有独立 PostgreSQL 18，预检会确认服务端版本、数据库写入状态与数据库架构迁移权限。Web 控制台默认浅色，可在顶部切换深色。监控工作台并排呈现集群身份、成员、事件与时序，可用性时间条可打开同一故障或恢复事件。实例时序、等待和 SQL 排名共用选定时间范围。管理员可启动短时高频采样，并从同一个事件回放会话身份、并行进程和历史阻塞树。打开事件即可查看首次发现、直接证据、时间相关信号、根因候选和恢复记录。支持对照 PostgreSQL、SQL、复制、Linux/Kubernetes 和可选应用 Trace，并从原始采样定位同窗趋势。可选 AI 提供证据问答与严重告警解读。[一键离线安装包](https://github.com/cuipengdba/DbSphere/releases/tag/v2.0.0) · [监控工作台](docs/MONITORING_WORKSPACE.md) · [支持矩阵](docs/SUPPORT_MATRIX.md) · [安全审查](docs/V1_SECURITY_REVIEW.md)

慢日志支持分批增量导入和重复记录检测。SQL 诊断按纳管数据库和 Query ID 回放计划，呈现扫描、连接方式、估算偏差与临时 I/O 的前后对照。[SQL 证据与前后计划](docs/MONITORING_WORKSPACE.md#sql-证据与前后计划)

接入 OpenTelemetry 后，同一事件可查看跨服务请求、PgBouncer 路由和 PostgreSQL 后端 PID 的对应证据，展示采样声明、缺失的父 Span 和时间异常。[全链路故障回放](docs/MONITORING_WORKSPACE.md#全链路故障回放)

## 部署与接入

- [事件证据助手与本地模型](docs/AI_EVIDENCE_REVIEW.md)
- [无模型风险预警与历史回测](docs/RISK_FORECAST.md)
- [长期指标留存与容量验收](docs/HISTORY_STORAGE.md)
- [外部运维接口与 Prometheus](docs/OPERATIONS_INTEGRATION.md)
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
