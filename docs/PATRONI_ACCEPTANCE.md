# Patroni 只读监控与故障验收

> 2026-11-18 开发版记录。当前实现是可选信号；未填写 Patroni 地址的 PostgreSQL 资产仍按原方式采集。PG14–18 的隔离 Linux 容器双节点故障矩阵已通过；本记录不代表生产高可用、全部运行形态或 V1.0 发布验收。

## 接入与边界

在资产表单填写 Patroni REST 根地址，例如 `https://reader:密码@patroni.example:8008`。DbSphere 只调用 `GET /cluster`，以加密字段保存接入地址，资产 API 仅返回是否配置。采集限制为 5 秒、1 MiB 响应、100 个成员；不跟随重定向，也不使用环境代理。页面显示成员角色、状态、节点地址、timeline 和可用的字节级复制延迟；Patroni 返回 `"unknown"` 时保持未知，不填 0。Patroni 采集失败单列为遥测告警，**不会**据此推断数据库宕机；持续 30 秒的 leader 数量异常才触发默认可用性告警和根因候选。候选只确认观察到的拓扑异常，不把 DCS、网络或节点故障当成已证实原因。[Patroni `/cluster` 协议说明](https://patroni.readthedocs.io/en/latest/rest_api.html)。

Go 后端没有调用切主、重启、重建副本等 Patroni 写接口。HTTP 明文地址仅适合受信测试网络；生产接入应使用受信证书的 HTTPS，并让 DbSphere 到 REST API 的网络路径可达。此功能无 AI 模型也可工作。

## 实际验收

本机隔离 Docker 网络运行 PostgreSQL 18.6 + Patroni 4.1.5 双节点、单节点 etcd；测试用 Go 后端使用独立 PostgreSQL 18 监控库存储，`/readyz` 返回 `ready/postgresql18`。被监控数据库使用具 `pg_monitor` 权限、无超级用户权限的账号。初始真实样本 `d317ff81-a37b-4fda-a149-71888bacc7e4` 同时读到 PG18、一个 `running` leader、一个 `streaming` replica 和复制延迟。Playwright 浏览器在资产详情显示完整拓扑，控制台无错误。截图保存在本机 `H:\CodexLS\outputs\DbSphere-patroni-local.png`。

首次故障演练强制停止原 leader `patroni1`，连续采样读到 3 次 `leaderCount=0`，随后 `patroni2` 自动晋升；旧节点重入后成功样本 `7cb0a3c7-16f7-4c60-b238-028649358f9a` 恢复。主可用性事件 `d61fb82e-d4e2-42a1-872e-428a925d03a1` 汇集 PG 无法连接、Patroni 无 leader、两条告警、`patroni_leader_anomaly` 候选与 `supported_by` 证据关系，并以同一 ID 关闭。复核发现旧节点恢复时的 PostgreSQL `57P03` 被误分为另一个遥测事件，因此修正了连接相关 SQLSTATE 的分类：`08xxx`、`53300`、`57P01`、`57P02`、`57P03` 统一归入可用性；认证和权限错误继续归入遥测。[PostgreSQL SQLSTATE 定义](https://www.postgresql.org/docs/current/errcodes-appendix.html)。

更新 Go 后端后再次强制停止当前 leader `patroni2`。连续采样读到 2 次 `leaderCount=0`，`patroni1` 晋升；旧节点恢复为 `streaming` replica，成功样本 `514a7674-da1e-439c-ad8f-5b45700fe72a` 关闭**唯一**事件 `7410d94d-0e99-45dd-ad08-8f961c2952df`。该事件包含 `pg_unavailable`、`patroni_leader_invalid` 告警、`patroni_leader_anomaly` 候选、原始 Patroni/PG 样本、`hypothesizes` 与 `supported_by` 边；没有再生成同故障遥测事件。测试将 leader 规则持续时间暂设为 0 秒以捕获短暂选主窗口，结束后恢复默认 30 秒，不能据此声称默认持续时间在该短窗口必然告警。浏览器事件页显示同一事件 ID、候选证据、无 leader 时间线及恢复样本，控制台无错误。脱敏运行结果与截图在本机 `H:\CodexLS\outputs\DbSphere-patroni-failover-fixed.json`、`DbSphere-patroni-events-fixed.json`、`DbSphere-patroni-event.png`。

本轮最终源码执行 `go test ./... -count=1`（连接隔离 PG18 测试库）、`go vet ./...` 和 `node --check public/app.js` 均通过。Go Linux 静态镜像还在本机 kind 的既有 PG18/K8S 资产上取得新成功样本，见[本机 Kubernetes 验收](KIND_ACCEPTANCE.md)。

## 可重复故障演练

仓库新增 `test/patroni-drill/` 与 `scripts/patroni-drill-acceptance.mjs`：使用一次性凭据，在隔离 Docker Compose 网络中启动两个 Patroni 4.1.5 + 指定 PG14–18 版本节点、etcd、独立 PG18 监控库及 Go 静态页面服务。脚本动态找出 leader，以 `pg_monitor` 账号纳管，停止 leader，要求得到 PG 不可用和无 leader 的真实样本，待原节点作为 streaming replica 重入后，检查**同一个事件 ID**中的告警、原始样本、根因候选、证据图、恢复和复盘报告。测试将 leader 规则持续时间临时设为 0 秒，结束时恢复为 30 秒。镜像构建需要联网安装测试用 Patroni；它不是 DbSphere 的离线交付镜像。

本机首次运行揭示测试容器的本地 PostgreSQL socket 缺少 `pg_hba` 规则，修正后 PG18 自动演练通过，事件 `520f45e8-48ea-4599-8176-bf8eeba79fa6` 仅有一个已恢复的可用性事件；无 leader 样本 `400f5476-5543-40f8-92a6-59a37a7afd43` 与恢复样本 `f41020c2-2568-4a48-b0a3-b5ff8fde8747` 均在其证据图中。脱敏结果保存在本机 `H:\CodexLS\outputs\DbSphere-patroni-ci-local.json`。

[首次独立 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44336037312) 在 PG18 快速自动切换中没有采到短暂的无 leader 窗口，因而故障演练作业失败，其余作业通过。为消除时序依赖，脚本现在由测试夹具的 `patronictl` 先暂停自动切换，待 PG 不可用和无 leader 样本落入监控库后恢复自动切换；**DbSphere 服务仍只读**。本机 PG14 双节点用更新后的脚本通过，目标版本 14、单事件 `17df8d8a-31b6-4a88-a4fc-67a24724aa4b`、无 leader 样本 `52ea1b9e-8729-45b1-8b92-c0a10a8b16ec`、恢复样本 `bbda1d8c-8cc3-4164-ba4e-c6c21b9dde86` 已核对；脱敏结果在 `H:\CodexLS\outputs\DbSphere-patroni-pg14-local.json`。[修正后的独立 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44339801805) 中 PG14–18 五个 Patroni 作业及其他作业全部通过。

Linux 开发机可执行以下复现命令，`TMPDIR` 指向仓库外的临时目录，运行完成后删除一次性数据卷：

```bash
TMPDIR=$(mktemp -d)
node scripts/replication-drill-secrets.mjs "$TMPDIR/patroni.env" "$TMPDIR/patroni-credentials.json"
docker build -f test/patroni-drill/Dockerfile -t dbsphere/patroni-drill:local .
docker build -f Dockerfile-go -t dbsphere/patroni-app-drill:local .
docker compose --env-file "$TMPDIR/patroni.env" -f test/patroni-drill/compose.yaml up -d --wait --wait-timeout 300
node scripts/patroni-drill-acceptance.mjs http://127.0.0.1:18097 \
  "$TMPDIR/patroni-credentials.json" "$TMPDIR/patroni.env" "$TMPDIR/patroni-result.json"
docker compose --env-file "$TMPDIR/patroni.env" -f test/patroni-drill/compose.yaml down -v --remove-orphans
```

其他 PG 大版本可在 `patroni.env` 中加入 `DBSPHERE_DRILL_PG_MAJOR=14`（或 15–17），构建时增加 `--build-arg PG_MAJOR=14`，运行验收脚本时追加 `14` 参数。长期监控库始终为 PostgreSQL 18。

## Kubernetes 本机故障演练

2026-11-19 在本机单节点 `kind-dbsphere-test` 的 `dbsphere-smoke` Namespace 中部署 PG14 双节点 Patroni、单节点 etcd 测试夹具，复用已有 Go 服务 Pod 与独立 PG18 监控库。测试夹具由 [`test/patroni-drill/k8s.yaml`](../test/patroni-drill/k8s.yaml) 定义；只在测试集群创建资源。Go 服务使用现有只读 ServiceAccount 观察 PostgreSQL、Patroni REST 和 Kubernetes；测试脚本使用本机测试集群管理员身份暂停选主和缩容 Deployment，故障操作不经过 DbSphere API。

首次运行取得 PG14、K8S 与 Patroni 的成功基线。缩容主节点后，真实样本分别记录 PG 不可用和 `leaderCount=0`，原节点重建并以 `streaming` 备库重入。验收脚本最初要求资产只生成一个事件，实际还产生独立的 `k8s_collection_failed` 遥测事件：主节点 Pod 被缩容时映射暂时无法读取，系统按数据缺口报警。这是需要显示的采集缺口；同一个**可用性事件**已包含 `k8s_state` 缺口证据。因此脚本改为要求一个可用性事件，并允许另有明确标记的遥测事件。

修正断言后再次演练通过：PG14 基线样本 `29a7b368-c65d-432e-8ccb-3404f37fc8dc`，无 leader 样本 `4e5936c2-d5eb-4406-adc3-bf28f77804be`，PG 不可用样本 `eb12d2dc-3283-4846-bd9c-1a9013738bc4`，恢复样本 `b2822229-4802-45df-8a3f-c4e196179eb1`；可用性事件 `c0c3e0dc-a8df-4d37-a86d-5464013d416b` 在同一 ID 下关联 Patroni/K8S 状态、`patroni_leader_invalid` 告警、`patroni_leader_anomaly` 候选与恢复，状态为 `resolved`。该事件的 Markdown 复盘报告返回 200，并包含事件 ID、候选代码和无 leader 样本 ID。单独的遥测事件为 `536500d4-a2da-4e3a-9504-bb47d5c2d0bf`。脱敏结果保存在本机 `H:\CodexLS\outputs\DbSphere-patroni-kind-pg14.json`。测试期间 Patroni leader 告警持续时间设为 0 秒以捕获选主窗口，结束后恢复默认 30 秒。

跨事件下钻补充：Go 事件 API 现在只用**同资产、同一原始样本 ID**关联单独的遥测事件，双向返回事件 ID、状态和共享样本；浏览器可从主可用性事件打开采集缺口事件，Markdown 报告也列出链接。模型证据包省略关联事件详情，不扩大外发范围。PG18 实库测试验证精确匹配、反向链接、不同样本及跨资产隔离；本机 kind 既有事件复验确认主事件关联遥测事件 `536500d4-a2da-4e3a-9504-bb47d5c2d0bf`，共享样本 `eb12d2dc-3283-4846-bd9c-1a9013738bc4`，报告包含该事件链接。共享样本只证明同一次采集，不把采集缺口解释为故障根因。

复现前需有 `dbsphere-smoke` 测试 Namespace、已运行的 Go 服务与独立 PG18、镜像 `dbsphere/patroni-drill:local` 已导入 kind，以及只包含 `DBSPHERE_DRILL_PG_PASSWORD`、`DBSPHERE_DRILL_REPL_PASSWORD` 的 `dbsphere-patroni-drill` Secret。管理员 kubeconfig 只供测试脚本故障注入；Go 服务继续使用 Namespace 内只读 ServiceAccount。对 Go 服务做本机端口转发后运行：

```powershell
$env:KUBECONFIG = 'H:\CodexLS\tmp\dbsphere-local-admin.kubeconfig'
kubectl apply -f test/patroni-drill/k8s.yaml
kubectl port-forward -n dbsphere-smoke svc/dbsphere-go 18090:8080
```

在另一终端设置同一 `KUBECONFIG`，将路径换成各自的隔离测试凭据文件，再运行：

```powershell
$env:KUBECONFIG = 'H:\CodexLS\tmp\dbsphere-local-admin.kubeconfig'
$credentials = 'H:\CodexLS\tmp\dbsphere-patroni-pg14-credentials.json'
$adminPasswordFile = 'H:\CodexLS\outputs\DbSphere-kind-admin-password.txt'
$resultFile = 'H:\CodexLS\outputs\DbSphere-patroni-kind-pg14.json'
node scripts/patroni-kind-acceptance.mjs http://127.0.0.1:18090 `
  $credentials $adminPasswordFile $resultFile 14
```

本机 kind 只有一个 control-plane 节点，Patroni 使用单节点 etcd 和临时 Pod 数据层，不能证明多节点 DCS、持久卷、网络分区或生产 Operator 的可靠性。故障时独立遥测事件也提醒值班员 K8S Pod 映射缺口；可用性事件仍是本次问题从故障到恢复的主要追踪入口。

## 尚待验收

本机覆盖 PG14 和 PG18 双节点 Linux 容器、PG14 单节点 kind、单节点 DCS 和人工故障注入；PG15–17 双节点已由独立 Linux CI 验证。三节点 DCS、Kubernetes Operator/StatefulSet、TLS/Basic Auth 生产证书、跨节点网络分区、长期负载、自动发现与真实业务 Trace 尚未测试。生产稳定性仍待验收。DbSphere 仍是只读监控，不承担 Patroni 的自动选主或故障切换。V1.0 任务保持未完成，见[开发任务](DEVELOPMENT_TASKS.md)。
