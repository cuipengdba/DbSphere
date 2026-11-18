# PostgreSQL autovacuum 关闭故障演练

> 2026-11-19 开发版记录。验证真实 PG18 参数关闭的监控、告警、根因候选与恢复；不将参数风险等同于已证明的业务查询退化。

隔离 `test/replication-drill/compose.yaml` 新增独立的 `maintenance` PostgreSQL 18 容器，与已有主备故障夹具、DbSphere Go 服务和长期监控 PostgreSQL 18 分离。测试容器创建专用 `pg_monitor` 账号，DbSphere 只读采集 `pg_settings`；脚本在夹具容器中以超级用户临时 `ALTER SYSTEM` 设置 `autovacuum=off`，随后恢复为 `on`。这些配置写入只作用于一次性测试库，不经过 DbSphere API。

最初在已有主库上执行时，本机 PostgreSQL 进程在参数重载期间重新初始化，测试未形成完整结果，清理分支当时也无法连接。待主库恢复后已手动确认 `autovacuum=on`；为避免让主备/WAL 演练状态影响维护场景，测试改用独立、全新数据卷的 `maintenance` 容器。该调整后的本机脚本通过，不能由首次进程重启推断参数本身有缺陷。

成功演练的基线为 `autovacuum=on`。关闭后真实采样 `bb51114e-be41-4eda-acfc-a57f7ab06a80` 产生 `pg_autovacuum_disabled` 告警和 `autovacuum_disabled` 候选，二者与原始样本、Markdown 报告共用维护事件 `ffd4e827-8719-45b5-b8a4-2f5d2657cc3f`。候选置信度保持 **low**，因为没有实际死元组积压或业务慢查询证据。恢复参数后，成功样本 `6aa7fa7a-6c0a-4382-a386-4f66201bf69e` 在同一事件中记录告警恢复，事件状态变为 `resolved`。DbSphere 长期存储仍为独立 PostgreSQL 18。

复现时先按[复制故障演练](REPLICATION_DRILL.md)启动夹具，再运行：

```bash
node scripts/autovacuum-drill-acceptance.mjs http://127.0.0.1:18092 /tmp/dbsphere-replication-credentials.json
```

[Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44360581324) 的隔离主备作业已复做 autovacuum 关闭与同事件恢复，整套 29 个作业均通过。真实业务性能影响、长时间关闭后的表膨胀、生产参数变更审批与自动修复仍未验收。
