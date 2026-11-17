# V0.5 SQL 性能洞察

DbSphere 在目标 PostgreSQL 14–18 上只读采集 `pg_stat_statements`。管理员需自行在目标实例配置 `shared_preload_libraries`、重启并在被纳管数据库安装扩展；DbSphere 不修改目标配置或自动创建扩展。扩展未安装、未预加载或权限不足时，页面显示具体缺口，资产监控、告警和巡检继续工作。建议监控角色只授予 `pg_monitor`，避免超级用户。

采样保留单个数据库累计耗时最高的前 50 个指纹，记录 Query ID、数据库和用户 OID、调用数、总执行时间、行数、共享块命中/读/写、临时块读/写、WAL 字节及扩展统计重置时间。**不采集、保存或返回代表性 SQL 正文**。页面显示 Top 20、相邻采样增量和平均耗时；两个可比窗口中每个窗口至少 5 次调用、平均耗时提高至少两倍且增加至少 50 毫秒，才提示退化。统计重置、指纹缺失或计数下降时不计算增量。最多 12 个采样窗口可与同期 Linux CPU/内存、K8S Pod 状态/重启并列查看，并链接该资产的开放事件 ID。

`pg_stat_statements` 不提供 p95/p99 分位数，界面不虚构分位结果。前 50 名以外的 SQL 可能缺少连续趋势。Query ID 受对象 OID、体系结构及 PostgreSQL 大版本变化影响，**不能跨大版本直接等同指纹**；同一 Query ID 在不同数据库或用户下也须区分。[PG14 扩展文档](https://www.postgresql.org/docs/14/pgstatstatements.html)及[PG18 扩展文档](https://www.postgresql.org/docs/18/pgstatstatements.html)说明了这些边界。

## 慢 SQL 日志导入

资产详情可手动导入已经轮转的 PostgreSQL `csvlog`（PG14–18）或 `jsonlog`（PG15–18）片段，每批不超过 60 KiB 和 200 条记录。DbSphere 只接受与资产数据库名一致、消息包含 `duration: ... ms` 的记录；保存时间、耗时、来源格式和可用的精确 Query ID。原始日志、语句正文、参数、过滤表达式、用户名和客户端地址在解析后立即丢弃，不进入 PG18 长期存储或 API 响应。同一原始记录的 SHA-256 键用于重复导入去重。缺 Query ID 的日志仍可按时间查看，但不强行匹配指纹。

手动导入 API：`POST /api/assets/{id}/slow-logs/import`，JSON 为 `{ "format": "csvlog|jsonlog", "content": "..." }`；查询 API 为 `GET /api/assets/{id}/slow-logs?from=...&to=...`，时间窗最多 31 天。接口需要管理员会话，写操作需要 CSRF。时间与同一资产事件相距不超过 5 分钟时，页面提供事件入口；此链接是时间相关性，**不自动宣称因果关系**。PG14 CSV 和 PG18 JSON 字段依据[PostgreSQL 日志文档](https://www.postgresql.org/docs/18/runtime-config-logging.html)。

## 执行计划证据

管理员可上传已有的 `EXPLAIN (FORMAT JSON)` 输出与 Query ID。DbSphere **不向被监控库执行 `EXPLAIN ANALYZE`**。保存节点类型、深度、估计成本/行数和已存在的实际耗时/块计数；计划中的 Filter、Index Cond、SQL 正文、常量和对象名不入库。相同 Query ID 的最近两份计划显示结构是否变化，已有实际执行时间时才计算比值。这个比值来自手动导入的证据，不能取代同环境的重复测量。

API：`POST /api/assets/{id}/sql-plans`，JSON 为 `{ "queryId": "123", "observedAt": "...", "plan": [{ "Plan": { ... } }] }`；`GET /api/assets/{id}/sql-plans` 返回最多 50 份安全摘要。两类导入数据都写入 PG18 正式存储；SQLite 预览兼容，并可通过 SQLite → PG18 迁移脚本搬迁。

## 验证与限制

本地真实 PG14–18 容器验证扩展采集和只读计划解析；真实 PG18 的五次快查询与五次慢查询演练产生约九倍的平均耗时退化提示。慢日志、去重、权限、敏感字段丢弃、计划结构变化和 SQLite → PG18 迁移通过自动化测试。生产日志导出器、真实应用负载和所有云服务扩展权限仍需部署环境验收。当前只关联已有事件，不会因为 SQL 退化自动生成新事件；跨层根因分析在 V0.6 继续实现。
