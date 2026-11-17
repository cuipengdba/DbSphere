# DbSphere V0.5.0 发行说明

V0.5 为 PostgreSQL 14–18 增加 SQL 性能洞察，继续采用 Linux 服务端、浏览器控制台、Docker Compose 一键部署和独立 PostgreSQL 18 长期存储。没有 AI 模型也能使用 Top SQL、巡检、告警及事件证据。

## 新增能力

- 只读检测 `pg_stat_statements` 扩展及视图字段，采集每个资产累计耗时前 50 个指纹的调用、耗时、行数、共享块读写、临时块读写和 WAL 计数。扩展缺失或不可用时保持基础监控并显示缺口。
- 浏览器展示 Top 20、相邻采样增量与最多 12 个窗口趋势。退化提示需要两个可比窗口、各至少 5 次调用、平均耗时至少翻倍且增加至少 50 毫秒；统计重置时不计算增量。同期 Linux CPU/内存、K8S Pod 状态/重启与已有事件 ID 可并列查看。
- 管理员可导入 PostgreSQL CSV/JSON 慢日志片段，按资产数据库和时间筛选、重复导入去重；仅持久化耗时、时间、来源和可用 Query ID。原始 SQL、参数与客户端信息不入库。
- 管理员可导入已有 `EXPLAIN (FORMAT JSON)`，比较同 Query ID 的计划结构及已有的执行时间。DbSphere 不自动在生产库运行 `EXPLAIN ANALYZE`；计划过滤表达式、对象名和 SQL 正文不入库。
- SQLite 预览数据迁移到 PG18 的演练增加巡检历史、计划、慢日志和安全的执行计划摘要。

## 验证范围

- [Linux 发布验收 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44052541568)：11 个作业通过，含 49 项自动化测试、PG14–18 五版本真实 `pg_stat_statements`、真实 PG18 SQL 延迟退化、PG18 存储与 SQLite 迁移、一键 Compose、PG14/node_exporter 和 PgBouncer。发布提交还会复验无扩展降级。
- 本地 PG14–18 容器验证只读诊断、扩展采集与计划解析；PG18 五次快查询和五次慢查询呈现约九倍平均耗时差异。
- 浏览器连接独立 PG18 监控存储和真实 PG18 目标，验证 Top SQL、慢日志 JSON 导入、计划导入和敏感语句不在页面呈现。

## 已知限制

- `pg_stat_statements` 不提供 p95/p99，DbSphere 不生成虚构分位值。前 50 名之外的指纹可能没有趋势，Query ID 不保证跨 PG 大版本稳定。
- 慢日志需运维方提供已轮转片段，单批上限 60 KiB/200 条；目前无自动日志采集器。缺 Query ID 时只按时间呈现，不强行关联指纹。
- 执行计划由管理员上传，执行时间如存在也仅代表该份计划；不自动运行目标 SQL。Top SQL 退化与资源/事件的时间并列是定位线索，尚不构成完整跨层根因证明。
- 应用 Trace、通用证据图、八类跨层根因规则和可选 AI 仍在 V0.6–V0.8。V0.5 仍不能宣称完成一个问题的应用到数据库全链路根因分析。

配置和接口见[SQL 性能洞察说明](SQL_PERFORMANCE_V0.5.md)；后续工作见[十版本任务清单](DEVELOPMENT_TASKS.md)。
