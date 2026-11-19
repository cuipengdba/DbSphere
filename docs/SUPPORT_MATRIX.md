# V0.7.0 支持与验收矩阵

> 截至 2026-11-19。这是已执行测试的边界，不是 V1.0 正式支持声明。长期监控存储在以下测试中均为独立 PostgreSQL 18，业务目标 PostgreSQL 版本另列。

| 目标环境 | PG14 | PG15 | PG16 | PG17 | PG18 | 已验证内容与限制 |
| --- | --- | --- | --- | --- | --- | --- |
| Linux 容器中的 PostgreSQL | CI | CI | CI | CI | CI | 真实连接、数据库样本、角色识别与事件路径；[五版兼容 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44363714543)。尚未覆盖各发行版的裸机运维差异 |
| Kubernetes 中的 PostgreSQL | CI | CI | CI | CI | CI | 四节点 kind 的 Pod/Service/Node 与 PG18 PVC 持久化；[多节点矩阵](KIND_MATRIX_ACCEPTANCE.md)。生产集群、Operator、持续负载未验收 |
| Linux Patroni 双节点 | CI | CI | CI | CI | CI | 无 leader、选主、原节点重入及同一事件 ID 恢复；[Patroni 验收](PATRONI_ACCEPTANCE.md)。测试 DCS 为单节点，不代表生产高可用 |
| Kubernetes Patroni | 本机 | — | — | — | — | 单节点 kind 中 PG14 双节点故障闭环；[本机演练](PATRONI_ACCEPTANCE.md#kubernetes-本机故障演练)。多节点 DCS/Operator/PVC 组合未验收 |
| 托管模式的数据库协议路径 | CI | CI | CI | CI | CI | 仅数据库侧信号与缺口显示；[能力说明](MANAGED_PG.md)。CI 使用本地 PostgreSQL 容器，不等于云服务验收 |
| 实际云厂商托管 PostgreSQL | — | — | — | — | — | 尚无测试实例；TLS、角色权限、系统视图限制及供应商连接策略待验收 |

`CI` 表示独立 Ubuntu runner 的容器或 kind 测试，`本机` 表示本机隔离测试集群，`—` 表示未验收。[PostgreSQL 14 已结束上游支持](https://www.postgresql.org/about/news/postgresql-187-1712-1616-1520-1425-and-19-rc-2-released-3442/)，页面持续显示升级提示；DbSphere 在兼容矩阵中保留 PG14 测试，但生产使用应规划升级。

Go 后端与静态前端已在 Linux 容器和本机开发环境运行。Go 离线包面向 **Linux x86_64**，目标机仍需 Docker Engine 与 Compose v2；Node.js 不参与 Go 包的运行。Linux arm64、跨发行版裸机和生产断网升级/恢复尚无正式发布验收。详见[离线部署](OFFLINE_DEPLOYMENT.md)及[V1.0 发布任务](DEVELOPMENT_TASKS.md#v10正式版发布门槛)。

一个问题的全链路能力以同一事件 ID 的原始样本、告警、Trace（如已接入）、基础设施或 Patroni 信号、根因候选及恢复证据为准。缺少应用 Trace、K8S API、主机 exporter 或云服务视图时，页面显示缺口；时间相关的候选需人工复核，不能视为已证实根因。

V0.7.0 的应用链路支持范围与已知限制见[Trace 发布验收](V0.7_RELEASE_GATE.md)。官方 Go SDK 经 Collector JSON 转发与本机 PG18 存储已验证；其他语言 SDK、生产跨服务采样覆盖和自定义 Query ID 自动埋点尚未验收。
