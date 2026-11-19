# DbSphere V0.6.0 RC1（Go 预览版）

**全域数据库智能运维管理平台 DbSphere** 的首个 Go 架构 V0.6 候选包。此版本是预发布，不代表 V0.6 正式验收完成；V0.5.0 仍是最新正式版。采用 Apache License 2.0。

## 本次可用能力

- Go 后端内嵌静态 B/S 页面，通过 HTTP API 管理 PostgreSQL 14–18 资产；长期监控数据保存在独立 PostgreSQL 18。
- 只读采集 PostgreSQL、Linux 与 Kubernetes 的可用信号，包含 Pod、Node、Service、PVC/PV、事件和可选的 PgBouncer、Patroni、应用 Trace；缺少权限或数据源时显示缺口。
- 一个问题使用同一事件 ID 关联告警、样本、应用/数据库 Span、SQL 与基础设施证据、根因候选、人工审阅和恢复验证。事件与资产列表可按条件搜索及分页；证据不足时不输出已证实根因。
- 规则告警、静默与通知、巡检报告、SQL 性能线索和默认关闭的可选 AI 证据解读；不接模型也可完成采集、告警和事件诊断。
- Linux x86_64 Go/PG18 容器离线包：目标机预装 Docker Engine 与 Compose v2，复制 `.run` 和同名 `.run.sha256` 后运行 `bash <包名>.run`。安装器校验外层与内部清单，从包内导入镜像并以 `--pull never` 启动。另提供连接外部 PG18 的静态 Go 二进制。

## 验收与边界

- PG14–18 真实容器采集与部分故障演练、本机 kind 的 Kubernetes/PG18 同事件根因和 PVC 恢复、Linux CI 的无网络容器安装与校验失败拒绝已通过。详细范围见[支持矩阵](SUPPORT_MATRIX.md)、[本机 kind 验收](KIND_ACCEPTANCE.md)和[开发任务清单](DEVELOPMENT_TASKS.md)。
- V0.6 的八类故障完整矩阵、所有根因候选的反证、物理断网主机升级/回滚仍未通过；生产 K8S、真实云 PostgreSQL、安全发行审查也未完成。请勿将 RC 用作生产稳定性承诺。
- PostgreSQL 14 已结束上游支持，界面保留升级提示。长期数据需按部署文档设置保留期与备份；PG18 镜像扫描匹配仍在[发行依赖审计](GO_DEPENDENCY_AUDIT.md)中跟踪。

## 产物

- `DbSphere-go-offline-0.6.0-rc.1-linux-amd64.run` 与同名 `.sha256`：Go 与 PostgreSQL 18 一键离线容器安装包。
- `DbSphere-go-0.6.0-rc.1-linux-amd64` 与同名 `.sha256`：静态 Go 服务端，需外部 PostgreSQL 18；按[部署说明](OFFLINE_DEPLOYMENT.md)配置环境变量。
- `DbSphere-go-licenses-0.6.0-rc.1.tar.gz` 与同名 `.sha256`：原生 Go 二进制随附的 DbSphere 和第三方许可证文本。

下一正式里程碑为 **V0.6.0**；完成对应任务后再发布 V0.7.0，直至 V1.0.0。
