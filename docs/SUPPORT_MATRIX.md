# DbSphere V1.0 支持与验收矩阵

> 本矩阵区分已通过的版本与部署路径、依赖额外信号的能力，以及未完成的环境验收。监控数据始终写入独立 PostgreSQL 18；下表 PG14–18 指被监控业务数据库。

| 目标环境 | PG14 | PG15 | PG16 | PG17 | PG18 | 验收范围 |
| --- | --- | --- | --- | --- | --- | --- |
| Linux 容器中的单实例 PostgreSQL | CI | CI | CI | CI | CI | 真实只读连接、诊断采样、版本与角色识别、事件；[五版兼容 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44363714543) |
| Linux PgBouncer → PostgreSQL | CI | CI | CI | CI | CI | 真实 HTTP 请求经池发生 PG 锁等待，同一事件关联精确后端 PID、阻塞会话、根因候选与恢复；[五版故障链演练](TRACE_POOLER_INCIDENT.md)。[业务代理地址](TRACE_POOLER_ROUTE.md)可与统计管理端口不同；本机独立端口锁故障链已通过，五版 CI 作业也包含该场景。生产代理与 TLS 配置未验收 |
| Kubernetes 中的单实例 PostgreSQL | CI | CI | CI | CI | CI | 四节点 kind 的 Pod/Service/Node 关联、故障证据及独立 PG18 PVC；[多节点矩阵](KIND_MATRIX_ACCEPTANCE.md)。PG18 另通过[真实应用连接失败→Pod OOM→同事件恢复 CI](KIND_APP_OOM_ACCEPTANCE.md)，以及[官方 Go SDK 经 Collector 的同事件 OOM CI](SDK_KIND_OOM_ACCEPTANCE.md)；其他大版本的 OOM 组合未验收 |
| Linux Patroni 双节点 | CI | CI | CI | CI | CI | 只读拓扑、失去 leader、选主、原节点重入及同一事件 ID 的恢复；[Patroni 验收](PATRONI_ACCEPTANCE.md) |
| Kubernetes Patroni | 本机 PG14 | — | — | — | — | 单节点 kind 的 PG14 故障闭环；多节点 DCS、Operator 和不同 PG 大版本组合未验收 |
| 托管模式的数据库协议路径 | CI | CI | CI | CI | CI | 使用本地 PostgreSQL 容器验证数据库侧采集与缺口显示；不代表实际云服务验收 |
| 实际云厂商托管 PostgreSQL | — | — | — | — | — | 尚无测试实例；TLS、角色权限、系统视图限制及厂商连接策略未验收 |

`CI` 表示独立 Ubuntu Linux runner 的真实数据库容器或 kind 测试；`本机` 表示隔离本机测试。`—` 表示未验收，不能据此推断支持。Linux 裸机/虚机上的 PostgreSQL 使用相同数据库协议，但不同发行版、文件系统、服务管理和生产持续负载未完成独立验收。生产 Kubernetes 集群、Operator、跨区域和持续高负载不在已测矩阵内。

[PostgreSQL 14 已结束上游支持](https://www.postgresql.org/about/news/postgresql-187-1712-1616-1520-1425-and-19-rc-2-released-3442/)；DbSphere 仍回归其监控兼容性，并在资产界面提示升级风险。

## 部署与依赖

| 交付方式 | 已验证平台 | 运行时依赖 | 监控存储 |
| --- | --- | --- | --- |
| 一键离线容器包 | Linux x86_64、Docker Engine 27 与 Compose v2 的独立 CI；含无外网嵌套守护进程验收 | 目标机预装 Docker Engine/Compose；安装过程无需 Node.js、Go、包管理器或镜像仓库 | 包内 PostgreSQL 18 镜像，独立持久卷；可改用已有独立 PG18 |
| 静态 Go 程序 | Linux amd64 CI，`CGO_ENABLED=0` | 独立 PG18、服务管理与 HTTPS 入口由部署者提供 | 外部独立 PG18 |
| 源码在线 Compose | Ubuntu Linux CI | Docker Engine/Compose 与构建时网络 | 独立 PG18 容器或外部 PG18 |

Linux arm64 尚未构建与验收，不属于 V1.0 交付范围。当前只支持一个启用自动采集的 DbSphere 服务副本；采集租约和高可用控制台尚未完成。离线安装的“断网”指安装器及 Docker 守护进程均无法访问外网时使用本地镜像；它不代替物理服务器的离线升级、硬件故障或生产演练。

V1.0.8 的管理员系统状态页显示当前 Go 进程的自动采集循环、批次耗时及执行结果。状态在进程重启后重新计数；命中每轮 100 个资产的选择上限不代表测得的积压量，见[采集进程状态](COLLECTOR_RUNTIME.md)。

## 一个问题的全链路边界

一个事件 ID 汇集原始 PG 样本、告警、SQL/锁与等待、可用的 Linux/K8S/Patroni/PgBouncer 信号、应用 Trace、根因候选、人工决定和恢复证据。数据库连接串即可运行基础监控和无模型规则诊断；主机需要 node_exporter，K8S 需要只读 API 与相应指标，应用链路需要接入 OTel。K8S 资产的应用数据库 Span 可在 Pod UID 经只读 API 核对后连接应用 Pod、Node 与 PVC 引用；V1.0.6 另以定向 API GET 记录 PVC 当时可读到的阶段，未核对时显示未知。这不证明卷已挂载或造成故障，见[应用身份边界](TRACE_K8S_POD_IDENTITY.md)。缺少信号时明确显示未知或未定位，候选原因需要人工复核。可选 AI 只分析脱敏证据，不接入模型仍可纳管、监控、告警、巡检和生成报告。V1.0.7 可显示应用声明的采样概率和未来时间戳异常，并限制时间异常 Span 的跨层根因关联；这不是实际覆盖率或时钟偏移测量，见[Trace 时间可信度](TRACE_TIME_COVERAGE.md)。不同语言 SDK、跨服务采样覆盖及完整生产身份映射尚未验收。
