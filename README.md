# DbSphere

**全域数据库智能运维管理平台** · 开源数据库监控、诊断与运维工作台

![当前开发版本 v0.6.0-dev](https://img.shields.io/badge/当前开发版本-v0.6.0--dev-2F6FEB?style=flat-square) [![Apache License 2.0](https://img.shields.io/badge/License-Apache--2.0-4C778A?style=flat-square)](LICENSE)

DbSphere 面向已有 PostgreSQL 环境，以**一个问题、一个事件 ID、一条可追溯证据链**为核心，把资产、数据库指标、Linux/Kubernetes 资源、连接池、告警、巡检、SQL 性能和应用 Trace 汇集到浏览器工作台。第一阶段支持 PostgreSQL 14–18；“全域”是产品方向，不表示当前支持所有数据库。

## 功能说明

| 领域 | 功能 |
|---|---|
| 资产纳管 | 浏览器登记与管理 PostgreSQL 实例，区分 Linux 与 Kubernetes 运行环境；凭据加密保存。Go 开发版可按唯一 Deployment/StatefulSet 自动解析重建后的 Pod，歧义时明确报错。可选接入 node_exporter、Kubernetes 只读 API、PgBouncer、备份信息和应用 Trace。 |
| 连续监控 | 采集连接、会话、等待、锁链、长事务、复制/WAL、检查点、autovacuum、表与索引容量；从长期样本估计数据库容量增长。可结合 Linux CPU/内存/磁盘、K8S Pod/Node/PVC/事件和连接池状态。缺失或无权限的数据明确标为未知。 |
| 问题与根因 | 采集错误、阈值告警和恢复归入事件；事件详情展示时间线、原始脱敏证据、跨层关联及可解释的根因候选，并记录人工确认或驳回。应用 Span、SQL 指纹、锁阻塞和基础设施信号可关联到同一事件 ID；按服务展示已观测的调用与错误 Span，证据不足时显示“未定位”。可导出同一事件 ID 的脱敏复盘报告。 |
| 告警与值班 | 受控规则、阈值、持续时间和严重度；告警去重、维护静默、确认、恢复、升级与持久化 Webhook/邮件投递。采集器失联、权限错误和样本陈旧有独立状态，不因指标缺失推断健康。 |
| 巡检与报告 | PostgreSQL、Linux、Kubernetes 和备份模板；手动与定时巡检、阈值版本、异常与未知项、历史差异、Markdown/HTML 报告及关联事件 ID。 |
| 访问与审计 | 管理员会话和可选全局只读查看账号；Go 开发版将管理员 API 写入尝试与 HTTP 结果记录到 PG18，系统状态页可查最近 100 条，事件操作可按同一事件 ID 查阅。资产范围权限、审批与防篡改仍在开发。 |
| SQL 性能 | Top SQL 指纹与趋势、退化线索、慢日志和执行计划安全导入；不保存原始 SQL 文本与参数。 |
| 数据与部署 | 独立 PostgreSQL 18 保存长期监控数据；B/S 架构。提供容器化一键部署与离线部署方案，目标机无需安装 Node.js 或 Go 编译器。Go 离线包可对随包 PG18 执行一键备份并生成校验清单。 |
| AI 增强 | Go 开发版可选接入兼容 Chat Completions 的本地或 HTTPS 模型，按事件请求脱敏证据解读，并校验证据 ID；可显式开启严重告警的自动摘要，每日预算和同事件审计受 PG18 约束。预测性预警和运维问答仍在规划。**不接模型时**，采集、告警、巡检和规则分析独立可用；模型不执行生产变更。 |

### 一个问题的全链路定位

1. 从异常告警或资产进入问题事件，先确认影响范围与事件 ID。
2. 按时间查看应用 Trace、PostgreSQL 会话/等待/SQL、PgBouncer、Linux 与 Kubernetes 的同窗证据。
3. 查看根因候选、支持证据、时间顺序和缺失信号；证据不足时保持“未定位”。
4. 记录人工结论与处置，再以新样本和恢复证据关闭同一个事件。

## 当前交付状态

`main` 的开发版本为 **v0.6.0-dev**；[最新正式发布版](https://github.com/cuipengdba/DbSphere/releases/latest)为现有 Node.js 后端实现。V1.0 的交付目标是 **Go 后端 + 内嵌静态前端 + HTTP API + 独立 PG18 存储**。Go 版已能进行 PG14–18 基础采集、Linux/K8S/PgBouncer 只读采集、规则告警、同事件 ID 证据分析和部分巡检，但尚未完成全部功能对等、生产环境验收与正式发布。完整的跨应用根因分析、AI、生产 K8S/通知路径及离线升级回滚仍在开发。具体状态以[开发进度](docs/DEVELOPMENT_STATUS.md)和[Go 迁移清单](docs/GO_UI_MIGRATION.md)为准。

## 部署与接入

- [部署说明](docs/DEPLOYMENT.md)：容器一键部署、PG18 存储及运行配置。
- [离线部署](docs/OFFLINE_DEPLOYMENT.md)：Go 静态程序与容器离线包的开发验收范围。
- [Kubernetes 接入](docs/DEPLOYMENT_V0.2.md)：只读 ServiceAccount、Namespace 与指标权限。
- [告警通知](docs/NOTIFICATIONS_V0.3.md)、[巡检报告](docs/INSPECTIONS_V0.4.md)、[SQL 性能](docs/SQL_PERFORMANCE_V0.5.md)：各功能的配置与使用。
- [可选 AI 诊断](docs/AI_INTEGRATION.md)：Go 版接入方式、脱敏范围与故障回退。

纳管目标建议使用专用 `pg_monitor` 账号，默认只读。仅有 PostgreSQL 连接串时只提供数据库侧观测；主机、K8S、连接池、备份和应用链路需要相应的数据源。对外访问应使用 HTTPS 入口。

## 项目文档

- [功能与产品边界](docs/PRODUCT_DESIGN.md)
- [V0.1–V1.0 功能规划](docs/ROADMAP.md)
- [开发任务归档](docs/DEVELOPMENT_TASKS.md)

DbSphere 是独立自主研发的开源项目，采用 [Apache License 2.0](LICENSE)。欢迎通过 [GitHub Issues](https://github.com/cuipengdba/DbSphere/issues) 提交 PostgreSQL 故障场景与需求。
