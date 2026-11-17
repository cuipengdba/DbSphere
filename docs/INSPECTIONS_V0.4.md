# V0.4 巡检开发记录

V0.4 开发代码提供 PostgreSQL、Linux 和 Kubernetes 三个版本化巡检模板。管理员在浏览器选择资产和模板，可手动重新采样并巡检，或启用 15–10080 分钟的定时计划。模板只允许修改已知检查项的数值阈值；每次修改递增模板版本，历史运行保留执行时版本和样本 ID。

结果区分“风险”“通过”“未知”。采集失败、权限不足或指标不可用时显示未知及原因；不能把缺失数据判断为健康。历史记录保存结果及相对上一次巡检的状态变化；失败运行保留可重试记录。当前定时任务每分钟检查到期计划，单次运行重新采集资产。多副本并发部署尚未作为 V0.4 支持配置。

PostgreSQL 模板涵盖不可用、监控角色、PG14 EOL、连接、锁、长事务、WAL 归档、事务冻结年龄、复制槽活跃状态、保留 WAL 和失效状态。Linux 模板检查采集故障及 CPU/内存/文件系统；Kubernetes 模板检查 API 采集、Pod Ready、近期 OOMKilled、Node 压力与 PVC 用量。数据来源和缺口都随巡检结果保存。

备份模板读取由运维侧定期导出的 [pgBackRest `--output=json info`](https://pgbackrest.org/user-guide.html) 文件，不在被监控数据库主机执行命令。Linux 原生服务设置 `DBSPHERE_BACKUP_INFO_DIR`；一键 Compose 在 `.env` 设置 `DBSPHERE_BACKUP_INFO_HOST_PATH=/absolute/read-only/directory`，脚本校验目录并只读挂载。每个资产对应 `<资产 UUID>.json`，文件内容应是**单个 stanza** 的 pgBackRest JSON 数组。可选包装形式 `{ "pgbackrest": [ ... ], "lastAttempt": { "status": "failed", "at": "2026-11-17T00:00:00Z" } }` 由现有备份调度器额外记录最近尝试状态。DbSphere 不读取备份密码，也不执行备份或恢复。文件上限 1 MiB，符号链接不得指向配置目录外。

备份巡检分别检查最近成功备份年龄、仓库健康状态和最近一次尝试失败。原始 pgBackRest `info` 只含已完成备份及 stanza 健康状态，不足以证明最近一次调度是否失败；未提供 `lastAttempt` 时该项始终为**未知**。目录未配置、文件缺失或无备份时也显示未知，不会显示“备份正常”。

正式 PG18 存储的资产容量页按 UTC 日读取最近 30 天的数据库容量末次样本。至少跨三个自然日才计算相邻日增长速率中位数与 30 天粗估；下降按零增长处理，样本不足显示原因。巡检报告可下载 Markdown 和 HTML，记录资产、模板版本、巡检与样本 ID、执行当时开放的事件 ID、检查结果和生成报告时的容量估计。容量估计不能代替磁盘剩余空间或恢复能力验证。

`scripts/pg18-backup-restore-drill.mjs` 在独立测试数据库中写入资产、样本、事件证据和巡检记录，使用 PostgreSQL 18 的 `pg_dump` 生成备份文件，再恢复至另一个独立 PG18 数据库，验证以上记录一致后清理自己的测试数据库与临时文件。本地真实 PG18 容器已通过；此脚本是恢复演练，不是正式环境的自动备份计划。

一键 Compose 的内置 PG18 可执行 `bash scripts/backup-store.sh /absolute/backup/output/directory`，生成权限为 `0600` 的自定义格式 `pg_dump` 归档和不含凭据、供容器只读读取的 `0644` `monitor-store-backup.json` 清单；脚本会用 PG18 `pg_restore --list` 检查归档格式。应把输出目录放在 Compose 数据卷之外，定期将备份复制到独立存储，并按恢复演练验证可用性。外部 PG18 使用其对应的 PostgreSQL 18 客户端和备份体系。

若要在 B/S 存储面板显示**上报的**备份年龄与恢复点，Linux 原生服务设置 `DBSPHERE_STORE_BACKUP_MANIFEST`；一键 Compose 在 `.env` 设置 `DBSPHERE_STORE_BACKUP_HOST_PATH=/absolute/backup/output/directory/monitor-store-backup.json` 后重新运行一键部署。清单格式为 `{ "status": "success", "backupId": "dbsphere-...", "completedAt": "2026-11-17T08:00:00Z", "restorePointAt": "2026-11-17T07:59:00Z" }`。恢复点由备份作业保守上报；页面仅展示清单信息，**不会仅凭清单声称已验证恢复**。未配置、文件无效或不可读时状态保持未知。独立恢复演练脚本才实际执行还原并核对数据。

V0.4 的 PostgreSQL 采集新增 `pg_database.datfrozenxid` 年龄、`autovacuum_freeze_max_age`、`pg_replication_slots` 复制槽明细及选定的非敏感关键参数。五个 PostgreSQL 大版本的只读采集和真实锁等待测试通过。[PostgreSQL 冻结机制](https://www.postgresql.org/docs/18/routine-vacuuming.html)及[复制槽视图字段](https://www.postgresql.org/docs/18/view-pg-replication-slots.html)用于字段与阈值设计。

PG14–18 的只读监控 SQL 与 PG14 EOL 巡检已验证；真实 PG18 复制槽保留 WAL 演练、K8S/PVC 和备份失败信号测试已覆盖。真实 pgBackRest 服务及生产备份计划、生产规模恢复时间和多副本巡检仍需后续验收。本文件在 V0.4 发布前记录开发状态。
