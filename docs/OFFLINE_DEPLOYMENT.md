# Linux 一键离线部署

从 [V1.45.0 发布页](https://github.com/cuipengdba/DbSphere/releases/tag/v1.45.0)下载离线包及同名校验文件。Linux `x86_64` 使用 `amd64` 包，`aarch64` 使用 `arm64` 包。传到已安装 Docker Engine 和 Compose v2 的目标机，在同一目录执行以下命令。以下以 amd64 为例。

```bash
sha256sum --check DbSphere-go-offline-1.45.0-linux-amd64.run.sha256
bash DbSphere-go-offline-1.45.0-linux-amd64.run
```

一键脚本导入随包镜像并启动内嵌静态页面的 Go 服务及 PostgreSQL 18 长期监控库。默认本机入口为 `http://127.0.0.1:8081`。

## 原生 systemd 离线安装

原生服务需要 Linux systemd 和已可访问的独立 PostgreSQL 18。下载对应架构 `DbSphere-go-systemd-<版本>-linux-<架构>.run` 及同名 `.sha256`，使用安装包的 `--sample-config` 写出环境文件。把 `DBSPHERE_DATABASE_URL` 改为独立监控库的连接地址。连接账号需能在该数据库的 `public` schema 创建和更新 DbSphere 对象。已有环境应继续使用原 `DBSPHERE_MASTER_KEY`、管理员引导配置和数据库，原资产密码依赖这把主密钥。容器的 `DBSPHERE_WEB_PORT` 对应原生的 `DBSPHERE_PORT`，DSN 应使用 Linux 主机可解析的 PostgreSQL 地址。

```bash
sha256sum --check DbSphere-go-systemd-1.45.0-linux-amd64.run.sha256
(umask 077 && bash DbSphere-go-systemd-1.45.0-linux-amd64.run --sample-config > /absolute/dbsphere.env)
sudoedit /absolute/dbsphere.env
sudo bash DbSphere-go-systemd-1.45.0-linux-amd64.run --check /absolute/dbsphere.env
sudo bash DbSphere-go-systemd-1.45.0-linux-amd64.run /absolute/dbsphere.env
systemctl status dbsphere-native.service
```

`--check` 使用 systemd 的环境文件解析规则，检查 PG18、写权限和已有迁移版本。它只读取数据库，`schemaVersion=0` 对应尚未初始化的监控库。安装启动服务并设为开机启动，环境文件固定保存在 `/etc/dbsphere-native/dbsphere.env`，升级时省略文件参数即可沿用配置。若要取代容器实例，先对外部 PG18 做备份并停止原应用容器，使用同一个 DSN 和主密钥启动原生服务。容器的安装脚本与原生包均不迁移数据库文件。检测到启动失败时，原生安装器恢复此前的服务文件；数据库备份和结构回退依照对应版本的兼容性操作。

> [V1.0.0 正式版](https://github.com/cuipengdba/DbSphere/releases/tag/v1.0.0)提供 Go 一键离线包和静态 Go 程序。文末保留早期 Node.js 预览包说明，详见[迁移清单](GO_UI_MIGRATION.md)。

V1.0.0 延续 Docker Engine 27 导入镜像后的 ID 规范化，并增加正式 V0.9.0 安装包 → V1.0.0 → V0.9.0 的无外网守护进程升级/回退验收，同一 PG18 数据记录须在三阶段保留。物理断网服务器仍需在部署方环境验收。

## Go 架构 V1.0.0 部署

从[V1.0.0 发布页](https://github.com/cuipengdba/DbSphere/releases/tag/v1.0.0)下载 `DbSphere-go-offline-1.0.0-linux-amd64.run` 及同名 `.sha256`，复制到已安装 Docker Engine 与 Compose v2 的 Linux x86_64 目标机同一目录，运行：

```bash
bash DbSphere-go-offline-1.0.0-linux-amd64.run
```

无 Docker 环境可下载同页的 `DbSphere-go-1.0.0-linux-amd64` 静态服务端，但需自行提供独立的 PostgreSQL 18。运行前设置 `DBSPHERE_DATABASE_URL`、不少于 12 字符的 `DBSPHERE_ADMIN_PASSWORD` 和随机生成的 32 字节 base64 `DBSPHERE_MASTER_KEY`，并将主密钥安全持久化以供重启使用；`DBSPHERE_HOST` 和 `DBSPHERE_PORT` 默认分别为 `127.0.0.1` 与 `8080`。校验同名 `.sha256` 后执行 `chmod +x DbSphere-go-1.0.0-linux-amd64 && ./DbSphere-go-1.0.0-linux-amd64`。三个发布产物及对应 SHA-256 文件均来自同一发布提交的验收构建。

源码仓库现有 `Dockerfile-go`：`CGO_ENABLED=0` 构建后端，嵌入静态前端，最终镜像只含 Go 程序和 CA 证书。默认 Compose 配套独立 PostgreSQL 18.7 Bookworm，使用[固定的官方镜像摘要](GO_DEPENDENCY_AUDIT.md)；连接已有 PG18 可改用 `deploy/compose/compose.go.external.yaml`。联网 Linux 构建机可运行 `bash scripts/one-click-go.sh`，首次创建独立的 `deploy/compose/.env.go` 随机凭据，默认绑定 `127.0.0.1:8081`；该开发环境使用单独的 Compose 项目和数据卷。更换 `DBSPHERE_PG18_IMAGE` 会改变在线部署的存储镜像，须独立验证 PG18 数据卷的升级与回滚。

Go 一键离线包由联网 Linux amd64 或 arm64 构建机执行 `bash scripts/build-go-offline-bundle.sh /absolute/output/directory` 制作，输出与构建机相同架构的包。目标机预装 Docker Engine/Compose v2 后，将生成的 `DbSphere-go-offline-*.run` 和同名 `.run.sha256` 放在同一目录，再运行 `.run`。安装器先核对校验文件和包内架构，再加载静态 Go 应用镜像与固定摘要的 PG18 镜像。Compose 以 `--no-build --pull never` 启动。首次安装在 `${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go/dbsphere-go.env` 生成随机凭据，默认端口 8081；`DBSPHERE_GO_CONFIG_DIR` 可指定独立配置目录。两种架构都经无外网 Docker 守护进程验收，覆盖升级、回退和历史存储保留。管理员密码轮换及 PG18 备份恢复在 Linux amd64 发布 CI 中验收。通过发布页取得校验文件，以 SHA-256 核对传输完整性。[安全审查](V1_SECURITY_REVIEW.md)

新增的 [Linux 无网络安装验收作业](https://github.com/cuipengdba/DbSphere/actions/runs/44426339798/job/135232046226)先在联网阶段构建测试容器，再移除本地 Go/PG18 镜像标签，把原始 `.run` 放进 `--network none` 的容器执行。测试容器仅通过本机 Docker socket 操作预装的 Docker Engine；安装器从包内加载镜像，Compose 使用 `--pull never`，端口 18081 的 `/readyz` 报告 `postgresql18`。这证明安装器进程不需要外网，且在该 Linux CI 环境可完成一键部署；Docker 守护进程所在主机仍有网络，不能将其等同于一台物理断网目标机的完整安装/升级/回滚验收。

Go 原生进程设置 `DBSPHERE_ENABLE_GO_COLLECTOR=true` 后启动自动采集，一键 Compose 模式默认开启；`DBSPHERE_COLLECT_INTERVAL_SECONDS` 可设为 15–3600 秒，`DBSPHERE_COLLECT_WORKERS` 可设为 1–16（默认 4）。每轮最多选取 100 个到期资产，按工作数并行，每个资产有独立 45 秒上限。请按目标实例及 PG18 监控库的负载调整工作数。多服务副本的跨实例采集租约尚未实现，当前仅在一个 DbSphere 副本启用自动采集。

Go 版 PG18 历史数据清理默认关闭。按需在 `deploy/compose/.env.go` 或离线包的 `dbsphere-go.env` 中配置 `DBSPHERE_RETENTION_DAYS=90`（样本）及 `DBSPHERE_EVENT_RETENTION_DAYS=180`（已结事件），可只启用其中一项；取值 1–3650。重新运行一键脚本后生效。每轮限量清理，保留仍被事件证据或巡检引用的样本，以及未结事件、未完成通知和巡检关联事件；实际保留时间可能超过设置值。开启前请备份 PG18，详见[存储记录](STORAGE_V0.3.md)。

Go 版额外使用 `scripts/go-offline-upgrade-rollback-smoke.sh` 在 Linux CI 演练旧 Go 包 → 当前包 → 旧 Go 包，复用独立 PG18 数据卷，并逐段读取同一资产、样本和事件 ID。[Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44232205949) 已通过该演练；后续 schema 变更必须另做前向迁移和可回滚性验证。

外部 PG18 模式使用同一安装包。应用和网关的 `/readyz` 只在长期存储有可写 PG18 主节点时返回成功。存储暂时不可用时，控制台保留已读内容并显示状态，恢复后自动刷新。应用会经过共享 PG18 继续读取原资产、样本、事件和独立账号会话。

### 双副本离线模式

首次安装时以 `DBSPHERE_GO_HA=true ./DbSphere-go-offline-<版本>-linux-<架构>.run` 启动，已有安装可在 `dbsphere-go.env` 加入 `DBSPHERE_GO_HA=true` 后重运行安装包。双副本模式需要 Docker Compose v2.24.4 或更新版本。网关使用同一 Go 镜像，并沿用 `DBSPHERE_WEB_PORT`。在「系统状态 → 平台副本」查看在线副本、采集轮次、当前维护主节点和租约。双副本和网关在同一台 Docker 主机运行。

跨主机可将应用实例接入同一外置负载均衡器和 PG18 可写主节点入口，例如已经部署并验收的 Patroni 写入口。副本使用相同的 `DBSPHERE_MASTER_KEY`、目标采集周期、通知和作业配置。外置模式要在相应副本设置 `DBSPHERE_ENABLE_GO_COLLECTOR=true`。网关或外置负载均衡器可用 `/readyz` 路由，`/healthz` 可用于进程存活检查。采集租约未完成时由下一副本在到期后接手。Webhook 的 `Idempotency-Key` 和 `X-DbSphere-Delivery-Id` 在重试时一致，接收端可据此去重。[Compose 合并规则](https://docs.docker.com/reference/compose-file/merge/)

Go 离线包内置随包 PG18 的备份命令：`bash DbSphere-go-offline-<版本>-linux-amd64.run --backup /absolute/backup/directory`。命令校验离线包和唯一运行的 Go PG18 容器，以 PG18 `pg_dump -Fc` 生成归档，`pg_restore --list` 验证可读性，并输出带 SHA-256 的 JSON 清单。源码一键部署可运行 `bash scripts/backup-go-store.sh /absolute/backup/directory`。外部 PG18 不由此命令备份，应使用其自身的 PG18 备份体系。此能力仅备份数据库；离线恢复到新 PG18 与升级/回滚联动仍须通过 CI 和生产演练，不应只凭清单认定备份可恢复。

在「系统状态 → 账号和团队」创建项目团队，填写团队负责的资产 UUID，再为独立账号分配角色和团队。管理员可全局管理。值班员可以确认、交接其授权事件并提交工单，独立审批人可以批准或驳回授权范围内他人提交的工单，只读人员可以查看授权资产和证据。新设密码至少 12 字符。停用账号或轮换凭据会撤销该账号已有的会话。团队清单为空的非管理员账号拥有零资产访问权。

### 组织、单点登录和审计

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

管理员密码只在空库首次启动时初始化；仅修改 `DBSPHERE_ADMIN_PASSWORD` 并重启不会轮换。要轮换现有安装，先在权限为 `0600` 的 `dbsphere-go.env` 中更改该值，再运行同一离线包的 `--rotate-admin-password` 参数。安装器在已运行的 PG18 存储中更新密码哈希，随后重建 Go 应用容器；旧管理员会话随密码哈希变化失效。源码 Compose 部署可在修改 `deploy/compose/.env.go` 后执行 `docker compose --env-file deploy/compose/.env.go -f deploy/compose/compose.go.yaml run --rm --no-deps app rotate-admin-password`，再重建应用容器。不要轮换 `DBSPHERE_MASTER_KEY`，它用于解密已有资产凭据。

Go 版需要访问 K8S API 时，可在 `deploy/compose/.env.go`（联网开发部署）或 `${DBSPHERE_GO_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go}/dbsphere-go.env`（离线包）追加 `DBSPHERE_KUBECONFIG_HOST_PATH=/absolute/path/to/read-only-kubeconfig` 后重新执行一键脚本。脚本校验文件存在并只读挂载；配置需使用静态 ServiceAccount token 和受信 CA，不执行 kubeconfig 中的 `exec` 凭据插件。Go K8S 适配器支持已映射 Pod 的状态、Node、Service、PVC/PV、Pod 事件与可用的 metrics.k8s.io；本机 kind 已完成只读采集与故障闭环，[验收边界见记录](KIND_ACCEPTANCE.md)，生产集群仍待验证。

如已有 Prometheus 抓取 kubelet 的 `kubelet_volume_stats_used_bytes`，可在同一配置文件设置 `DBSPHERE_PROMETHEUS_URL=https://prometheus.example.internal`，需要 Bearer 令牌时再设置 `DBSPHERE_PROMETHEUS_TOKEN`，并重新运行一键脚本。此项可选；未配置或指标缺失时 PVC 使用率保持未知，不会误判为健康。Go 适配器按资产映射的 Namespace/PVC 查询，不接收浏览器指定的 Prometheus 地址。生产 Prometheus 仍待验收。

Go 告警通知也是可选配置。在同一配置文件设置 `DBSPHERE_WEBHOOK_URL=https://...`，可加 `DBSPHERE_WEBHOOK_SECRET` 启用 HMAC-SHA256 签名；邮件需同时设置 `DBSPHERE_SMTP_HOST`、`DBSPHERE_SMTP_FROM`、`DBSPHERE_SMTP_TO`，可选端口及用户/密码。默认邮件端口 587 并要求 STARTTLS，465 使用 TLS；仅环回 SMTP 可设置 `DBSPHERE_SMTP_ALLOW_PLAIN=true`。`DBSPHERE_NOTIFY_MIN_SEVERITY` 默认 `warning`，可按 `DBSPHERE_WEBHOOK_MIN_SEVERITY`、`DBSPHERE_EMAIL_MIN_SEVERITY` 分通道覆盖。未配置通道的投递记录标记为跳过；失败投递最多自动重试五次，之后可在 B/S 页面手动重试。生产通知通道仍待验收。

应用 Trace 接入默认关闭。需要时在同一配置文件设置不少于 32 字符的 `DBSPHERE_OTLP_TOKEN`，重启后将应用或 OpenTelemetry Collector 的 OTLP/HTTP JSON Trace 发送到 `POST /v1/traces`，使用 `Authorization: Bearer <token>`。仅与唯一纳管 PG 资产匹配的 Trace 入库，不保存 SQL、参数、URL 或任意未经允许的 Span 属性；可选白名单字段 `dbsphere.trace.sampling_probability` 仅作为发送方采样声明保存，详见[Trace 时间可信度](TRACE_TIME_COVERAGE.md)。`DBSPHERE_TRACE_RETENTION_DAYS` 默认 7 天，Go 工作器每小时最多清理 10000 条过期 Span；生产入口应使用 TLS。不同 SDK 的 `http/json` 支持仍需分别验证。

## 历史 Node.js 开发预览包

早期 [V0.6 Node.js 开发预览](https://github.com/cuipengdba/DbSphere/releases/tag/v0.6.0-dev)提供了两种 Linux x86_64 安装包及各自的 SHA-256 文件。新部署请使用上面的 Go V1.0.0 正式版产物。

从 V0.6 起，DbSphere 提供可在**目标服务器断网**时运行的 `.run` 安装包。包内含预构建的 DbSphere 应用镜像、PostgreSQL 18 镜像、Compose 配置和校验清单。目标机无需安装 Node.js、pnpm 或下载依赖；需事先具备 **Linux x86_64、Docker Engine 和 Docker Compose v2**。应用仍采用 B/S 架构，监控数据仍写入独立 PG18。监控目标是否可达由部署网络决定；“离线部署”只保证安装阶段不连接软件仓库或镜像仓库。

## 在联网构建机制作安装包

构建机需要 Linux x86_64、Docker Engine/Compose 和访问基础镜像、npm 包仓库的网络。仓库根目录执行：

```bash
bash scripts/build-offline-bundle.sh /absolute/output/directory
```

输出 `DbSphere-offline-<版本>-linux-amd64.run` 与同名 `.sha256`。构建脚本在联网机完成 `docker build`，通过 Docker 的[镜像保存格式](https://docs.docker.com/reference/cli/docker/image/save/)封装应用和 PG18 镜像；包中没有生产凭据。归档应通过受信渠道传输，不应直接将包含初始凭据的配置文件打入包中。

## 在断网目标机一条命令安装

把 `.run` 与 `.sha256` 复制到目标机同一目录，然后执行：

```bash
bash DbSphere-offline-<版本>-linux-amd64.run
```

脚本先检查包校验和、Linux 架构和 Docker/Compose，随后用 [`docker image load`](https://docs.docker.com/reference/cli/docker/image/load/)导入镜像。Compose 以 `--no-build --pull never` 启动 DbSphere 与独立 PG18；缺镜像时会失败，不会联网构建或拉取。首次运行在 `${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere/dbsphere.env` 生成随机管理员密码、PG18 密码和主密钥，并设置为仅当前用户可读。Compose 等待 PG18 和 B/S 健康检查完成后才报告就绪。默认仅绑定 `127.0.0.1:8080`；对外服务需自行配置 HTTPS 代理。再次运行同包会复用配置和 Compose 卷。

可设绝对路径 `DBSPHERE_CONFIG_DIR` 将配置文件放在指定目录。使用外部独立 PG18 时，在首次启动前创建该目录下的 `dbsphere.env`，设置 `DBSPHERE_DATABASE_URL`、`DBSPHERE_ADMIN_PASSWORD`、32 字节 base64 `DBSPHERE_MASTER_KEY` 和 Web 端口，然后运行同一 `.run`。已有 K8S kubeconfig、pgBackRest 状态目录或 PG18 备份清单可分别在配置中设置 `DBSPHERE_KUBECONFIG_HOST_PATH`、`DBSPHERE_BACKUP_INFO_HOST_PATH`、`DBSPHERE_STORE_BACKUP_HOST_PATH`，脚本验证绝对路径并只读挂载。

开发版可在 `dbsphere.env` 追加 `DBSPHERE_OTLP_TOKEN`（至少 32 字符）开启可选 OTLP/HTTP JSON Trace 接入，`DBSPHERE_TRACE_RETENTION_DAYS` 默认 7。应用/Collector 指向 B/S 地址的 `/v1/traces`，使用 Bearer 令牌并设置 `http/json` 协议；当前仅适合将 Trace 通过可信 TLS 入口发送。离线部署的 PG/Linux/K8S 监控无需该配置。详见[Trace 开发记录](TRACE_V0.7_PROGRESS.md)。

## 升级、回滚与边界

历史 Node `.run` 复用同一配置目录和 `dbsphere` Compose 卷，应用镜像随版本切换。[Linux CI 演练](https://github.com/cuipengdba/DbSphere/actions/runs/44058038127)曾使用真实 V0.5 应用镜像构造旧离线包，建资产后升级到 V0.6 开发版、再回到 V0.5 应用，三阶段均在同一 PG18 存储读到该资产。新部署和升级应使用上文的 Go V1.0.0 包；升级前备份 PG18 并保存当前安装包，涉及未来 schema 变更时需另验回滚兼容或先恢复备份。

## 可选的 Node.js 轻量离线包

已有独立 PG18、且不希望安装 Docker 的 Linux x86_64 环境，可在联网构建机运行：

```bash
bash scripts/build-node-offline-bundle.sh /absolute/output/directory
```

生成 `DbSphere-node-<版本>-linux-amd64.tar.gz` 与 `.sha256`。包中有生产依赖、服务端、静态页面和启动脚本；目标机只需预装 Node.js **24.13 或更高版本**与外部 PostgreSQL 18，不执行 `pnpm install`。解压后第一次以前台方式启动：

```bash
DBSPHERE_DATABASE_URL='postgres://user:password@pg18-host:5432/dbsphere' bash run.sh
```

脚本将随机管理员密码和主密钥存于 `${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-native/dbsphere.env`，再次运行复用配置。进程以前台运行，生产环境应交给 systemd 等服务管理器管理；此模式不附带 PG18，也不宣称为零依赖安装。

当前 `.run` 是自解压离线镜像包，**不是静态编译的单文件服务端**。PostgreSQL 18 作为独立容器运行，Docker 是明确的目标机依赖。Node.js 轻量包则需要现场安装 Node.js 与 PG18。Node.js 的[单文件应用能力](https://r2.nodejs.org/docs/latest-v24.x/api/single-executable-applications.html)仍处于开发阶段；只有完整打包、运行和升级演练通过后，才会另行发布单文件执行程序。
