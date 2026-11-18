# PostgreSQL 物理复制故障闭环演练

> 开发版本机验收。隔离 Docker Compose 工程运行 PostgreSQL 18 主库、物理备库、独立 PG18 监控存储和 Go 静态服务；不依赖 AI 模型。此演练用于验证真实复制延迟的一问题一事件 ID 路径，不代表 Patroni、云 PG 或生产高可用验收。

## 2026-11-18 本机结果

官方 PG18 主库报告 1 个 `streaming` 备库，备库报告 `pg_is_in_recovery()=true` 和 WAL 接收器 `streaming`。DbSphere 用只读 `pg_monitor` 对主备各取一条正常基线。暂停备库回放后，每次写入一条真实 WAL 记录再切换段；故障样本观测到 `134217464` 字节回放差值，超过默认 64 MiB 阈值并持续超过 120 秒。

Go 服务在**同一个事件** `10b01d66-5969-476d-b1b8-f4112a4e7685` 中保存延迟告警、原始故障样本 `4a1c96ea-0c99-459c-854f-15e13ff1ca43`、`replication_lag` 候选、支持证据与 Markdown 复盘。恢复回放并追平后，成功样本 `43ba041a-60d8-4762-b37f-84a17a3f5135` 的延迟为 **0 字节**、主库仍有 1 个流复制备库，且 `alert_recovered` 引用该样本；原事件变为 `resolved`，故障告警和候选仍可追溯。当前源码的 Go 静态镜像替换 app 容器后，这些数据仍能从独立 PG18 存储读回。脱敏机器结果在 `H:\CodexLS\outputs\DbSphere-replication-acceptance.json`。

最终脚本和当前 Go 镜像再次执行完整故障闭环：新事件 `fec27a88-e4b5-4644-bd12-291f55af4ccc` 包含故障样本 `5825f5c1-fa0e-44a2-9c9e-95b60eeccfc5`（134217672 字节）、`replication_lag` 根因候选和恢复样本 `accade38-0274-40f4-b174-f632a46e5c9b`（0 字节、1 个流复制备库）；恢复告警引用该样本，事件为 `resolved`。再次替换 Go app 容器后只读验证通过，备库回放未暂停。脱敏结果在 `H:\CodexLS\outputs\DbSphere-replication-acceptance-current.json`。

首次试跑发现两个测试脚本问题：连续 `pg_switch_wal()` 而不写 WAL 只形成一段有效差值；旧断言把 Node API 的 `analysis.eventId` 用到了 Go API，而 Go 的 ID 位于 `event.id`。两处已修正，首轮留下的测试事件 `b429c5db-a2fc-44bc-a4c5-291977932842` 在恢复采样后确认为 `resolved`。上述通过结果来自修正后的第二轮完整脚本。

浏览器事件页还暴露了一个显示问题：恢复证据没有阈值字段，旧页面仍按触发告警格式渲染，把空阈值显示为“信号缺失”。原始恢复样本与告警状态实际上正常。页面已改为“已恢复；先前告警值…”并附恢复样本 ID；根因候选的支持证据、匹配条件、时间顺序和缺失信号也改为独立行。替换当前 Go 镜像后，Playwright 浏览器重载事件页已看到修正后的时间线、根因候选和同一事件 ID。截图留在 `H:\CodexLS\outputs\DbSphere-replication-event-rca.png`。

## 拓扑与故障

- 主库和备库以独立数据卷运行官方 `postgres:18-bookworm`。主库创建只读 `pg_monitor` 账号和专用物理复制账号；备库用 `pg_basebackup`、复制槽与流复制接入。测试时主库 `pg_stat_replication` 和备库 `pg_stat_wal_receiver` 都必须为 `streaming`。
- DbSphere Go 使用独立 PG18 数据卷保存资产、样本、告警、证据与事件。资产分别纳管主库和备库；浏览器可经本机 `127.0.0.1:18092` 访问。
- 管理员在**测试备库**执行 `pg_wal_replay_pause()`，主库每写一条真实 WAL 记录后切换 WAL 段，使 `pg_current_wal_lsn()` 与备库回放 LSN 相差超过默认 64 MiB 阈值。手动采样两次，间隔超过规则默认的 120 秒持续时间。
- 验收脚本检查主库真实指标、`pg.replication_lag_bytes` 告警、原始样本、`replication_lag` 候选、事件证据图和 Markdown 报告。随后执行 `pg_wal_replay_resume()`，等待备库追平，确认**同一个事件 ID**下的告警恢复和事件关闭。

## 本机复现

在 `E:\DbSphere\DbSphere` 目录运行下列 PowerShell 命令。密钥和脱敏报告写入 H 盘，不进入 Git：

```powershell
$envFile = 'H:\CodexLS\tmp\dbsphere-replication.env'
$credentials = 'H:\CodexLS\outputs\DbSphere-replication-credentials.json'
node scripts/replication-drill-secrets.mjs $envFile $credentials
docker build -f Dockerfile-go -t dbsphere/replication-drill:local .
docker compose --env-file $envFile -f test/replication-drill/compose.yaml up -d
docker compose --env-file $envFile -f test/replication-drill/compose.yaml ps
node scripts/replication-drill-acceptance.mjs http://127.0.0.1:18092 $credentials > H:\CodexLS\outputs\DbSphere-replication-acceptance.json
node scripts/replication-drill-verify.mjs http://127.0.0.1:18092 $credentials H:\CodexLS\outputs\DbSphere-replication-acceptance.json
```

首次初始化和 `pg_basebackup` 可能需要几分钟。复跑验收脚本会创建新的测试资产；不应重新生成密钥后继续使用旧数据卷。失败时脚本尝试恢复备库回放，并在已知事件 ID 时尝试用成功采样关闭测试事件。删除测试工程前应先检查回放状态和报告；如果需要释放**仅此测试工程**的数据卷，明确使用相同 Compose 文件和 env 文件执行 `down -v`。

## 验收边界

此方案通过 PostgreSQL 管理员在隔离测试库注入故障；DbSphere 只使用 `pg_monitor` 账号进行采集，不取得备库控制权限。复制延迟只说明备库回放落后；报告中的根因候选仍须区分已观察现象与造成延迟的原因。未验证应用请求影响、复制网络中断、自动切换、Patroni DCS、云数据库限制或持续负载。完整 V1.0 故障矩阵仍以[开发任务清单](DEVELOPMENT_TASKS.md)为准。
