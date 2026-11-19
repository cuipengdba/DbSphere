# DbSphere V0.6.0 RC2（Go 预览版）

**全域数据库智能运维管理平台 DbSphere** 的第二个 V0.6 Go 候选包。RC2 是预发布，不代表 V0.6.0 正式验收完成；V0.5.0 仍是最新正式版。采用 Apache License 2.0。

## 本次改进

- 修复 RC1 离线包在 Docker Engine 27 导入镜像后因本地镜像 ID 规范化而误拒绝安装的问题。仍先校验外层 `.run.sha256` 和内部清单，再要求 `docker image load` 明确加载 Go 与 PostgreSQL 18 两个预期标签；跨引擎 ID 改变时提示并继续。
- 新增无外网嵌套 Docker 守护进程验收：安装器与守护进程同时处于 `--network none`，验证 Go B/S `/readyz`、内嵌版本、PG18 存储和重复安装后数据保留。
- 真实 PG18 锁等待超过默认 60 秒后生成同事件锁链候选；解除锁后，新样本关闭原事件，复盘报告保留故障与恢复证据。
- 真实连接压力演练要求根因候选保持“假设”并说明缺少连接拒绝证据。

## 交付与边界

- `DbSphere-go-offline-0.6.0-rc.2-linux-amd64.run` 与同名 `.sha256`：Linux x86_64 的 Go 与独立 PostgreSQL 18 一键离线容器安装包；目标机预装 Docker Engine 与 Compose v2。
- `DbSphere-go-0.6.0-rc.2-linux-amd64` 与同名 `.sha256`：静态 Go 服务端，需外部 PostgreSQL 18。
- `DbSphere-go-licenses-0.6.0-rc.2.tar.gz` 与同名 `.sha256`：许可证文本。

PG14–18、Linux/K8S 纳管、同事件证据图、确定性告警与可选 AI 的使用范围沿用 RC1，详见[完整功能说明](../README.md)和[支持矩阵](SUPPORT_MATRIX.md)。八类故障的完整真实矩阵、物理断网主机安装与生产 K8S 等正式门槛仍在[V0.6 发布清单](V0.6_RELEASE_GATE.md)中跟踪。无模型接入时监控、告警与规则诊断仍可运行。
