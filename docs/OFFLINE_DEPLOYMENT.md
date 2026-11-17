# Linux 一键离线部署

> 当前页面描述迁移前 Node.js 开发预览包。2026-11-17 起正式版后端要求改为 Go；Go 版离线包验收前，这份预览不代表新架构的正式交付。详见[迁移清单](GO_UI_MIGRATION.md)。

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
