# 参考产品与资料来源

> 调研日期：2026-11-15。此文记录设计依据与可验证的能力边界，不代表对参考产品的完整测试或复制授权。

## DeepLens 玄镜智巡平台

**来源**：用户提供的本地文件 `E:\DbSphere\DeepLens\使用说明-V26ai1.0.md`，以及 Linux/Windows 安装包的文件目录与 Linux 安装说明。未运行安装包，也未复用其中程序代码、依赖、界面、报告模板或文档内容。

使用说明描述了数据库与 Linux 主机纳管、可配置巡检模板、手动/定时巡检、异常概览、报告、工单、三级角色权限、AI 报告解读和自然语言问数。可借鉴的**产品问题**是“如何把巡检结果、异常与工单串起来”。文档主要展示周期性巡检流程，不能据此认定其具有连续时序监控、应用 Trace 或 PostgreSQL 根因分析能力，因此 DbSphere 将这些作为独立设计目标。

## DBdoctor

- [用户提供的产品下载页](https://www.dbdoctor.cn/product/11)：企业版/免费版下载与部署信息；官方页面提供 Linux 一键安装包和安装命令。
- [DBdoctor 官方首页](https://www.dbdoctor.cn/)：公开介绍 SQL 性能洞察、锁问题可视化、慢 SQL 指纹聚合、执行计划、根因诊断、AI 辅助和工单闭环。

可借鉴的**问题域**是从性能异常进入 SQL/锁证据，再提出优化或处置建议。官网描述的 eBPF、外置 Cost 优化器及多智能体属于其产品实现和宣传能力；DbSphere V1.0 不宣称具备同等内核透视、优化器或自动改写能力。DbSphere 首先使用 PostgreSQL 官方视图、扩展接口、日志、开放 Trace 和可审查的规则构建诊断。
其一键安装体现了低门槛部署的产品需求；DbSphere 将独立实现 Linux Docker Compose 一键部署。

## Pigsty

- [Pigsty 官方 PostgreSQL 监控文档](https://pigsty.io/docs/pgsql/monitor/)：覆盖 PostgreSQL、节点、连接池、HA 和日志的监控，并区分现有自建集群与仅连接串的 RDS 模式。文档说明仅连接串模式缺少主机、连接池、负载均衡和 HA 组件指标。
- [Pigsty 官方文档首页](https://pigsty.io/docs/)：其范围包括高可用、PITR、IaC、监控与组件化部署。
- [Pigsty GitHub 仓库](https://github.com/pgsty/pigsty)：项目代码与许可应在任何复用前单独核查。
- [Pigsty Docker 部署文档](https://pigsty.io/docs/setup/docker/)：容器快速体验、持久卷和随机密码生成；其文档明确单节点容器模式的生产边界。
- [Pigsty PostgreSQL 内核文档](https://pigsty.io/docs/pgsql/kernel/postgres/)：PG18 默认版本及 PG14–18 版本选择。

可借鉴的**架构问题**是如何组合不同采集层并明确监控深度。DbSphere 的优先目标是统一纳管与诊断工作台；不要求用户交给 DbSphere 部署或重建 PostgreSQL 集群。不会复制 Pigsty 的代码、配置、图表或部署剧本。
DbSphere 自身长期存储选择独立 PG18，产品交付选择最小权限的双容器 Compose 模式；这一决策不意味着复用 Pigsty 的具体部署脚本或监控存储栈。

## 技术标准与版本依据

- [PostgreSQL 官方版本策略](https://www.postgresql.org/support/versioning/)：截至调研日，PostgreSQL 15–18 仍在支持期，14 于 2026-11-12 结束支持。按项目需求 DbSphere 仍兼容监控 PG14，并明确显示 EOL 状态。
- [Kubernetes StatefulSet 文档](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)与[系统组件指标文档](https://kubernetes.io/docs/concepts/cluster-administration/system-metrics/)：K8S 工作负载身份、存储和基础设施遥测的设计依据。
- [PostgreSQL 统计系统](https://www.postgresql.org/docs/current/monitoring-stats.html)：`pg_stat_activity`、等待事件和累积统计等数据库侧观测依据。
- [PostgreSQL `pg_stat_statements` 文档](https://www.postgresql.org/docs/current/pgstatstatements.html)：SQL 聚合统计需要启用扩展，且预加载配置可能要求重启；因此它是分级可选信号。
- [PgBouncer 官方管理控制台](https://www.pgbouncer.org/usage)：`stats_users` 可访问只读统计，`SHOW POOLS` 与 `SHOW STATS` 提供连接池排队和查询累计信息；DbSphere 的可选适配器仅调用这两个命令。
- [OpenTelemetry PostgreSQL 语义约定](https://opentelemetry.io/docs/specs/semconv/db/postgresql/)：应用侧数据库调用 Span 的标准依据。应用到数据库的全链路分析须实际接入应用遥测，不能仅靠数据库指标声称完成。

## 设计归纳

### 界面组织补充（2026-11-17）

用户指定的 [Pigsty 中文站](https://doc.pigsty.cc/)以清晰标题层级和聚焦入口组织内容；其[监控仪表盘目录](https://pigsty.io/docs/pgsql/dashboard/)按总览、集群、实例、数据库/查询逐级下钻。[DBdoctor 官方首页](https://www.dbdoctor.cn/)把性能问题、锁、慢 SQL 和根因诊断放在同一处置路径。本地 DeepLens 使用说明明确左侧菜单的监控中心、资源中心和执行中心。DbSphere 据此采用左侧任务导航与事件列表/证据双栏；视觉使用自己的蓝色动作色、状态色和组件实现，不复用参考站点图像、CSS 或仪表盘。


DeepLens 提供巡检与报告的流程启发，DBdoctor 提供 SQL/锁问题诊断的目标启发，Pigsty 提供 PostgreSQL 多层观测与既有环境接入的边界启发。DbSphere 的独立定位是：**面向已有 PostgreSQL 环境、以证据关联和可解释根因为核心、AI 可选且默认不参与生产操作的开源运维工作台**。
