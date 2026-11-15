# 开发状态与验证记录

> 更新于 2026-11-15。此文只记录已实际实现或测试的内容，不代表版本发布。

## 当前阶段

**V0.1 开发预览，V0.2 K8S 采集起步**。已实现 B/S 服务端与浏览器页面、管理员登录和 CSRF 校验、PostgreSQL 资产增删改查、凭据加密、基础采集、SQLite 持久化、采集失败与恢复共用一个事件 ID 的证据记录、Linux/K8S 运行位置字段。K8S 预览适配器可按手工映射读取 Pod 状态、容器重启/OOMKilled、PVC 与事件；当 PG 连接失败与近期 Pod OOMKilled 同时出现时，在**同一个事件 ID** 中给出中等置信度、可查看原始证据的根因候选。

当前尚未完成 V0.1 的 K8S PostgreSQL 实际纳管演练、Linux 主机原生部署验收和版本发布；K8S 自动发现/Node 指标、应用 Trace 及通用跨层根因引擎属后续版本任务。当前只支持“采集连接失败 + 近期 Pod OOMKilled”的有限候选，**不能将其宣称为完整的全链路根因分析**。

## 已通过的验证

| 验证 | 结果 | 范围 |
| --- | --- | --- |
| `pnpm test` | 通过，3 项 | 登录、CSRF、资产编辑、凭据不外泄、单事件归并、恢复与证据持久化、无关 OOM 不误判、PG14 EOL 标识 |
| K8S 适配器单元测试 | 通过 | 按 Pod UID 限定事件，读取容器 OOMKilled、PVC 和事件；尚未接入真实集群 |
| Linux 容器构建与启动 | 通过 | `docker build`；容器 `/healthz` 返回 `{"status":"ok"}` |
| PostgreSQL 14 | 通过 | 14.24，`pg_monitor` 账号读取版本/角色/连接/事务/容量/复制基础指标 |
| PostgreSQL 15 | 通过 | 15.20，同上 |
| PostgreSQL 16 | 通过 | 16.15，同上 |
| PostgreSQL 17 | 通过 | 17.11，同上 |
| PostgreSQL 18 | 通过 | 18.6，同上；官方容器使用 `/var/lib/postgresql/18/docker` 数据目录 |

兼容性测试使用临时 PostgreSQL 容器与 `scripts/pg-compat.mjs`，验证基础采集查询。上述结果尚不等于所有指标、扩展、Linux/K8S 运行形态或云服务兼容性通过。V1.0 需要完整支持矩阵和故障演练。

## 下一步

1. 完成 V0.1 剩余的 Linux 原生部署与 K8S PG 连接验收，整理安装脚本和端到端演练。
2. 开始 V0.2：K8S 只读发现与 Pod/Node/PVC 事件指标；Linux 主机采集及 PostgreSQL 等待/锁/WAL 指标。
3. 按 [开发任务清单](DEVELOPMENT_TASKS.md) 推进告警、巡检、SQL 性能、跨层证据化根因和应用 Trace，逐版验收。
