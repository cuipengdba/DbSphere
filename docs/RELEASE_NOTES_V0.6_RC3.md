# DbSphere V0.6.0 RC3（Go 预览版）

**全域数据库智能运维管理平台 DbSphere** 的第三个 V0.6 Go 候选包，采用 Apache License 2.0。RC3 仍是预发布；V0.5.0 是最新正式版。

## 本次改进

- PostgreSQL 真实 SQL 采样连续两个有效区间中，同一 Query ID 的平均耗时达到相对与绝对退化门槛时触发 `pg_sql_regression` 告警。故障区间、前一区间和基线样本进入同一事件证据图；恢复后由新快速区间关闭原事件。PG18 实库演练已通过，缺少扩展、统计重置或调用量不足时不推断退化。
- Kubernetes PVC 使用率采用 kubelet 同一时间、同一卷上报的已用字节与实际容量，避免将 PVC 申请容量误当文件系统容量。缺少、过期或不匹配的指标会显示不可用，不生成虚假压力告警。
- 延续 RC2 的 Go 静态服务端、独立 PostgreSQL 18 长期存储、一键离线容器包、PG14–18 监控、Linux/K8S 纳管、同事件根因候选与可选 AI。

## 交付与边界

- `DbSphere-go-offline-0.6.0-rc.3-linux-amd64.run` 与同名 `.sha256`：Linux x86_64 Go 与 PostgreSQL 18 一键离线容器安装包，目标机需预装 Docker Engine 与 Compose v2。
- `DbSphere-go-0.6.0-rc.3-linux-amd64` 与同名 `.sha256`：静态 Go 服务端，需外部 PostgreSQL 18。
- `DbSphere-go-licenses-0.6.0-rc.3.tar.gz` 与同名 `.sha256`：许可证文本。

V0.6 正式版的八类故障矩阵、真实 PVC 压力与物理断网主机验收仍在[发布门槛](V0.6_RELEASE_GATE.md)跟踪。无 AI 模型时监控、告警与规则诊断仍可运行。
