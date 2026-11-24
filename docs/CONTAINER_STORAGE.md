# 主机、容器与存储监控

从 **全域总览 → 主机与容器** 或监控工作台的「基础设施同窗」进入。资源专题对齐 `node_exporter` 的 CPU、内存、磁盘和网络、Kubernetes 的 Pod 用量以及 PostgreSQL 的 I/O 等待。容器专题展示 Docker 或 Kubernetes 容器的 IOPS、页缓存及压力。卷与设备专题显示 PVC 容量、已用、设备时延及对应采样。各专题共享资产、时间窗和游标，双击图表可缩小时间窗。

基础设施身份栏显示已观测到的工作负载、Pod UID、Node UID 和容器 ID，并可打开原始采样。资源用量取决于 `node_exporter`、Kubernetes 资源指标 API 或容器采集端的实际可见性。采集源缺失时，相关数值显示缺采。增强包从同一资产和时间窗打开 Grafana 面板。

## 连接采集源

在 DbSphere 中登记 PostgreSQL，记下资产 ID。Linux 上的 Docker 容器使用 `linux` 运行环境，Kubernetes 使用已有集群、命名空间、Pod 或工作负载关联。采集端需要对 Linux cgroup 文件具备读取权限。应用通过环境变量 `DBSPHERE_CONTAINER_STORAGE_BINDINGS` 加载绑定清单，`DBSPHERE_CONTAINER_AGENT_TOKEN` 保存两端共享的采集密钥。使用一键离线包部署时，将变量写入安装目录中的 `dbsphere-go.env` 后重跑同一个安装包。使用 systemd 时将变量写入服务环境并重启。

下面的绑定示范包含一个 Docker 容器和一个 Kubernetes 工作节点。将 URL、资产 ID、容器 ID、集群和节点名替换为现场值。`nodes` 的 Kubernetes 键使用 `集群/节点`，`primary` 用于多容器 Pod 中 PostgreSQL 容器名称无法用端口区分的情况。模型接入和增强观测组件均为可选。

```json
{
  "nodes": {
    "linux-db-01": {"url": "http://container-agent:9918", "nodeExporter": "linux-db-01:9100"},
    "production/db-worker-01": {"url": "https://db-worker-01.monitoring.example"}
  },
  "docker": {
    "123e4567-e89b-12d3-a456-426614174000": {
      "host": "linux-db-01",
      "containerId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  },
  "primary": {"123e4567-e89b-12d3-a456-426614174001": "postgres"}
}
```

### Docker

标准和增强离线包都包含采集端，复用 DbSphere 镜像。用 `docker inspect --format '{{.Id}}' 容器名` 获取完整容器 ID。`dbsphere-monitoring` 是 DbSphere 能访问、且限制采集端访问面的私有网络。将单独生成的随机密钥设置到同名环境变量，两端取相同值。远程连接可用内部 TLS 代理。

```sh
docker run -d --name container-agent --network dbsphere-monitoring --pull never \
  --no-healthcheck --read-only --cap-drop ALL --security-opt no-new-privileges \
  --mount type=bind,src=/sys/fs/cgroup,dst=/host-cgroups,readonly \
  -e DBSPHERE_CGROUP_ROOT=/host-cgroups -e DBSPHERE_CONTAINER_AGENT_ADDRESS=0.0.0.0:9918 \
  -e DBSPHERE_CONTAINER_AGENT_TOKEN dbsphere/offline-go-app:2.0.0 container-agent
```

### Kubernetes

将离线包内的 DbSphere 镜像导入目标节点的容器运行时。安装后，采集端清单位于配置目录下的 `kubernetes/dbsphere-container-agent.yaml`。创建 `dbsphere-monitoring` 命名空间和同名采集凭据，Secret `dbsphere-container-agent` 使用键 `token`。应用清单后，在绑定中为受监控的节点登记采集地址。节点 9918 端口应仅允许内部采集网段访问。API 对象关联沿用现有的只读 Kubernetes 凭据，DaemonSet 仅挂载只读 cgroup 目录。

### 已有 Prometheus

在某个 `nodes` 来源上指定非空 `job`，并将 `url` 指向受信的 Prometheus 地址。DbSphere 从该 job 按完整运行时标识匹配 cAdvisor 的 `id` 标签，使用 `container_last_seen` 校验数据时间和 `container_start_time_seconds` 识别重启。配置 `DBSPHERE_PROMETHEUS_TOKEN` 可连接需要认证的服务。通过记录规则将上游独立采样的 Active、Inactive 字节数映射为 `dbsphere_source_container_active_file_bytes` 和 `dbsphere_source_container_inactive_file_bytes`，保留原 `job`、`id` 标签。可选的重读累计次数对应 `dbsphere_source_container_refault_file_total`。重复标识或过时的数据点会显示状态。

## 读图与诊断

在设备热点选择块设备，可沿采集源匹配的 `major:minor` 核对节点设备名称、队列深度和采样时间。PVC 侧分别列出选定 Pod 中声明该卷的容器，以及经只读 API 核对的 PVC 和 PV 标识。同 Pod 有多个挂载者时标明共享范围，卷级 I/O 归属仍需可验证的卷到设备证据。

点击 PVC 查看同一 UID 的容量、占用和估算。估算基于至少四个按小时保留的样本，覆盖至少 24 小时，且 PVC UID、PV UID、采集序列和容量一致。当前样本与基线记录保存在 PG18。Grafana 保存资产、设备号、PVC UID 和时间窗，返回时按这些条件恢复专题。

- 读写吞吐和 IOPS 是相邻有效样本的累计量差值除以间隔。重启、迁移、计数器重置及采集中断会形成断点。历史点显示桶内峰值，响应同时保留均值、最小值和有效覆盖信息。需要更快刷新时可将 `DBSPHERE_COLLECT_INTERVAL_SECONDS` 设置为 `10`，也可沿用已有采集频率。
- Active 与 Inactive file 分别表示 Linux 文件页所在的回收列表。`file` 单独采集。结合 refault、回收速率、内存和 I/O 压力，可以区分正常波动和伴随磁盘重读的回收压力。cgroup v1 展示可用的 `memory.stat` 和 `blkio` 指标。
- Kubernetes PVC 显示对象归属和可用的容量数据。节点延迟根据已采到的设备时间和完成操作数计算平均值。节点或容器设备指标保持各自归属，PVC I/O 缺少可验证来源时显示卷映射信息。
- PostgreSQL 14–18 结合 I/O 等待、已有 SQL 统计、临时文件、WAL 和检查点。PG16 起可附带 `pg_stat_io`。诊断规则使用已关联的容器与数据库信号，覆盖缓存回收伴随重读、持续写回、接近已配置的 I/O 限流及节点设备忙碌。事件记录采样证据，可在同一事件中复核后续恢复。

技术口径见 [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)、[Kubernetes 节点指标](https://kubernetes.io/docs/reference/instrumentation/node-metrics/) 和 [PostgreSQL 16 统计视图](https://www.postgresql.org/docs/16/monitoring-stats.html)。
