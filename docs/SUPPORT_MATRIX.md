# DbSphere 支持与验收矩阵

> 监控数据写入独立 PostgreSQL 18。下表 PG14–19 指被监控业务数据库。

| 目标环境 | PG14 | PG15 | PG16 | PG17 | PG18 | PG19 | 验收范围 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Linux 容器中的单实例 PostgreSQL | CI | CI | CI | CI | CI | CI | 真实只读连接、诊断采样、角色识别和同事件锁故障演练。 |
| Linux PgBouncer → PostgreSQL | CI | CI | CI | CI | CI | CI | 真实 HTTP 请求经池发生 PG 锁等待，同一事件关联后端 PID、阻塞会话及恢复。[故障链演练](TRACE_POOLER_INCIDENT.md) · [业务代理地址](TRACE_POOLER_ROUTE.md) |
| Kubernetes 中的单实例 PostgreSQL | CI | CI | CI | CI | CI | CI | 四节点 kind 的 Pod/Service/Node 关联及独立 PG18 PVC。PG18 另通过真实应用和 Go SDK 的 Pod OOM 事件演练。[多节点矩阵](KIND_MATRIX_ACCEPTANCE.md) · [应用 OOM](KIND_APP_OOM_ACCEPTANCE.md) · [Go SDK OOM](SDK_KIND_OOM_ACCEPTANCE.md) |
| Kubernetes CloudNativePG 1.30.2 | — | — | — | — | CI | — | Kubernetes 1.35.8 与 PostgreSQL 18.7，验证读写 Service、只读副本、故障切换、原事件恢复和隔离逻辑恢复。 [Operator 版本说明](https://cloudnative-pg.io/releases/cloudnative-pg-1-30.2-released/) |
| Linux Patroni 双节点 | CI | CI | CI | CI | CI | — | 只读拓扑、选主、原节点重入及同事件恢复。[Patroni 验收](PATRONI_ACCEPTANCE.md) |
| Kubernetes Patroni | 本机 | — | — | — | — | — | 单节点 kind 的 PG14 故障闭环。 |
| 托管模式的数据库协议路径 | CI | CI | CI | CI | CI、本机专项 | — | 本地 PostgreSQL 容器的数据库侧采集与缺口显示。PG18 本机专项包含主库和物理只读副本、CA 校验、受限权限和原事件恢复。[托管 PostgreSQL 接入](MANAGED_POSTGRESQL.md) |
| 实际云厂商托管 PostgreSQL | — | — | — | — | — | — | AWS RDS 适配器可识别厂商特征，实云 TLS、角色权限、系统视图限制及连接故障恢复待验收。 |

`CI` 表示独立 Ubuntu Linux runner 的真实数据库容器或 kind 测试；`本机` 表示隔离本机测试。`—` 表示未验收，不能据此推断支持。Linux 裸机/虚机上的 PostgreSQL 使用相同数据库协议，但不同发行版、文件系统、服务管理和生产持续负载未完成独立验收。生产 Kubernetes 集群、Operator、跨区域和持续高负载不在已测矩阵内。

资产页按样本展示等待、I/O、检查点、WAL、复制和 Top SQL 的实际采集状态。PG16 起提供 `pg_stat_io`，PG17 起从 `pg_stat_checkpointer` 读取检查点统计。等待明细与跨账号 SQL 指纹需要可用的 `pg_read_all_stats` 权限，后者还需目标数据库安装 `pg_stat_statements`。DbSphere 继续回归已结束上游支持的 PostgreSQL 14，并在资产界面标注其生命周期。[PostgreSQL 版本策略](https://www.postgresql.org/support/versioning/) · [监控系统视图](https://www.postgresql.org/docs/19/monitoring-stats.html)

## 瀚高安全版 4.5.9

支持在 Linux 上纳管瀚高安全版 V4.5.9，兼容内核 12.7，实测构建修订 `63e6077`、x86_64、CentOS 7.9。资产页显示数据库产品与采集能力。监控历史仍保存在独立 PostgreSQL 18。[接入、权限与指标范围](HIGHGO.md)

## 达梦 DM8

已在 Linux x86_64 的 DM8 构建 `03134284604-20260707-335949-20228` 验收单实例和两节点 Data Watch 实时主备。事务锁、归档、守护和回放信号按授权读取，从同一事件查看原始与恢复样本。DbSphere 内置 Go 探针，现场提供 DIsql。 [接入及权限](DM8.md)

## 金仓 KingbaseES V9

Linux x86_64 的 PostgreSQL 兼容模式，验收介质 `V009R001C010B0004`。实测两节点流复制积压、回放追平和备库提升。会话等待历史、归档与 Top SQL 指纹按目标库的统计配置和权限提供。 [金仓接入与权限](KINGBASE.md)

## Redis 6.2、7.2 和 8.4

独立只读 ACL 用户可读取 Redis 单节点、主从、Sentinel 与 Cluster。覆盖 OPS、缓存窗口、慢命令、持久化及拓扑，按可用权限显示缺口。 [Redis 接入与指标](REDIS.md)

| 兼容项 | 新增版本 | 已验收范围 |
| --- | --- | --- |
| Redis OSS 6.2 | v3.11.0 | Redis 6.2.24 单节点完成认证修正与原资产复检。三主节点 Cluster 验证 `CLUSTER SLOTS` 权限缺口及恢复后的全槽位采集。 |

## etcd 3.5 与 3.6

已验证 Linux etcd 3.5.36 单节点和 etcd 3.6.17 三成员的原生指标。使用双向 TLS 查看成员及 Raft 状态，Kubernetes v1.35.8 API Server 以仅包含指标读取权限的账号采集同窗统计。 [etcd 接入与指标](ETCD.md)

## MySQL 8.4 LTS

读取 InnoDB 缓冲池、等待和事务、SQL 指纹和原生复制通道及工作线程，支持文件慢日志。Linux x86_64 验收使用 MySQL Community Server 8.4.13，普通实例和两节点异步复制均保留原始与恢复证据。 [接入与权限](MYSQL_PREVIEW.md)

## 日志持续接入

| 兼容项 | 首次交付 | 格式与权限 |
| --- | --- | --- |
| PostgreSQL 日志持续接入 | v3.15.0 | JSON Lines、CSV 或带日志前缀的文本。发送端需要日志文件读取权限和绑定批次的来源令牌。 |
| MySQL 8.4 FILE 慢日志持续接入 | v3.15.0 | MySQL FILE 慢日志。发送端需要慢日志文件读取权限和绑定批次的来源令牌。 |

[配置与使用](LOG_ANALYSIS.md#持续接入)

## 部署与依赖

混合采集负载记录纳管资产与物理实例的数量。100 个健康资产覆盖 PostgreSQL 14、16、18、MySQL 8.4、Redis OSS 7.2 和 etcd 3.6，另外观察认证失败与端点不可达。达梦、金仓、瀚高按各节的实库构建和拓扑验收。长期历史压测用 300 个资产、90 天合成样本，另验证 PostgreSQL 18 备份和独立恢复。 [规模采集](COLLECTOR_LOAD_ACCEPTANCE.md) · [历史留存](HISTORY_STORAGE.md)

| 交付方式 | 已验证平台 | 运行时依赖 | 监控存储 |
| --- | --- | --- | --- |
| 一键离线容器包 | Linux amd64、arm64 实机架构 CI，含无外网的 Docker Engine 27 与 Compose v2 升级回退 | 目标机预装 Docker Engine/Compose | 包内 PostgreSQL 18 镜像，独立持久卷；可改用已有独立 PG18 |
| 静态 Go 程序 | Linux amd64、arm64 CI，`CGO_ENABLED=0` | 已可访问的独立 PG18、服务管理与 HTTPS 入口 | 外部独立 PG18 |
| 原生 systemd 离线包 | Linux amd64、arm64 CI，含启用开机启动、重启、原资产和事件保留 | systemd、Bash 与基础 Linux 工具 | 外部独立 PG18 |
| 源码在线 Compose | Ubuntu Linux CI | Docker Engine/Compose 与构建时网络 | 独立 PG18 容器或外部 PG18 |

标准与增强离线包均通过 Linux amd64、arm64 安装验收。标准包的升级和回滚沿用 PG18 监控数据。双副本模式使用共享采集租约和同一 PG18 写入端点，见[离线部署手册](OFFLINE_DEPLOYMENT.md#双副本与备份)。监控目标和外部存储的可达性由部署网络决定。

管理员系统状态页显示当前 Go 进程的自动采集循环、批次耗时及执行结果。状态在进程重启后重新计数；命中每轮 100 个资产的选择上限不代表测得的积压量，见[采集进程状态](COLLECTOR_RUNTIME.md)。

## 一个问题的全链路边界

一个事件 ID 汇集原始 PG 样本、告警、SQL/锁与等待、可用的 Linux/K8S/Patroni/PgBouncer 信号、应用 Trace、根因候选、人工决定和恢复证据。数据库连接串即可运行基础监控和无模型规则诊断；主机需要 node_exporter，K8S 需要只读 API 与相应指标，应用链路需要接入 OTel。K8S 资产的应用数据库 Span 可在 Pod UID 经只读 API 核对后连接应用 Pod、Node 与 PVC 引用；另以定向 API GET 记录 PVC 当时可读到的阶段，未核对时显示未知。这不证明卷已挂载或造成故障，见[应用身份边界](TRACE_K8S_POD_IDENTITY.md)。缺少信号时明确显示未知或未定位，候选原因需要人工复核。可选 AI 只分析脱敏证据，不接入模型仍可纳管、监控、告警、巡检和生成报告。事件证据可显示应用声明的采样概率和未来时间戳异常，并限制时间异常 Span 的跨层根因关联；这不是实际覆盖率或时钟偏移测量，见[Trace 时间可信度](TRACE_TIME_COVERAGE.md)。不同语言 SDK、跨服务采样覆盖及完整生产身份映射尚未验收。

## 随版本维护矩阵

发布前核对首页和详细矩阵。新增纳管数据源、兼容版本或部署模式时，为新增范围单列兼容项，保留已有条目，并在 GitHub Release 说明中链接对应条目。兼容项记录首次交付版本、版本或模式、支持能力、权限要求及实测范围。验收结果说明使用的真实部署或测试夹具。
