# DbSphere V0.2 容器化一键部署（开发中）

> 本文对应开发分支上的 V0.2 功能；正式发行仍需 Compose 与 PG18 CI、升级/恢复演练通过。V0.1.0 的手动启动见[部署与接入](DEPLOYMENT.md)。

## 一条命令启动

在 Linux 主机上安装 Docker Engine 与 Docker Compose v2，克隆仓库后运行：

```bash
bash scripts/one-click.sh
```

脚本首次运行在 `deploy/compose/.env` 生成强随机管理员密码、PG18 存储密码和 32 字节主密钥，权限为 `0600`。首次密码仅在终端显示一次；请安全保存 `.env` 和数据库备份。之后运行会复用密钥与数据。Compose 启动两个容器：DbSphere B/S 服务和独立 PostgreSQL 18。PG18 端口不映射到宿主机，Web 默认仅绑定宿主机 `127.0.0.1:8080`。

可在 `.env` 中调整 `DBSPHERE_WEB_PORT`、采集间隔以及 HTTPS 代理场景的 `DBSPHERE_COOKIE_SECURE=true`，然后再次运行脚本。远程浏览器访问需由 HTTPS 反向代理转发，或使用 SSH 隧道。`GET /healthz` 检查进程，`GET /readyz` 检查 PG18 存储可用性。普通监控目标的故障不会使 `/readyz` 失败。

Docker 命名卷 `dbsphere_pg18-data` 保存长期数据，`dbsphere_app-data` 保存应用本地配置目录。`docker compose down` 不删除命名卷；不要使用 `down -v` 清理包含正式数据的卷。当前版本尚未实现自动备份。

### 使用已有的独立 PostgreSQL 18

如果已有单独的 PG18 存储实例，先为 DbSphere 创建专属数据库和账号，并确保容器能访问。首次运行脚本生成 `.env` 后，加入一行 `DBSPHERE_DATABASE_URL=postgres://用户:URL编码后的密码@主机:5432/数据库`，再次运行脚本会只启动 DbSphere 应用容器。目标必须是 PostgreSQL 18，连接失败或版本不符会使启动明确失败。请通过网络策略、TLS 和数据库权限保护这个连接；不得指向被纳管的业务库。

## 从 V0.1 SQLite 迁移

先停止 V0.1 服务并备份它的整个数据目录，然后运行：

```bash
bash scripts/one-click.sh --migrate-from /absolute/path/to/v0.1/data
```

脚本先启动独立 PG18，导入 SQLite 中的设置、资产、样本、事件和证据，校验条数，并使用新的主密钥重新加密监控账号密码；目标 PG18 若已有资产或历史样本会拒绝导入，避免合并冲突。迁移后管理员继续使用 V0.1 的密码登录。原 SQLite 文件保持不变，便于核对和回退。若 V0.1 使用环境变量而不是 `master.key` 文件保存主密钥，迁移前须提供 `DBSPHERE_OLD_MASTER_KEY`（原 32 字节密钥的 base64 值）。

迁移过程应在停写窗口进行。当前脚本按记录逐行复制，适用于 V0.1 的小规模预览数据；正式大规模导入、回滚和断点恢复尚待验证。迁移成功后先核对资产数、样本数、事件证据与登录，再切换入口。

## 当前边界

Linux 主机增强采集可按 [Prometheus 官方指南](https://prometheus.io/docs/guides/node-exporter/)在目标主机安装 `node_exporter`，资产中填写 Linux 主机名和 exporter 端口（默认常见端口 9100）。DbSphere 只读取 `/metrics`，设置 4 秒超时和 5 MiB 响应上限，不在目标主机执行脚本；请将 exporter 限制在可信内网，不要直接暴露到公网。未配置时界面明确显示“未采集”。CPU 使用率与 IO/网络速率由相邻两次累计计数计算，首次采样仅显示累计量。

PG18 存储是 DbSphere 自身的监控库，必须与被纳管的业务库隔离。V0.2 仍在开发，分区、保留期、备份恢复与外部 PG18 模式的端到端验收将继续补齐。连接 PostgreSQL 14–18 监控目标的只读权限和 K8S 凭据说明仍见[部署与接入](DEPLOYMENT.md)。
