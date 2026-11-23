# MySQL 8.4 LTS 只读监控预览

在「资产与实例」里选择「MySQL 8.4 · 只读预览」，填写 DbSphere 可达的 MySQL 地址、端口和专用监控账号。数据库名称用于筛选 SQL 指纹。连接默认验证服务器证书链和主机名，使用 DbSphere 服务所在系统的信任链。采集会话设置为只读，长期监控数据仍存储在独立 PostgreSQL 18。

## 账号权限

由数据库管理员创建专用账号，限定允许连接的采集端地址。按需授予下列系统表的 `SELECT` 权限。示例用实际采集端地址替换 `dbsphere-host`，并在建立账号时配置独立密码。

```sql
CREATE USER 'dbsphere_monitor'@'dbsphere-host' IDENTIFIED BY 'replace-with-a-unique-password';
GRANT SELECT ON performance_schema.global_status TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.data_lock_waits TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.data_locks TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.replication_connection_status TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.replication_applier_status TO 'dbsphere_monitor'@'dbsphere-host';
```

## 采集与事件回放

| 能力 | 页面和样本中的证据 |
| --- | --- |
| 连接 | 当前连接、运行线程、最大连接数、会话只读、TLS 状态和最近采样。 |
| InnoDB 锁等待 | 等待线程、阻塞线程、对象和锁模式。直接阻塞关系和原始样本进入事件时间线。一次成功的零等待采样将原事件标记为恢复。采集缺失和权限受限时保留原事件状态。 |
| Top SQL | 按累计耗时显示指定数据库的指纹、执行次数、平均耗时和扫描行数。采集器保存指纹及汇总数据。 |
| 复制 | 入站通道、源 UUID、接收线程和应用线程状态及最近 I/O 错误码。 |

当前预览的纳管入口是单实例页面和资产 API，API 使用 `runtime: "mysql"`。PostgreSQL 工作台、批量清单和 PostgreSQL 专项诊断维持现有范围。MySQL 事件的规则分析和回放无需接入模型。[InnoDB 锁等待表](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-data-lock-waits-table.html) · [SQL 指纹汇总](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-summary-tables.html) · [复制状态](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-replication-tables.html)
