# DbSphere 离线部署手册

[快速开始](QUICK_START.md) · [最新正式版](https://github.com/cuipengdba/DbSphere/releases/latest) · [支持矩阵](SUPPORT_MATRIX.md)

发布页提供 Linux amd64 和 arm64 产物。先在联网机器下载所选文件及同名 `.sha256`，再复制到目标服务器。下列命令以 v3.8.0 为例。

| 部署方式 | 文件前缀 | 目标机条件 |
| --- | --- | --- |
| 标准一键离线包 | `DbSphere-go-offline-` | Docker Engine、Compose v2，内置 PostgreSQL 18 监控库 |
| Grafana 增强离线包 | `DbSphere-go-observability-` | Docker Engine、Compose v2，内置 PostgreSQL 18、Prometheus 与 Grafana OSS |
| 原生 systemd 包 | `DbSphere-go-systemd-` | Linux systemd、独立可写 PostgreSQL 18 |
| 静态 Go 程序 | `DbSphere-go-<版本>-linux-<架构>` | 独立可写 PostgreSQL 18，按现场需要配置服务管理 |

## 容器化一键安装

```bash
bundle=DbSphere-go-offline-3.8.0-linux-amd64.run
sha256sum --check "$bundle.sha256" && bash "$bundle"
curl -fsS http://127.0.0.1:8081/readyz
```

安装器验证架构并载入本地镜像，Compose 使用 `--no-build --pull never`。默认绑定部署主机的 `127.0.0.1:8081`。首次登录和远程 SSH 转发见[快速开始](QUICK_START.md#2-首次登录)。

配置文件默认在 `${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go/dbsphere-go.env`。可通过 `DBSPHERE_GO_CONFIG_DIR` 指定绝对配置目录。在配置文件中设置 `DBSPHERE_WEB_PORT` 可调整宿主机端口。容器模式自动采集默认开启，`DBSPHERE_COLLECT_INTERVAL_SECONDS` 为 15–3600 秒，`DBSPHERE_COLLECT_WORKERS` 为 1–16，默认 60 秒和 4 个工作进程。

要接入已有独立 PG18，在配置文件中填写应用可访问的 `DBSPHERE_DATABASE_URL`。容器内的环回地址指向应用容器自身。数据库连接账号需要在监控数据库的 `public` schema 中创建和更新 DbSphere 对象。切换 DSN 不会迁移原监控库数据。

## Grafana 增强离线包

```bash
bundle=DbSphere-go-observability-3.8.0-linux-amd64.run
sha256sum --check "$bundle.sha256" && bash "$bundle"
```

增强模式使用相同的配置目录和 PG18 数据卷。Prometheus 和 Grafana 在 Compose 私有网络中监听。平台管理员登录 DbSphere 后，从实时观测、基础设施专题或事件详情进入站内 Grafana，可直接查看只读面板。返回时保留当前调查上下文。

Prometheus 数据默认保留 7 天并限制为 2 GB，可通过 `DBSPHERE_PROMETHEUS_RETENTION_SIZE` 调整。明确登记 Linux Exporter 地址和端口的资产会自动生成抓取目标。PG18 继续保存 DbSphere 的长期监控与事件证据。

需要停用附加组件时，使用当前增强包运行 `bash "$bundle" --disable-observability`，之后可运行对应版本标准包，沿用现有配置和数据卷。

## 原生 systemd 安装

从发布页下载对应架构的 systemd 包。将示例路径 `/absolute/dbsphere.env` 改成实际配置路径。填写独立 `DBSPHERE_DATABASE_URL` 和管理员引导配置。已部署环境须沿用原 `DBSPHERE_MASTER_KEY`，它用于解密已有资产凭据。

```bash
bundle=DbSphere-go-systemd-3.8.0-linux-amd64.run
sha256sum --check "$bundle.sha256"
(umask 077 && bash "$bundle" --sample-config > /absolute/dbsphere.env)
sudoedit /absolute/dbsphere.env
sudo bash "$bundle" --check /absolute/dbsphere.env
sudo bash "$bundle" /absolute/dbsphere.env
systemctl status dbsphere-native.service
```

`--check` 检查 PG18、写入状态、权限和迁移版本。安装后配置位于 `/etc/dbsphere-native/dbsphere.env`，升级时可省略环境文件参数。原生运行端口使用 `DBSPHERE_PORT`，默认 8080。静态程序也可用同一组连接和主密钥配置自行管理。

## 升级与密码轮换

1. 备份监控 PG18 数据、日志原文和环境文件。环境文件包含连接凭据和主密钥，按私密文件管理。
2. 使用同架构、同部署方式的新正式包校验并执行安装。沿用已有配置目录、PG18 数据卷或外部 DSN。
3. 检查 `/readyz`、左下角版本号，并打开一条已有资产、采样和事件。回滚时使用已验证兼容的旧版安装包和适用的数据库快照。

已有管理员密码保存在监控库中。容器安装要主动轮换时，先更新权限为 `0600` 的 `dbsphere-go.env` 中的 `DBSPHERE_ADMIN_PASSWORD`，再运行对应离线包的 `--rotate-admin-password`。旧管理员会话随密码轮换失效。

长期保留及清理策略见[历史数据存储](HISTORY_STORAGE.md)，归档原文保留策略见[日志分析](LOG_ANALYSIS.md#续传归档和备份)。

## 双副本与备份

首次安装时以 `DBSPHERE_GO_HA=true ./DbSphere-go-offline-<版本>-linux-<架构>.run` 启动，已有安装可在 `dbsphere-go.env` 加入 `DBSPHERE_GO_HA=true` 后重运行安装包。双副本模式需要 Docker Compose v2.24.4 或更新版本。网关使用同一 Go 镜像，并沿用 `DBSPHERE_WEB_PORT`。在「系统状态 → 平台副本」查看在线副本、采集轮次、当前维护主节点和租约。双副本和网关在同一台 Docker 主机运行。

跨主机可将应用实例接入同一外置负载均衡器和 PG18 可写主节点入口，例如已经部署并验收的 Patroni 写入口。副本使用相同的 `DBSPHERE_MASTER_KEY`、目标采集周期、通知和作业配置。外置模式要在相应副本设置 `DBSPHERE_ENABLE_GO_COLLECTOR=true`。网关或外置负载均衡器可用 `/readyz` 路由，`/healthz` 可用于进程存活检查。采集租约未完成时由下一副本在到期后接手。Webhook 的 `Idempotency-Key` 和 `X-DbSphere-Delivery-Id` 在重试时一致，接收端可据此去重。[Compose 合并规则](https://docs.docker.com/reference/compose-file/merge/)

Go 离线包内置随包 PG18 的备份命令：`bash DbSphere-go-offline-<版本>-linux-amd64.run --backup /absolute/backup/directory`。命令校验离线包和唯一运行的 Go PG18 容器，以 PG18 `pg_dump -Fc` 生成归档，`pg_restore --list` 验证可读性，并输出带 SHA-256 的 JSON 清单。源码一键部署可运行 `bash scripts/backup-go-store.sh /absolute/backup/directory`。外部 PG18 不由此命令备份，应使用其自身的 PG18 备份体系。此能力仅备份数据库。部署方可在独立 PG18 中演练恢复，并核对资产、事件和原始日志。

在「系统状态 → 账号和团队」创建项目团队，填写团队负责的资产 UUID，再为独立账号分配角色和团队。管理员可全局管理。值班员可以确认、交接其授权事件并提交工单，独立审批人可以批准或驳回授权范围内他人提交的工单，只读人员可以查看授权资产和证据。新设密码至少 12 字符。停用账号或轮换凭据会撤销该账号已有的会话。团队清单为空的非管理员账号拥有零资产访问权。

## 账号、单点登录和审计

从旧版升级后，已有资产和团队归入默认组织，已有平台管理员继续用于本机管理。平台管理员可从「组织管理」创建组织，纳管新资产时选择所属组织。组织管理员可管理本组织资产及其凭据、账号、团队和事件链接。分享链接在期限内可解析，接收者登录后须具有该资产的访问权限。

启用 OIDC 时在部署环境配置 `DBSPHERE_OIDC_ISSUER`、`DBSPHERE_OIDC_CLIENT_ID`、`DBSPHERE_OIDC_CLIENT_SECRET` 与 `DBSPHERE_OIDC_REDIRECT_URL`。回调地址是可从浏览器访问的控制台绝对地址，路径固定为 `/api/oidc/callback`。为所需账号填写身份源提供的 `sub`，登录时会核对签发者与 `sub`。身份源不可用时可使用已有本地凭据。线上身份源、控制台回调和令牌交换使用 HTTPS。[OpenID Connect Go 客户端](https://github.com/coreos/go-oidc)

「操作记录」支持按组织导出签名审计。保存导出文件、公钥指纹以及可信审计头，再用发布包内的 Go 程序在离线环境核验。如导出之后的审计也需验证连续性，可传入先前保存的审计头哈希。

```bash
./DbSphere-go-<版本>-linux-<架构> verify-audit /absolute/path/to/audit.json <公钥指纹> <前次审计头哈希>
```

启用多个组织前备份监控库和配置。回退到尚未支持组织隔离的应用版本时，使用启用组织前的备份同时恢复应用和监控库。

已有初始管理员、`DBSPHERE_VIEWER_PASSWORD` 或 `DBSPHERE_APPROVER_PASSWORD` 可继续留空账号登录。选「迁移初始凭据」，即可用已配置的密码创建同角色的独立账号，并为其显式分配团队。迁移初始查看账号时，将原 `DBSPHERE_VIEWER_ASSET_IDS` 中应保留的资产加入团队。验证新账号后，可从安装配置删除旧只读或旧审批账号的环境变量并重启，旧会话随之失效。初始管理员仍可用于本机引导。

在「问题事件 → 处置」预演并提交只读采样或巡检，审批通过后由管理员执行。审批有效期默认 15 分钟，可设为 1–60 分钟。执行上限默认 35 秒，可设为 1–35 秒。作业结果、原始样本和巡检报告可从原事件追溯。API 可传 `idempotencyKey`，同一申请人和事件用相同参数重试会返回原作业。进程中断后，已认领的作业在期限届满后显示待复核，需核对目标实际状态再重新发起。

按需在本机部署环境文件配置单行 JSON `DBSPHERE_WORK_ORDER_TARGETS`，键为执行配置名称。每个条目包含 `assetId`、`action`、`dsn`、`schema` 和 `relation`。`action` 可为 `verify_restore` 或 `analyze_relation`。连接串使用独立的 PostgreSQL 执行账号，生产连接可用 `sslmode=verify-full`。应用配置及凭据发生变化后，重新预演和申请作业。

- `verify_restore` 连接已恢复的独立端点和数据库，额外配置 `markerColumn`、`requiredMarker` 与 `excludedMarker`。核验读取恢复点前后各一个标记，执行账号需要标记列的 SELECT 权限。
- `analyze_relation` 限制为事件资产数据库中的一张普通持久表。PostgreSQL 17 起可为执行账号授权该表的 MAINTAIN，更早的版本要求执行账号具备表所有者权限。部署配置完成后，作业仍经过预演和独立审批。[PostgreSQL ANALYZE 文档](https://www.postgresql.org/docs/18/sql-analyze.html)

管理员密码由首次改密或首次配置写入监控库。仅修改 `DBSPHERE_ADMIN_PASSWORD` 并重启时仍使用监控库中的密码。要轮换现有安装，先在权限为 `0600` 的 `dbsphere-go.env` 中更改该值，再运行同一离线包的 `--rotate-admin-password` 参数。安装器在已运行的 PG18 存储中更新密码哈希，随后重建 Go 应用容器。旧管理员会话随密码哈希变化失效。源码 Compose 部署可在修改 `deploy/compose/.env.go` 后执行 `docker compose --env-file deploy/compose/.env.go -f deploy/compose/compose.go.yaml run --rm --no-deps app rotate-admin-password`，再重建应用容器。保留 `DBSPHERE_MASTER_KEY`，它用于解密已有资产凭据。

## Kubernetes 接入

需要访问 K8S API 时，可在 `deploy/compose/.env.go`（联网开发部署）或 `${DBSPHERE_GO_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go}/dbsphere-go.env`（离线包）追加 `DBSPHERE_KUBECONFIG_HOST_PATH=/absolute/path/to/read-only-kubeconfig` 后重新执行一键脚本。脚本校验文件存在并只读挂载；配置需使用静态 ServiceAccount token 和受信 CA，不执行 kubeconfig 中的 `exec` 凭据插件。Go K8S 适配器支持已映射 Pod 的状态、Node、Service、PVC/PV、Pod 事件与可用的 metrics.k8s.io。权限按纳管命名空间和指标来源配置，验收范围见[支持矩阵](SUPPORT_MATRIX.md)。

如已有 Prometheus 抓取 kubelet 的 `kubelet_volume_stats_used_bytes`，可在同一配置文件设置 `DBSPHERE_PROMETHEUS_URL=https://prometheus.example.internal`，需要 Bearer 令牌时再设置 `DBSPHERE_PROMETHEUS_TOKEN`，并重新运行一键脚本。此项可选；未配置或指标缺失时 PVC 使用率保持未知，不会误判为健康。Go 适配器按资产映射的 Namespace/PVC 查询，不接收浏览器指定的 Prometheus 地址。

## 通知与应用 Trace

告警通知可按需配置。在同一配置文件设置 `DBSPHERE_WEBHOOK_URL=https://...`，可加 `DBSPHERE_WEBHOOK_SECRET` 启用 HMAC-SHA256 签名；邮件需同时设置 `DBSPHERE_SMTP_HOST`、`DBSPHERE_SMTP_FROM`、`DBSPHERE_SMTP_TO`，可选端口及用户/密码。默认邮件端口 587 并要求 STARTTLS，465 使用 TLS；仅环回 SMTP 可设置 `DBSPHERE_SMTP_ALLOW_PLAIN=true`。`DBSPHERE_NOTIFY_MIN_SEVERITY` 默认 `warning`，可按 `DBSPHERE_WEBHOOK_MIN_SEVERITY`、`DBSPHERE_EMAIL_MIN_SEVERITY` 分通道覆盖。未配置通道的投递记录标记为跳过；失败投递最多自动重试五次，之后可在 B/S 页面手动重试。

应用 Trace 接入默认关闭。需要时在同一配置文件设置不少于 32 字符的 `DBSPHERE_OTLP_TOKEN`，重启后将应用或 OpenTelemetry Collector 的 OTLP/HTTP JSON Trace 发送到 `POST /v1/traces`，使用 `Authorization: Bearer <token>`。仅与唯一纳管 PG 资产匹配的 Trace 入库，不保存 SQL、参数、URL 或任意未经允许的 Span 属性；可选白名单字段 `dbsphere.trace.sampling_probability` 仅作为发送方采样声明保存，详见[Trace 时间可信度](TRACE_TIME_COVERAGE.md)。`DBSPHERE_TRACE_RETENTION_DAYS` 默认 7 天，Go 工作器每小时最多清理 10000 条过期 Span；生产入口应使用 TLS。不同 SDK 的 `http/json` 支持仍需分别验证。
