# 托管 PostgreSQL 接入

在「资产」中选择托管 PostgreSQL，填写数据库端点和专用监控账号。主库和只读副本按各自的数据库端点登记，可使用相同的集群名称。资产详情的「采集能力与缺口」展示采集适配器、当前数据库角色、TLS 身份校验和只读状态。历史样本保留每次采集的能力快照。

## 账号和证书

为 DbSphere 创建专用 `LOGIN` 角色，授予目标数据库的 `CONNECT` 和 PostgreSQL 的 `pg_monitor`，不授予业务表权限、写入权限或管理权限。采集器设置 `default_transaction_read_only=on`，采样时读取服务端实际报告的事务只读状态。查询指纹要求目标数据库已安装并启用 `pg_stat_statements`。有限权限和未安装扩展在能力面板中单独显示。

录入资产时选择「验证服务器证书」或设置 API 的 `sslMode` 为 `require`。该设置同时校验证书链和目标主机名，连接失败时不会降级到明文。原生服务可通过 `DBSPHERE_POSTGRES_CA_FILE` 读取部署方提供的 PEM CA 文件。容器部署可将同一 PEM 文件用 `base64 -w0` 编码，设置到 `.env.go` 的 `DBSPHERE_POSTGRES_CA_PEM_BASE64` 后重建应用容器。AWS RDS 专用环境变量 `DBSPHERE_RDS_CA_FILE` 和 `DBSPHERE_RDS_CA_PEM_BASE64` 优先于通用配置。每组仅配置一种形式。容器建议使用实例所在区域的 CA 文件。环境变量为空时使用系统信任链。[AWS RDS PostgreSQL TLS 文档](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html)

## 能力矩阵

| 能力 | 验收范围 |
| --- | --- |
| PostgreSQL 通用采集 | 支持与自建实例相同的数据库侧采集。按 PostgreSQL 版本、扩展和权限报告有效指标。 |
| 托管接入 | 通过数据库协议识别主库、物理只读副本、TLS 和会话只读状态，保留采样证据。 |
| AWS RDS for PostgreSQL | 适配器识别公开 RDS 域名并强制身份校验。连接成功后可报告 `rds.force_ssl` 厂商特征。厂商实际实例的验证结果以正式验收记录为准。 |
| Linux 或 Kubernetes 信号 | 托管数据库端点提供数据库侧观测。操作系统与 Kubernetes 侧信号需要对应数据源。 |

## 运行验收

Go 采集契约在 `pkg/collectoradapter`。`Adapter.Collect` 返回指标和 `Report`，其中每个信号都声明数据源、状态和缺失原因。TLS 校验失败和连接中断经过原有采集失败事件路径，连续恢复时更新同一个事件 ID。

在可访问专用测试实例的环境下运行 `go test ./internal/collector -run TestLiveManagedPostgresAdapter -v`。在测试进程的环境中配置以下变量。测试按当前配置读取数据库状态，不打印连接串。

| 变量 | 内容 |
| --- | --- |
| `DBSPHERE_TEST_MANAGED_PROFILE` | `managed-postgresql` 或 `aws-rds-postgresql` |
| `DBSPHERE_TEST_MANAGED_PRIMARY_URL` | 专用 `pg_monitor` 账号连接主库的 PostgreSQL URL |
| `DBSPHERE_TEST_MANAGED_REPLICA_URL` | 专用 `pg_monitor` 账号连接活动物理副本的 URL |
| `DBSPHERE_TEST_MANAGED_LIMITED_URL` | 仅有 `CONNECT`、缺少 `pg_read_all_stats` 的账号连接主库的 URL |
| `DBSPHERE_TEST_MANAGED_UNAVAILABLE_URL` | 同一测试服务的不可达端点或证书主机名不匹配的地址 |

同时配置该测试环境所需的 CA。用控制台注册测试资产，通过可恢复的网络隔离或测试代理使连接中断。触发采集，记录采样与数据库不可达事件 ID。恢复网络并继续采集，核对原事件的恢复状态和关联的采样。如果使用测试代理，仍应使用证书中的数据库主机名建立 TLS 连接。
