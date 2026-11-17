# DbSphere

**中文名：全域数据库智能运维管理平台**

**英文名：DbSphere**

DbSphere 是一个以 PostgreSQL 为第一阶段重点的开源数据库运维平台。目标是在一处完成资产纳管、持续监控、告警、巡检、SQL 性能诊断、故障定位、根因分析和处置追踪。V0.1–V1.0 支持 PostgreSQL 14–18，覆盖 Linux 主机及运行在 Kubernetes 中的 PostgreSQL，采用浏览器/服务器（B/S）架构；“全域”表示未来的产品方向，V1.0 不宣称支持所有数据库。

**2026-11-17 架构调整**：V1.0 后端必须为 Go，前端为静态页面加 HTTP API，并以单文件 Go 程序和 PG18 长期存储提供简化部署。当前已发布 V0.1–V0.5 与 V0.6 开发预览仍是 Node.js 历史实现。Go 开发版已具备独立镜像/离线包、PG14–18 基础采集、Linux node_exporter、初步 K8S 与 PgBouncer 只读采集、十二条 PG/Linux/K8S/PgBouncer 告警规则及同一事件 ID 的证据图；功能尚未与历史版本对等，不能作为 V1.0 正式版。六视图静态工作台已重整。进度和发布门槛见[Go 与工作台迁移清单](docs/GO_UI_MIGRATION.md)。

已发布 [V0.1.0 基础版](https://github.com/cuipengdba/DbSphere/releases/tag/v0.1.0)、[V0.2.0](https://github.com/cuipengdba/DbSphere/releases/tag/v0.2.0)、[V0.3.0](https://github.com/cuipengdba/DbSphere/releases/tag/v0.3.0)、[V0.4.0](https://github.com/cuipengdba/DbSphere/releases/tag/v0.4.0) 和 [V0.5.0](https://github.com/cuipengdba/DbSphere/releases/tag/v0.5.0)：Docker Compose 一键部署、独立 PostgreSQL 18 存储、Linux 与 Kubernetes 资源采集、跨层告警、静默维护窗、可选通知、版本化巡检、监控存储备份恢复演练及 SQL 性能洞察。后续版本仅在满足各自验收条件后发布。

## 核心原则

- **没有 AI 也能运维**：采集、阈值与规则告警、巡检、故障证据和根因候选完全独立运行；模型仅增强解释、问答与预警建议。
- **先观察，后操作**：默认只读纳管。生产处置动作需单独授权、审批、预演与审计。
- **证据先于结论**：每条根因候选都展示时间线、相关指标、日志、SQL、等待/锁、拓扑与反证，标明置信程度和数据缺口。
- **明确采集边界**：仅有 PostgreSQL 连接串时，提供数据库侧观测；主机、连接池、HA、备份及应用链路需要相应采集器、组件接口或应用 Trace 接入。
- **可独立部署**：面向已有 PostgreSQL、托管 PostgreSQL 和云 RDS，优先纳管，不要求由 DbSphere 创建或接管数据库集群。

## 阅读规划

- [V0.1–V1.0 十版本路线图](docs/ROADMAP.md)
- [产品设计与能力边界](docs/PRODUCT_DESIGN.md)
- [全版本开发任务清单](docs/DEVELOPMENT_TASKS.md)
- [当前开发状态与测试记录](docs/DEVELOPMENT_STATUS.md)
- [部署与接入](docs/DEPLOYMENT.md)
- [V0.1.0 发行说明](docs/RELEASE_NOTES_V0.1.md)
- [V0.2.0 发行说明](docs/RELEASE_NOTES_V0.2.md)
- [V0.3.0 发行说明](docs/RELEASE_NOTES_V0.3.md)
- [V0.4.0 发行说明](docs/RELEASE_NOTES_V0.4.md)
- [V0.5.0 发行说明](docs/RELEASE_NOTES_V0.5.md)
- [V0.2 一键部署与 PG18 存储](docs/DEPLOYMENT_V0.2.md)
- [V0.3 告警通知配置](docs/NOTIFICATIONS_V0.3.md)
- [V0.3 PG18 分区与保留策略](docs/STORAGE_V0.3.md)
- [V0.4 巡检与可恢复性](docs/INSPECTIONS_V0.4.md)
- [V0.5 SQL 性能洞察](docs/SQL_PERFORMANCE_V0.5.md)
- [V0.6 Linux 一键离线部署开发记录](docs/OFFLINE_DEPLOYMENT.md)
- [Go 后端与分层工作台迁移清单](docs/GO_UI_MIGRATION.md)
- [V0.7 Trace 开发记录](docs/TRACE_V0.7_PROGRESS.md)
- [参考产品与资料来源](docs/RESEARCH.md)

## 当前可用范围

已实现浏览器登录、PostgreSQL 资产增删改查、AES-256-GCM 加密凭据、PG14–18 版本识别、基础只读采样、采集失败/恢复事件与证据时间线。V0.2 的 PG 诊断采集覆盖会话、等待、阻塞链、长事务、WAL、检查点、autovacuum 和表/索引容量，五个大版本均有真实锁等待演练。Linux 主机可从 node_exporter 读取资源指标；K8S 可按 Namespace 发现 Pod、Workload、Service、Node、PVC/PV，并读取可用的 metrics-server 指标和可选的 Prometheus PVC 用量。PgBouncer 连接池统计为可选接入。V0.3 加入规则告警、同事件 ID 告警证据、采样陈旧状态、静默维护窗、持久化 Webhook/邮件通知和 PG18 月度采样/事件分区。V0.4 加入 PostgreSQL/Linux/K8S/备份巡检、容量粗估、Markdown/HTML 报告和 PG18 存储恢复演练。V0.5 加入 Top SQL 指纹趋势、退化提示、慢日志和执行计划安全导入。跨应用的完整全链路根因分析仍在 V0.6–V0.7 任务中。控制台会明确呈现数据缺口。

### 本机开发启动

Go 后端开发版可在 Linux 联网构建机运行 `bash scripts/one-click-go.sh`，以独立 PG18 和默认 `127.0.0.1:8081` 启动；Go 静态二进制与一键离线包说明见[离线部署文档](docs/OFFLINE_DEPLOYMENT.md)。以下 Node.js 命令用于已发布历史实现。

需要 Node.js 24 和 pnpm。首次启动先设置不少于 12 位的 `DBSPHERE_ADMIN_PASSWORD`，再执行：

```bash
pnpm install --frozen-lockfile
export DBSPHERE_ADMIN_PASSWORD='choose-a-strong-password'
pnpm start
```

默认仅监听 `127.0.0.1:8080`，数据保存在 `./data`。可设置 `DBSPHERE_DATA_DIR`、`DBSPHERE_HOST`、`DBSPHERE_PORT`、`DBSPHERE_COLLECT_INTERVAL_SECONDS`。首次启动会在数据目录生成权限为 `0600` 的主密钥；正式部署建议从独立密钥管理渠道设置 32 字节 base64 编码的 `DBSPHERE_MASTER_KEY`。更换主密钥前必须处理已有加密凭据。

Linux 容器镜像可用 `docker build -t dbsphere:dev .` 构建。对外提供浏览器访问时，应通过 HTTPS 反向代理，并设置 `DBSPHERE_COOKIE_SECURE=true`。目前尚未通过生产部署与安全验收。

建议为纳管目标创建专用 PostgreSQL 登录角色并授予 `pg_monitor`；DbSphere 不需要数据库超级用户。PG14 的监控兼容已纳入测试，但其上游已停止安全维护，控制台会显示 EOL 提示。

### Kubernetes 接入

如果 DbSphere 在 Kubernetes 内运行，可给它配置只读 ServiceAccount；如果在集群外运行，设置 `DBSPHERE_KUBECONFIG` 为可信 kubeconfig 文件路径，资产里的“K8S 集群”填写该文件中的 context 名。服务端禁止执行 kubeconfig 中的外部认证插件。Compose 部署可在 `.env` 中设置只读 kubeconfig 的绝对路径。所需的 Namespace/ClusterRole 权限与可选指标来源见 [V0.2 部署说明](docs/DEPLOYMENT_V0.2.md)。kind 中的 PostgreSQL 18 Pod、PVC/PV、Service、Node、只读 ServiceAccount 与容器重启已通过实际演练。

## 许可与参考边界

DbSphere 仓库采用 [Apache License 2.0](LICENSE)。DeepLens 的本地安装包及其程序代码、DBdoctor 的产品实现、Pigsty 的代码和界面均不属于本仓库；这里只参考公开或用户提供的功能信息，DbSphere 将独立设计与实现。若未来复用第三方组件或素材，应逐项核查原始许可与署名要求。

## 参与讨论

路线图是交付计划。建议通过 GitHub Issues 反馈 PostgreSQL 场景、关键指标、故障案例与版本优先级。
