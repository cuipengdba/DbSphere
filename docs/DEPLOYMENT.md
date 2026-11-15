# DbSphere V0.1 部署与接入

## 运行条件

- Linux x86_64 或 arm64，Node.js 24.13+、pnpm 11；或运行 Linux 容器的 Docker。
- 浏览器访问 DbSphere HTTP 服务。跨主机访问必须在可信的 HTTPS 反向代理后面，并设置 `DBSPHERE_COOKIE_SECURE=true`。
- DbSphere 到 PostgreSQL 14–18 的 TCP 可达；Kubernetes 实例需要额外的 K8S API 只读凭据。
- 持久化目录应由 DbSphere 进程独占，权限 `0700`，定期备份 `dbsphere.sqlite` 和主密钥。主密钥丢失后，已加密的数据库凭据无法解密。

## Linux 原生运行

```bash
corepack enable
pnpm install --prod --frozen-lockfile
install -d -m 0700 /var/lib/dbsphere
export DBSPHERE_DATA_DIR=/var/lib/dbsphere
export DBSPHERE_ADMIN_PASSWORD='替换成不少于12位的独立密码'
pnpm start
```

默认监听 `127.0.0.1:8080`。首次启动后，管理员密码以 scrypt 哈希保存；后续环境变量不会重置已有密码。`GET /healthz` 返回服务存活状态。可设置 `DBSPHERE_HOST`、`DBSPHERE_PORT` 和 `DBSPHERE_COLLECT_INTERVAL_SECONDS`（最小 15 秒）。

建议通过独立密钥管理渠道设置 `DBSPHERE_MASTER_KEY`（32 字节随机值的 base64 编码），并保持跨重启一致。未设置时，首次启动在数据目录创建 `master.key`。更换主密钥前需要迁移已有凭据；直接覆盖会使凭据不可解密。

## Linux 容器运行

先构建镜像，然后把持久化目录和仅管理员可读的环境文件挂入容器：

```bash
docker build -t dbsphere:0.1.0 .
install -d -m 0700 /srv/dbsphere/data
# /srv/dbsphere/dbsphere.env 内设置 DBSPHERE_ADMIN_PASSWORD、DBSPHERE_COOKIE_SECURE 等变量
chmod 0600 /srv/dbsphere/dbsphere.env
docker run -d --name dbsphere --restart unless-stopped \
  --env-file /srv/dbsphere/dbsphere.env \
  -v /srv/dbsphere/data:/var/lib/dbsphere \
  -p 127.0.0.1:8080:8080 dbsphere:0.1.0
```

镜像使用非 root 用户。挂载的数据目录需要让容器内 UID 1000 可写。对外的 HTTPS 代理应转发到宿主机 `127.0.0.1:8080`，并把 `DBSPHERE_COOKIE_SECURE=true` 放入环境文件。当前 `/healthz` 是进程存活检查，不代表 PostgreSQL 或 Kubernetes 目标健康。

## PostgreSQL 只读纳管

在每个目标实例上由数据库管理员创建专用账号，密码由管理员自行生成：

```sql
CREATE ROLE dbsphere_monitor LOGIN PASSWORD '替换成强密码';
GRANT pg_monitor TO dbsphere_monitor;
```

在浏览器中新增资产，填写主机、端口、数据库、账号、密码、TLS 模式和 Linux/K8S 运行位置。若数据库开启 TLS，选择 `require`；当前实现校验服务端证书链，需确保运行环境信任签发 CA。采集使用只读事务设置和 3 秒语句超时。部分受权限或版本限制的指标会显示“未采集”及原因。

## Kubernetes 中的 PostgreSQL

目前需要手工填写 Namespace 和 Pod 名，重建 Pod 后应更新映射。DbSphere 在集群内运行时，给其 ServiceAccount 在目标 Namespace 授予 `pods`、`persistentvolumeclaims` 的 `get` 和 `events` 的 `list`。集群外运行时，将只读 kubeconfig 保存为仅 DbSphere 服务账号可读的文件，并设置 `DBSPHERE_KUBECONFIG`；资产中的“K8S 集群”填写 kubeconfig 的 context 名。DbSphere 禁止执行 kubeconfig 中的外部认证插件。

[`test/fixtures/k8s-postgres.yaml`](../test/fixtures/k8s-postgres.yaml) 包含一个**仅供临时测试**的 PG18 Pod、PVC 与最小权限 Role/RoleBinding。它使用公开的测试密码，不能用于生产。生产环境应自行创建只读 ServiceAccount，并按实际 Namespace 绑定权限。

## 数据与限制

资产、样本和事件存于 SQLite，数据库密码使用 AES-256-GCM 加密。V0.1 不含备份编排、自动发现、通用告警、完整全链路分析或自动处置。当前 K8S 根因候选仅覆盖“PG 连接失败 + 同一 Pod 近期 OOMKilled”的窄场景，结论需人工复核。详见[开发状态](DEVELOPMENT_STATUS.md)和[版本路线图](ROADMAP.md)。
