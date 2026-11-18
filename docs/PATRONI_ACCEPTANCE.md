# Patroni 只读监控与本机验收

> 2026-11-18 开发版记录。当前实现是可选信号；未填写 Patroni 地址的 PostgreSQL 资产仍按原方式采集。本记录不代表 PG14–18 × Patroni 全矩阵、生产高可用或 V1.0 发布验收。

## 接入与边界

在资产表单填写 Patroni REST 根地址，例如 `https://reader:密码@patroni.example:8008`。DbSphere 只调用 `GET /cluster`，以加密字段保存接入地址，资产 API 仅返回是否配置。采集限制为 5 秒、1 MiB 响应、100 个成员；不跟随重定向，也不使用环境代理。页面显示成员角色、状态、节点地址、timeline 和可用的字节级复制延迟；Patroni 返回 `"unknown"` 时保持未知，不填 0。Patroni 采集失败单列为遥测告警，**不会**据此推断数据库宕机；持续 30 秒的 leader 数量异常才触发默认可用性告警和根因候选。候选只确认观察到的拓扑异常，不把 DCS、网络或节点故障当成已证实原因。[Patroni `/cluster` 协议说明](https://patroni.readthedocs.io/en/latest/rest_api.html)。

Go 后端没有调用切主、重启、重建副本等 Patroni 写接口。HTTP 明文地址仅适合受信测试网络；生产接入应使用受信证书的 HTTPS，并让 DbSphere 到 REST API 的网络路径可达。此功能无 AI 模型也可工作。

## 实际验收

本机隔离 Docker 网络运行 PostgreSQL 18.6 + Patroni 4.1.5 双节点、单节点 etcd；测试用 Go 后端使用独立 PostgreSQL 18 监控库存储，`/readyz` 返回 `ready/postgresql18`。被监控数据库使用具 `pg_monitor` 权限、无超级用户权限的账号。初始真实样本 `d317ff81-a37b-4fda-a149-71888bacc7e4` 同时读到 PG18、一个 `running` leader、一个 `streaming` replica 和复制延迟。Playwright 浏览器在资产详情显示完整拓扑，控制台无错误。截图保存在本机 `H:\CodexLS\outputs\DbSphere-patroni-local.png`。

首次故障演练强制停止原 leader `patroni1`，连续采样读到 3 次 `leaderCount=0`，随后 `patroni2` 自动晋升；旧节点重入后成功样本 `7cb0a3c7-16f7-4c60-b238-028649358f9a` 恢复。主可用性事件 `d61fb82e-d4e2-42a1-872e-428a925d03a1` 汇集 PG 无法连接、Patroni 无 leader、两条告警、`patroni_leader_anomaly` 候选与 `supported_by` 证据关系，并以同一 ID 关闭。复核发现旧节点恢复时的 PostgreSQL `57P03` 被误分为另一个遥测事件，因此修正了连接相关 SQLSTATE 的分类：`08xxx`、`53300`、`57P01`、`57P02`、`57P03` 统一归入可用性；认证和权限错误继续归入遥测。[PostgreSQL SQLSTATE 定义](https://www.postgresql.org/docs/current/errcodes-appendix.html)。

更新 Go 后端后再次强制停止当前 leader `patroni2`。连续采样读到 2 次 `leaderCount=0`，`patroni1` 晋升；旧节点恢复为 `streaming` replica，成功样本 `514a7674-da1e-439c-ad8f-5b45700fe72a` 关闭**唯一**事件 `7410d94d-0e99-45dd-ad08-8f961c2952df`。该事件包含 `pg_unavailable`、`patroni_leader_invalid` 告警、`patroni_leader_anomaly` 候选、原始 Patroni/PG 样本、`hypothesizes` 与 `supported_by` 边；没有再生成同故障遥测事件。测试将 leader 规则持续时间暂设为 0 秒以捕获短暂选主窗口，结束后恢复默认 30 秒，不能据此声称默认持续时间在该短窗口必然告警。浏览器事件页显示同一事件 ID、候选证据、无 leader 时间线及恢复样本，控制台无错误。脱敏运行结果与截图在本机 `H:\CodexLS\outputs\DbSphere-patroni-failover-fixed.json`、`DbSphere-patroni-events-fixed.json`、`DbSphere-patroni-event.png`。

本轮最终源码执行 `go test ./... -count=1`（连接隔离 PG18 测试库）、`go vet ./...` 和 `node --check public/app.js` 均通过。Go Linux 静态镜像还在本机 kind 的既有 PG18/K8S 资产上取得新成功样本，见[本机 Kubernetes 验收](KIND_ACCEPTANCE.md)。

## 尚待验收

本次只覆盖 PG18 双节点、单节点 DCS、同一台开发机和人工故障注入。PG14–17 Patroni 组合、三节点 DCS、Kubernetes Operator/StatefulSet、TLS/Basic Auth 生产证书、跨节点网络分区、长期负载、自动发现与真实业务 Trace 尚未测试。DbSphere 仍是只读监控，不承担 Patroni 的自动选主或故障切换。V1.0 任务保持未完成，见[开发任务](DEVELOPMENT_TASKS.md)。
