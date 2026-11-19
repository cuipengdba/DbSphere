# V1.0.0 正式版发布验收

> 2026-11-19。V1.0.0 是 V0.1–V0.9 逐版发布后的 PostgreSQL 专项正式版。这里列出本版实际可交付范围和证据；未验收环境不会因版本号变成已支持。

## 正式交付范围

- **PG14–18 与环境**：对 Linux 容器中的五个 PostgreSQL 大版本和四节点 kind 中的五个版本进行真实只读纳管；Linux Patroni 双节点五版演练。目标数据库可在 Linux 或 Kubernetes 中运行。实际云厂商、生产 Operator 与裸机发行版组合仍见[支持矩阵](SUPPORT_MATRIX.md)的空白项。PG14 在页面提示上游 EOL。
- **完整工程**：Go 服务端、静态前端与 HTTP API 构成 B/S 系统。Linux amd64 静态 Go 程序和一键离线容器包包含在同一发布；容器包内置独立 PG18 长期监控存储，目标机无需 Node.js 或 Go，需预装 Docker Engine/Compose。
- **一个问题的全链路**：同一事件 ID 展示 PG 原始样本、告警、SQL 指纹/锁链/等待、可用的 Linux/K8S/Patroni/PgBouncer、应用 Trace、证据化候选、人工处理与恢复；可导出脱敏复盘报告。缺失数据标为未知，根因候选不是未经证实的结论。
- **无模型主干**：纳管、自动采集、巡检、告警、规则诊断、工单和报告无需 AI。可选模型仅分析同事件脱敏证据，受预算、引用与审计约束，不执行生产变更。提前预测故障的 AI 预警未纳入本版。
- **受控操作**：管理员申请、独立审批账号决定、管理员一次性执行只读采样；PG18 保存同一事件内的工单、采样及角色审计。没有组织/租户、个人账号或防篡改审计。

## 验收证据

| 范围 | 已有证据 | V1.0 最终检查 |
| --- | --- | --- |
| PG14–18 Linux/K8S | [五版兼容](SUPPORT_MATRIX.md)、[多节点 kind](KIND_MATRIX_ACCEPTANCE.md)、[Patroni](PATRONI_ACCEPTANCE.md) | 最终提交的 Linux CI 全部通过 |
| 同一事件根因与恢复 | [真实锁链](V0.6_RELEASE_GATE.md)、[应用 OOM](KIND_ACCEPTANCE.md)、[PG18 复制延迟](REPLICATION_DRILL.md)、[WAL 归档](WAL_ARCHIVE_DRILL.md)等 | 最终 Go 实库与故障演练回归 |
| PG18 长期存储 | 分区、保留期、12 万样本/5000 事件清理、备份恢复和独立 PVC [演练](KIND_PG18_RESTORE_ACCEPTANCE.md) | 当前版本安装与升级/回滚中同一记录保留 |
| 一键断网交付 | Docker 27 无外网嵌套守护进程的安装、重复安装与健康检查 | V0.9.0 → V1.0.0 → V0.9.0 三阶段在无外网守护进程里保持同一 PG18 数据记录；校验六个发行附件 |
| 安全与许可证 | [Go 依赖审计](GO_DEPENDENCY_AUDIT.md)、[V1.0 镜像风险记录](V1_SECURITY_REVIEW.md) | 固定镜像摘要，SBOM/扫描留档；Go 无 Critical/High，PG18 未知 Critical 阻断 |

最终 CI 运行号、六个附件及 SHA-256 以[正式发布页](https://github.com/cuipengdba/DbSphere/releases/tag/v1.0.0)为准；发布前须在同一最终提交的 CI 中逐项验证本表。

## 已知限制与后续

实际云厂商托管 PG、生产 K8S/Operator、Linux 裸机发行版、arm64、百万级资产/事件和多副本采集租约尚未验收；本版仅交付表中有证据的组合。应用全链路依赖 OTel，Linux/K8S/连接池/PVC 等信号分别依赖对应可用数据源。个人账号、完整 RBAC、审计防篡改、AI 提前预警、跨语言 SDK 与所有连接池/Pod/PVC 身份映射继续保留为未完成任务。

PG18 官方 Bookworm 镜像有已记录的高危扫描匹配；[安全审查](V1_SECURITY_REVIEW.md)列出 Critical 解释和剩余风险，不宣称零漏洞或安全认证。容器离线包已在无外网 Docker 守护进程 CI 中测试，物理断网服务器和生产故障演练仍需部署方在自己的环境验收。SHA-256 不是发行签名。

[开发任务清单](DEVELOPMENT_TASKS.md)中的未完成项保持未勾选，后续按实际需求继续开发，不能以 V1.0 版本号视作已交付。
