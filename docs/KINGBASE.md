# 金仓 KingbaseES V9 接入

DbSphere 直接使用数据库的 PostgreSQL 兼容协议，通过 Go 静态程序采集。已验证 Linux x86_64 介质 `V009R001C010B0004`，服务端版本标识为 `KingbaseES V009R001C010`。纳管时选择「金仓 V9」并填写实际数据库名和端口。每个资产对应一个目标数据库。

## 监控账号

由数据库管理员为目标数据库创建专用账号并授权连接及读取统计视图。以 PostgreSQL 兼容模式为例，可使用以下权限。

```sql
CREATE ROLE dbsphere_monitor LOGIN;
GRANT CONNECT ON DATABASE appdb TO dbsphere_monitor;
GRANT pg_read_all_stats TO dbsphere_monitor;
```

通过部署方凭据管理设置口令。如果同一实例有多个数据库，为需要纳管的数据库分别创建资产。采集连接设为只读，并校验数据库模式和版本。在跨主机部署时选择 TLS 校验，证书按地址或域名验证。自签名 CA 可使用现有 `DBSPHERE_POSTGRES_CA_FILE` 或 `DBSPHERE_POSTGRES_CA_PEM_BASE64`。

| 信号 | 来源 | 使用范围 |
| --- | --- | --- |
| 会话、等待类别、长事务 | `sys_catalog.sys_stat_activity` | 当前数据库。长事务面板展示超过 10 秒的事务。 |
| 阻塞关系 | `sys_catalog.sys_locks` 和 `sys_catalog.sys_blocking_pids` | 当前数据库的等待会话、直接持锁会话及采样时间。每次保留至多 32 条关系。 |
| 数据库容量 | `sys_catalog.sys_database_size` | 当前数据库的数据大小。 |
| 复制观测 | `sys_catalog.sys_stat_replication`、`sys_catalog.sys_stat_wal_receiver` | 实例角色、已连接副本、可见回放差距与 WAL 接收进程。整组主备切换单独验收。 |
| Top SQL | `sys_stat_statements` | 累计执行时间、次数、平均及最大耗时，按数据库、用户和语句 ID 区分指纹。需启用 `sys_stat_statements.track` 并在目标数据库有扩展。 |

页面逐项标记权限不足、统计关闭与视图不可用。SQL 指纹采集只读取统计字段。实时趋势和历史对照使用 DbSphere 周期采样，数据保存在独立 PostgreSQL 18。厂商 KSH 历史将随原生诊断扩展单独验收。

## 查看一次锁故障

在「实时观测」选择金仓资产，切换「阻塞关系」查看故障时间窗，打开事件后可查看根因、原始采样和恢复点。将原生日志按金仓格式导入时绑定同一资产及对应的日志前缀时区。通过该事件的日志证据区选择时间窗匹配的记录并查看归档原文。

部署 Grafana 增强包时，可从实时观测或事件详情打开金仓专题并保留实例、时间窗与原事件。标准包中的实时观测、根因和原始证据可独立使用。
