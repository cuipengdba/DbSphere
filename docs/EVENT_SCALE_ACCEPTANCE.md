# PG18 事件历史查询容量初测

2026-11-19，在独立 Docker PostgreSQL 18 测试库执行 `TestEventPageScale`。夹具创建一个临时资产、100000 条未结事件，其中每 1000 条有一条命中 `needle`；测量完成后删除该资产及事件，生产和本机 kind 数据未参与。每项先预热一次，再执行 5 次并记录平均与最慢耗时。查询走真实 Go `EventPage` / `EventOverview` 接口，时间包含本机 Go 到 PostgreSQL 的往返及 JSON 解码。

| 场景 | schema 22，批量写入后未 ANALYZE | schema 23，批量写入后未 ANALYZE | schema 23，ANALYZE 后 |
| --- | ---: | ---: | ---: |
| 首 50 条，按资产过滤 | 646 ms | 691 ms | 2.7 ms |
| 深页，跳过前 99950 条 | 26 ms | 3.0 ms | 2.2 ms |
| 标题搜索，100 条命中 | 172 ms | 129 ms | 117 ms |
| 全量未结计数及最新 5 条 | 711 ms | 652 ms | 42 ms |

上述均为 5 次平均，耗时仅代表这台机器。schema 23 新增 `(asset_id,started_at DESC,id DESC)` 与 `(asset_id,status,started_at DESC,id DESC)` 分区索引。对现有大库升级仍需评估索引构建锁、磁盘增长和迁移时长；批量导入后应等待自动分析或执行维护窗口内的 `ANALYZE events`，否则首页和总览可能暂时走低效计划。文本子串搜索仍扫描较多事件，百万级历史、并发写入、多资产规模与真实保留期未验证，V1.0 的 10-02 继续开放。

复现命令（仅对可丢弃 PG18 测试库执行）：

```powershell
$env:DBSPHERE_TEST_PG18_URL='postgres://USER:PASSWORD@HOST:PORT/DISPOSABLE_DB'
$env:DBSPHERE_RUN_EVENT_SCALE='1'
go test -p 1 ./internal/store -run '^TestEventPageScale$' -v -count=1
```
