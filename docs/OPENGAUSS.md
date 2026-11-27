# openGauss 7.0 接入

在「资产与实例 → 纳管实例」选择「openGauss 7.0」，填写实例地址、实际数据库名和专用登录账号。批量导入使用 `runtime: opengauss`，端口默认 `5432`，密码通过凭据引用提供。验收介质为 Linux x86_64 单实例 openGauss 7.0.0，构建 `2397a86f`。支持范围见[验收矩阵](SUPPORT_MATRIX.md)。

## 账号与传输

DBA 为目标数据库建立专用账号，授予连接权限，并通过客户凭据管理设置密码。该账号只需查询固定的系统视图。跨会话统计和锁等待需要 `MONADMIN`。这是 openGauss 的监控管理权限，授权前应由 DBA 核对适用范围。

```sql
GRANT CONNECT ON DATABASE appdb TO dbsphere_monitor;
ALTER USER dbsphere_monitor WITH MONADMIN;
```

DbSphere 使用只读会话、`pg_catalog` 搜索路径和 3 秒语句超时。跨主机连接选择验证 TLS，可通过 `DBSPHERE_POSTGRES_CA_FILE` 或 `DBSPHERE_POSTGRES_CA_PEM_BASE64` 提供数据库 CA。隔离验收使用环回 TCP 的 SHA-256 口令认证。

| 指标 | 来源 | 范围 |
| --- | --- | --- |
| 会话、活动会话与有事务的会话数 | `pg_catalog.pg_stat_activity` | 当前数据库，排除采集会话。 |
| 锁等待 | `pg_catalog.pg_thread_wait_status` | 当前数据库内带锁模式的会话等待，最多保存 32 条。视图提供阻塞会话 ID 时展示已观测关系。 |
| 容量 | `pg_catalog.pg_database_size` | 当前数据库大小。 |

## 首采、复检与事件

在导入批次运行首采。会话或锁视图权限不足时，报告显示「有限」并保留已经读到的容量等指标。DBA 修正权限后对同一批次运行复检，可分别打开首采及复检原始样本。

在资产详情或实时观测打开锁等待事件。在「时间线与报告」点击「查看触发采样」核对会话 ID、锁模式和来源，再点击「返回原事件」。锁等待视图成功返回零条等待时保存恢复采样。权限撤回、查询失败或缺采期间，原等待事件保持开启。

Grafana 增强包提供 openGauss 专题和相同资产、时间窗及事件的回链。此版实库范围为 Linux 单实例。高可用拓扑、分布式部署和其他补丁构建需单独验收。
