# MySQL 8.4 LTS 接入

在「资产与实例」选择「MySQL 8.4」，填写 DbSphere 可达的实例和专用监控账号。同一主备集群使用相同集群名，每台实例独立纳管。当前数据库用于筛选 SQL 指纹和事务。连接默认验证证书与主机名，采集会话为只读，监控历史写入独立 PostgreSQL 18。

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
GRANT SELECT ON performance_schema.replication_applier_status_by_worker TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.setup_consumers TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.events_transactions_current TO 'dbsphere_monitor'@'dbsphere-host';
GRANT SELECT ON performance_schema.threads TO 'dbsphere_monitor'@'dbsphere-host';
```

## 采集与事件回放

| 能力 | 页面和样本中的证据 |
| --- | --- |
| 连接及缓冲池 | 连接、运行线程、最大连接、客户端语句速率、缓冲池命中率、脏页和待处理 I/O。命中率由同一实例相邻有效采样计算。 |
| InnoDB 锁与事务 | 观测等待线程、阻塞线程、对象和锁模式。启用 `events_transactions_current` 后可看活跃事务。成功采样确认等待解除后保留原事件的恢复证据。 |
| Top SQL | 指纹、次数、累计和平均耗时、原生 P95/P99 上界、扫描行和错误。可按当前数据库筛选。 |
| 复制 | 入站通道、源 UUID、收发线程、各工作线程、最近一次应用耗时及错误。复制通道恢复记录回到对应事件。 |
| 日志和基础设施 | 日志分析可导入 MySQL 文件慢日志并绑定实例。原文可从同窗日志进入。按需填入 Linux Exporter 或 Kubernetes Pod/PVC 映射。 |

打开「实时观测」选择 MySQL，放大图表可沿同一时段查原始样本。增强包提供 Grafana 专题。纳管 API 使用 `runtime: "mysql"`，规则分析与回放可在未配置模型时使用。 [InnoDB 锁等待表](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-data-lock-waits-table.html) · [SQL 指纹汇总](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-statement-summary-tables.html) · [复制状态](https://dev.mysql.com/doc/refman/8.4/en/performance-schema-replication-tables.html)
