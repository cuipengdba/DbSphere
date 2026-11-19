# DbSphere V0.6.0

**全域数据库智能运维管理平台 DbSphere** 的 V0.6.0 正式版，采用 Apache License 2.0。

## 功能

- Go 后端与内嵌静态前端组成 B/S 工作台；支持 PostgreSQL 14–18 的 Linux 与 Kubernetes 资产纳管、监控、SQL 性能、告警、巡检、备份状态及可选 Patroni、PgBouncer 和应用 Trace 信号。
- 以一个问题对应一个事件 ID：展示原始样本、告警、跨层证据图、时间顺序、根因候选、缺失信号、人工确认或驳回及恢复报告。证据不足时保持未定位或低置信度假设；不接 AI 模型也能运行。
- PostgreSQL 18 独立保存长期监控数据。可选 AI 仅解读脱敏证据，不执行生产变更。
- Linux x86_64 一键离线容器包内含 Go 应用与 PostgreSQL 18 镜像；目标机预装 Docker Engine 与 Compose v2，无需 Node.js、Go 编译器或在线拉取。另提供连接外部 PG18 的静态 Go 服务端。

## 下载

- `DbSphere-go-offline-0.6.0-linux-amd64.run` 与同名 `.sha256`：一键离线容器安装。
- `DbSphere-go-0.6.0-linux-amd64` 与同名 `.sha256`：静态 Go 服务端。
- `DbSphere-go-licenses-0.6.0.tar.gz` 与同名 `.sha256`：许可证文本。

## 验收与限制

发布提交须通过 PG14–18、Go/PG18、Kubernetes、离线包、安全与升级 CI，并从同一构建上传六个校验过的文件。真实故障演练已覆盖连接压力、锁链、SQL 退化与计划/慢日志、复制/WAL、autovacuum、Pod OOMKilled 和 CSI PVC 容量压力；每个已验证场景都按同一事件 ID 保留故障与恢复证据。Linux 文件系统容量压力与 PGDATA/WAL 挂载映射仍缺独立实测。CSI PVC 演练在本机 kind 使用真实 kubelet 指标与测试适配器，告警持续时间临时设为 0 秒；生产 CSI/Prometheus、默认五分钟持续时间及独立稳定集群仍待复核。

离线包已在 Linux CI 的无外网 Docker 27 守护进程中完成安装、就绪和 PG18 数据保留；物理断网 Linux 主机尚未实测。生产 Kubernetes、长期容量与签名发布属于 V1.0 验收范围。详见[支持矩阵](SUPPORT_MATRIX.md)、[部署文档](OFFLINE_DEPLOYMENT.md)和[V0.6 验收](V0.6_RELEASE_GATE.md)。
