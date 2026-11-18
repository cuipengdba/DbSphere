# Linux 一键离线部署

> 当前页面描述迁移前 Node.js 开发预览包。2026-11-17 起正式版后端要求改为 Go；Go 版离线包验收前，这份预览不代表新架构的正式交付。详见[迁移清单](GO_UI_MIGRATION.md)。

## Go 架构开发版部署

源码仓库现有 `Dockerfile-go`：`CGO_ENABLED=0` 构建后端，嵌入静态前端，最终镜像只含 Go 程序和 CA 证书。默认 Compose 配套独立 PostgreSQL 18，连接已有 PG18 可改用 `deploy/compose/compose.go.external.yaml`。联网 Linux 构建机可运行 `bash scripts/one-click-go.sh`，首次创建独立的 `deploy/compose/.env.go` 随机凭据，默认绑定 `127.0.0.1:8081`；该开发环境使用单独的 Compose 项目和数据卷。

Go 一键离线包由联网 Linux x86_64 构建机执行 `bash scripts/build-go-offline-bundle.sh /absolute/output/directory` 制作。目标 Linux x86_64 主机预装 Docker Engine/Compose v2 后，运行生成的 `DbSphere-go-offline-*.run`；包内含静态 Go 应用镜像、PG18 镜像、Compose 文件和 SHA-256 清单，安装时仅从包中导入镜像，并用 `--no-build --pull never` 启动。首次安装在 `${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go/dbsphere-go.env` 生成随机凭据，默认端口 8081；`DBSPHERE_GO_CONFIG_DIR` 可指定独立配置目录。[21 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44088758026)已验证删除本机镜像后的离线安装、健康接口及重复运行；本地 Go 容器也验证了首次管理员登录。Go 后端功能对等、真实故障演练及未来 schema 变更的升级回滚尚未完成，因此这是开发版交付路径，不是 V1.0 发布包。

Go 原生进程设置 `DBSPHERE_ENABLE_GO_COLLECTOR=true` 后启动自动采集，一键 Compose 模式默认开启；`DBSPHERE_COLLECT_INTERVAL_SECONDS` 可设为 15–3600 秒，`DBSPHERE_COLLECT_WORKERS` 可设为 1–16（默认 4）。每轮最多选取 100 个到期资产，按工作数并行，每个资产有独立 45 秒上限。请按目标实例及 PG18 监控库的负载调整工作数。多服务副本的跨实例采集租约尚未实现，当前仅在一个 DbSphere 副本启用自动采集。

Go 版额外使用 `scripts/go-offline-upgrade-rollback-smoke.sh` 在 Linux CI 演练旧 Go 包 → 当前包 → 旧 Go 包，复用独立 PG18 数据卷，并逐段读取同一资产、样本和事件 ID。[Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44232205949) 已通过该演练；后续 schema 变更必须另做前向迁移和可回滚性验证。

Go 离线包内置随包 PG18 的备份命令：`bash DbSphere-go-offline-<版本>-linux-amd64.run --backup /absolute/backup/directory`。命令校验离线包和唯一运行的 Go PG18 容器，以 PG18 `pg_dump -Fc` 生成归档，`pg_restore --list` 验证可读性，并输出带 SHA-256 的 JSON 清单。源码一键部署可运行 `bash scripts/backup-go-store.sh /absolute/backup/directory`。外部 PG18 不由此命令备份，应使用其自身的 PG18 备份体系。此能力仅备份数据库；离线恢复到新 PG18 与升级/回滚联动仍须通过 CI 和生产演练，不应只凭清单认定备份可恢复。

如需 Go 控制台的只读查看账号，在 Go 配置文件 `dbsphere-go.env` 中增加 `DBSPHERE_VIEWER_PASSWORD`（至少 12 字符，且不同于管理员密码），然后重新运行同一安装包。更改该值并重启会轮换密码；移除该行并重启会停用账号，旧查看会话失效。默认可读取全部资产；如需限定范围，再设置 `DBSPHERE_VIEWER_ASSET_IDS=资产UUID1,资产UUID2` 并重启。资产 UUID 可从管理员登录后的资产列表或 URL 取得；白名单中填写不存在的 UUID 不会授予访问。受限账号无法读取全平台存储统计或修改任何数据。当前只有一个查看账号，尚无多用户/组织 RBAC。

管理员密码只在空库首次启动时初始化；仅修改 `DBSPHERE_ADMIN_PASSWORD` 并重启不会轮换。要轮换现有安装，先在权限为 `0600` 的 `dbsphere-go.env` 中更改该值，再运行同一离线包的 `--rotate-admin-password` 参数。安装器在已运行的 PG18 存储中更新密码哈希，随后重建 Go 应用容器；旧管理员会话随密码哈希变化失效。源码 Compose 部署可在修改 `deploy/compose/.env.go` 后执行 `docker compose --env-file deploy/compose/.env.go -f deploy/compose/compose.go.yaml run --rm --no-deps app rotate-admin-password`，再重建应用容器。不要轮换 `DBSPHERE_MASTER_KEY`，它用于解密已有资产凭据。

Go 版需要访问 K8S API 时，可在 `deploy/compose/.env.go`（联网开发部署）或 `${DBSPHERE_GO_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go}/dbsphere-go.env`（离线包）追加 `DBSPHERE_KUBECONFIG_HOST_PATH=/absolute/path/to/read-only-kubeconfig` 后重新执行一键脚本。脚本校验文件存在并只读挂载；配置需使用静态 ServiceAccount token 和受信 CA，不执行 kubeconfig 中的 `exec` 凭据插件。Go K8S 适配器支持已映射 Pod 的状态、Node、Service、PVC/PV、Pod 事件与可用的 metrics.k8s.io，真实集群验收尚未完成。

如已有 Prometheus 抓取 kubelet 的 `kubelet_volume_stats_used_bytes`，可在同一配置文件设置 `DBSPHERE_PROMETHEUS_URL=https://prometheus.example.internal`，需要 Bearer 令牌时再设置 `DBSPHERE_PROMETHEUS_TOKEN`，并重新运行一键脚本。此项可选；未配置或指标缺失时 PVC 使用率保持未知，不会误判为健康。Go 适配器按资产映射的 Namespace/PVC 查询，不接收浏览器指定的 Prometheus 地址。生产 Prometheus 仍待验收。

Go 告警通知也是可选配置。在同一配置文件设置 `DBSPHERE_WEBHOOK_URL=https://...`，可加 `DBSPHERE_WEBHOOK_SECRET` 启用 HMAC-SHA256 签名；邮件需同时设置 `DBSPHERE_SMTP_HOST`、`DBSPHERE_SMTP_FROM`、`DBSPHERE_SMTP_TO`，可选端口及用户/密码。默认邮件端口 587 并要求 STARTTLS，465 使用 TLS；仅环回 SMTP 可设置 `DBSPHERE_SMTP_ALLOW_PLAIN=true`。`DBSPHERE_NOTIFY_MIN_SEVERITY` 默认 `warning`，可按 `DBSPHERE_WEBHOOK_MIN_SEVERITY`、`DBSPHERE_EMAIL_MIN_SEVERITY` 分通道覆盖。未配置通道的投递记录标记为跳过；失败投递最多自动重试五次，之后可在 B/S 页面手动重试。生产通知通道仍待验收。

应用 Trace 接入默认关闭。需要时在同一配置文件设置不少于 32 字符的 `DBSPHERE_OTLP_TOKEN`，重启后将应用或 OpenTelemetry Collector 的 OTLP/HTTP JSON Trace 发送到 `POST /v1/traces`，使用 `Authorization: Bearer <token>`。仅与唯一纳管 PG 资产匹配的 Trace 入库，不保存 SQL、参数、URL 或任意 Span 属性。`DBSPHERE_TRACE_RETENTION_DAYS` 默认 7 天，Go 工作器每小时最多清理 10000 条过期 Span；生产入口应使用 TLS。不同 SDK 的 `http/json` 支持仍需分别验证。

当前可从 [V0.6 开发预览](https://github.com/cuipengdba/DbSphere/releases/tag/v0.6.0-dev) 下载两种 Linux x86_64 安装包及各自的 SHA-256 文件。V0.6 的跨层根因分析仍在开发中。

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

后续版本的 `.run` 复用同一配置目录和 `dbsphere` Compose 卷，应用镜像随版本切换。[Linux CI 演练](https://github.com/cuipengdba/DbSphere/actions/runs/44058038127)已使用真实 V0.5 应用镜像构造旧离线包，建资产后升级到 V0.6 开发版、再回到 V0.5 应用，三阶段均在同一 PG18 存储读到该资产。这只验证了目前两版的应用切换和该数据路径。升级前仍应使用[PG18 存储备份与恢复演练](INSPECTIONS_V0.4.md)备份数据并保存当前安装包；未来 schema 变更的回滚需单独验证兼容或先恢复备份。V1.0 将正式验收断网升级与回滚。

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
