# DbSphere V0.7.0

**全域数据库智能运维管理平台 DbSphere** 的应用到 PostgreSQL 链路版本，采用 Apache License 2.0。

## 功能

- 可选 OTLP/HTTP JSON Trace 接入，支持官方 OpenTelemetry Collector 的 JSON 转发。独立令牌认证、资产唯一映射、敏感字段白名单、PG18 去重与默认 7 天保留期；未接入 Trace 时，数据库、Linux/K8S 监控与无模型规则诊断照常运行。
- 在一个问题的**同一事件 ID**下，沿服务父子 Span、PostgreSQL 客户端 Span、锁等待会话、SQL 指纹及 PG/Linux/K8S 证据查看问题与恢复。已观测跨服务父子 Span 形成服务依赖；页面区分同期请求和有明确故障信号的 Trace。
- 可选后端 PID 精确匹配锁等待；可选 Query ID 在同期样本中唯一对应指纹时连接 DB Span 与 SQL。真实 PG18 慢查询、退化告警、受控应用 Span 与恢复已验证同事件闭环。关联是可审查线索，不自动证明唯一根因。
- 延续 PG14–18、Go 后端与静态 B/S 页面、独立 PostgreSQL 18 长期存储、Linux x86_64 一键离线容器包和静态 Go 服务端。

## 下载

- `DbSphere-go-offline-0.7.0-linux-amd64.run` 与同名 `.sha256`：一键离线容器安装。
- `DbSphere-go-0.7.0-linux-amd64` 与同名 `.sha256`：静态 Go 服务端。
- `DbSphere-go-licenses-0.7.0.tar.gz` 与同名 `.sha256`：许可证文本。

## 验收与限制

发布提交须通过 PG14–18、Go/PG18、Kubernetes、离线包、安全与升级 CI，六个附件来自同一验收构建且校验值一致。真实 PG14–18 应用锁链、kind 中 PG Pod OOMKilled 和 PG18 SQL 退化均已在一个事件 ID 下核对链路与恢复，详见[发布验收](V0.7_RELEASE_GATE.md)。

普通 SDK 不会自动提供 DbSphere 自定义的后端 PID 或 Query ID；其他语言 SDK、完整跨服务传播、连接池/代理与动态 K8S 身份映射、采样率和时钟偏移仍待验收。生产 Kubernetes、物理断网主机与签名发布保留在 V1.0 门槛。时间相关性及 SQL 身份匹配不能单独证明根因。
