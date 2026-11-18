# PostgreSQL WAL 归档失败故障演练

> 2026-11-19 开发版验收。此场景覆盖隔离 PG18 主库的一次真实归档失败与恢复；不代表生产归档仓库、备份可恢复性或 V1.0 全故障矩阵通过。

## 场景与隔离

`test/replication-drill/compose.yaml` 在隔离 Docker 网络运行 PG18 主备、独立 PG18 监控库和 Go 静态页面服务。主库启用 `archive_mode=on`；测试脚本先把 `archive_command` 设为 `true` 并取得只读采样基线，再临时设为 `false`、切换 WAL，等待真实 `pg_stat_archiver.failed_count` 增加。DbSphere 用 `pg_monitor` 账号只读采集；测试夹具通过本机 `docker exec` 执行故障注入和恢复，不通过 DbSphere API 修改业务库。

首次脚本运行没有形成故障：夹具将 `archive_command=true` 放在 PostgreSQL 命令行参数，`ALTER SYSTEM` 无法覆盖该高优先级参数，等待重载超时。夹具现只在命令行设置需要重启的 `archive_mode`，而把可热重载的 `archive_command` 留给测试脚本；失败时脚本的 `finally` 也恢复为 `true`。修正后本机演练通过。

## 本机结果

`pg_stat_archiver.failed_count` 从 0 增至 1 时，Go 采样 `614165b4-2f11-417d-b8ef-0d06b2c5c1a6` 触发 `pg_archive_failures` 告警；同一存储问题事件 `8dad5aff-608f-4d86-9cab-3e94bd826cff` 显示 `wal_archive_failures` 根因候选、原始样本节点和 Markdown 报告中的事件/样本 ID。恢复归档命令后 `archived_count` 增加，后续样本 `fe1f1447-bf1d-4bbe-90a0-59bc278d0f54` 检测到**新增失败数为 0**，原事件告警恢复并关闭。累积失败数在恢复时为 3，不将累积值误当成持续故障。脚本输出的目标资产为 `d39ed10d-28d5-421d-b9c4-a99dfb0029d1`。

本机复现命令（凭据文件由隔离测试夹具生成，不提交仓库）：

```bash
node scripts/replication-drill-secrets.mjs /tmp/dbsphere-replication.env /tmp/dbsphere-replication-credentials.json
docker build -f Dockerfile-go -t dbsphere/replication-drill:local .
docker compose --env-file /tmp/dbsphere-replication.env -f test/replication-drill/compose.yaml up -d --wait --wait-timeout 300
node scripts/wal-archive-drill-acceptance.mjs http://127.0.0.1:18092 /tmp/dbsphere-replication-credentials.json
docker compose --env-file /tmp/dbsphere-replication.env -f test/replication-drill/compose.yaml down -v --remove-orphans
```

GitHub CI 在复制延迟演练后、清理测试网络前执行同一 WAL 脚本。还需真实归档仓库不可达、磁盘满、备份链恢复和生产告警路由验收；不能由一次 `archive_command=false` 推断生产归档/备份能力。
