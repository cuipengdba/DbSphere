# DbSphere V0.6 开发预览：离线部署

此预览版仅用于验证新增的 Linux x86_64 离线交付。**V0.6 的跨层证据图、确定性根因规则、故障演练尚未完成；这不是 V0.6 正式版，也不是 V1.0。**既有的 PG14–18、Linux/K8S 监控能力及独立 PG18 长期存储延续 V0.5。

## 下载与安装

发布资产有两种，均提供同名 `.sha256`：

- `DbSphere-offline-0.6.0-dev-linux-amd64.run`：目标 Linux x86_64 预装 Docker Engine/Compose v2；把 `.run` 和 `.sha256` 放同一目录，运行 `bash DbSphere-offline-0.6.0-dev-linux-amd64.run`。包内含应用和独立 PostgreSQL 18 镜像，目标机无需 Node.js、pnpm 或联网拉取。首次启动生成随机凭据，重复执行复用数据卷。
- `DbSphere-node-0.6.0-dev-linux-amd64.tar.gz`：目标 Linux x86_64 预装 Node.js 24.13+ 并提供独立 PostgreSQL 18；解压后设置 `DBSPHERE_DATABASE_URL`，运行 `bash run.sh`。包内含生产依赖，无需在目标机运行包管理器。

完整配置和边界见[离线部署说明](OFFLINE_DEPLOYMENT.md)。这些包不是静态编译的单文件服务端；当前只验证 x86_64。Docker 版部署仍需容器运行时，Node 版仍需 Node.js 和外部 PG18。暂未验收旧版本数据的离线升级与回滚，生产数据库升级前必须备份并在独立环境演练。

## 验证

- [CI 13 作业通过](https://github.com/cuipengdba/DbSphere/actions/runs/44056141672)：Docker 包删除本机镜像后仅从本地包导入，B/S 与 PG18 就绪，二次运行通过；Node 包解压后直接启动，外部 PG18 就绪。
- [预览资产构建](https://github.com/cuipengdba/DbSphere/actions/runs/44056371453)：两种包的外部 SHA-256 校验通过。

V0.6 正式版仍须完成[开发任务 06-01 至 06-06](DEVELOPMENT_TASKS.md)；V1.0 须完成离线升级、回滚及故障演练门槛。
