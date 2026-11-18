# DbSphere V0.2 容器化一键部署

> 本文对应 V0.2.0；默认和外部 PG18 Compose、更新和存储故障已通过 [Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44020986662)。生产规模的备份恢复演练仍待完成。V0.1.0 的手动启动见[部署与接入](DEPLOYMENT.md)。

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

PG18 存储是 DbSphere 自身的监控库，必须与被纳管的业务库隔离。外部 PG18 一键启动与更新已通过 Linux CI；分区、保留期、备份恢复和真实生产规模升级验收尚未完成。连接 PostgreSQL 14–18 监控目标的只读权限和 K8S 凭据说明仍见[部署与接入](DEPLOYMENT.md)。

### Kubernetes 只读发现与指标

浏览器选择 K8S 运行环境，填写 kubeconfig context 和 Namespace 后可查询该 Namespace 的 Pod，并按容器镜像/5432 端口提示可能的 PostgreSQL Pod。选择后会填入 Pod、Workload 与 Node，列出匹配的 Service。数据库地址须填写从 DbSphere 运行位置实际可达的地址；集群外的 Compose 容器通常不能直接解析 `.svc` 域名。每类资源最多读取 100 项；结果截断或缺少权限会显示具体原因。Pod 重建后应重新确认映射。

Namespace 内的 ServiceAccount 需要 `pods`、`persistentvolumeclaims` 的 `get,list`，`services`、`events` 的 `list`，以及 `deployments`、`replicasets`、`statefulsets` 的 `list`。关联 Node 和 PV 需额外在 ClusterRole 授予 `nodes`、`persistentvolumes` 的 `get`。若集群提供 metrics-server，Go 采集器按指定 Pod/Node 发起 `get`，因此 Namespace 中 `metrics.k8s.io/pods` 和 ClusterRole 中 `metrics.k8s.io/nodes` 都需 `get`；需要兼容列表发现时可同时授予 `list`。本机 kind 曾因示例仅含 `list` 返回 403，补充只读 `get` 后实际采到了 Pod/Node 资源指标。测试用配置见 [`k8s-postgres.yaml`](../test/fixtures/k8s-postgres.yaml)，其中 PostgreSQL 测试密码不得用于生产。

Go 版可只配置单 Pod Deployment/StatefulSet 的 Workload 名称；初始 Pod 被重建时也能按该 Workload 的 selector 与 owner UID 解析唯一新 Pod。发现 Workload、ReplicaSet、Pod 时使用 Kubernetes `continue` 令牌逐页读取，每页最多 100 项、每类最多 10 页；超过上限、令牌循环或多个就绪 Pod 时返回映射错误，不猜测实例。此路径需要上面的 `pods`、`deployments`、`replicasets`、`statefulsets` 列表权限；多副本主备应分别纳管并明确 Pod/实例身份。

在 Compose 模式使用 K8S 时，将只读 kubeconfig 保存为 Linux 主机上仅运维账号可读的文件，并在 `deploy/compose/.env` 增加 `DBSPHERE_KUBECONFIG_HOST_PATH=/绝对路径/readonly.kubeconfig`，再运行 `bash scripts/one-click.sh`。脚本验证文件存在后将其只读挂载到应用容器。kubeconfig 的 context 名须与资产中填写的集群名一致；DbSphere 禁止执行其中的外部认证插件。Kubernetes API 地址须从应用容器可达，ServiceAccount token 应按集群策略轮换。

metrics-server 提供 Pod/Node 资源使用量，不提供 PVC 文件系统实际用量。若已有 Prometheus 抓取 kubelet 的 `kubelet_volume_stats_used_bytes`，可在 `.env` 设置 `DBSPHERE_PROMETHEUS_URL=https://prometheus.example.internal`，如需 bearer token 再设置 `DBSPHERE_PROMETHEUS_TOKEN`。DbSphere 对每个关联 PVC 发起只读即时查询，限制 4 秒和 1 MiB 响应；实际用量与 PVC 申请量、容量分别展示。Prometheus 地址由管理员在部署配置中设置，不能从资产表单指定。如没有 metrics-server、Prometheus、权限或目标指标，DbSphere 会记录未采集原因，PG 侧监控继续工作。

### 可选 PgBouncer 观测

若 PostgreSQL 前面有 PgBouncer，可在资产中填写指向其**管理数据库**的连接 URL，如 `postgres://stats_user:URL编码后的密码@pooler.internal:6432/pgbouncer`。按 [PgBouncer 官方管理控制台说明](https://www.pgbouncer.org/usage)，给该用户配置 `stats_users`；DbSphere 只运行 `SHOW POOLS` 和 `SHOW STATS`，不会执行暂停、重载或关闭命令。连接串加密保存在 PG18 监控库，浏览器只看到“已配置”标志；编辑时留空会保留原值，勾选清除才移除。

统计按资产的数据库名过滤，显示等待客户端、活跃/空闲服务端连接和累计查询。若 PgBouncer 无对应数据库或统计权限不足，界面明确显示未关联或采集错误；不影响 PostgreSQL、Linux 和 K8S 采样。请将管理端口限定在可信网络。
