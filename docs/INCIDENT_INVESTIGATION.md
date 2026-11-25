# 事件调查工作台

从「实时观测」异常入口或「问题事件」进入。首屏显示同一事件的症状、可见关联实例、主要候选、关键原始采样和待核对项。跨实例事件保留子事件的原始 ID、开始时间和各自的结束时间。在工作台左下角可核对正在运行的 DbSphere 版本。

在「同一时间窗」选择故障前、触发时、峰值或恢复后。应用、入口与连接池、数据库、Linux 与 Kubernetes、容器与存储使用同一时间游标。单击标记定位已观测到的证据，查看原始采样或 Trace。沿等待趋势、锁树、存储或 Grafana 专题返回时保留事件 ID、阶段和已选证据。复制当前事件地址可恢复同一选区。

对于有原始锁等待告警样本的事件，「查看阻塞者原始采样」可以核对等待 PID、阻塞 PID 和已采集的 Query ID。PgBouncer 等待和主库回放差距可打开出现信号的样本。PVC 峰值核对同名同 UID 的卷，容器 I/O PSI 核对同一容器 ID。峰值最多检查触发后的 120 条原始采样，达到上限时显示扫描截止时间。时间相关、标识冲突、缺采和人工复核状态在候选旁可见。

「下载当前证据复盘」导出事件 ID、当前链接、时间窗、关键证据、恢复样本和人工复核摘要。原始链接沿用 DbSphere 会话与资产访问范围。未配置 AI 时仍可使用采样与规则完成调查。

## 跨数据库调用与基础设施证据

事件页按 Trace ID 和可核对的父 Span 展示应用调用。可按应用服务、数据库引擎和纳管实例筛选。点击「查看 Trace」核对父子 Span 与耗时，点击「原始采样」检查同一实例附近的 Redis 命中和淘汰、SQL 指纹、Linux 磁盘、容器和 PVC。Redis 暂按纳管的逻辑库编号匹配，默认编号为 0。

应用上报 `db.system.name`、`server.address`、`server.port` 和可用的 `db.namespace`。当前接受 `postgresql`、`mysql`、`redis`、`etcd`、`dm8`、`kingbase`。使用前须核对地址、端口和数据库与平台资产唯一对应。Trace 只保留已列入允许范围的身份和时间字段。读取同一 Trace 的其他实例仍需要该资产的访问权限。[OpenTelemetry 数据库客户端规范](https://opentelemetry.io/docs/specs/semconv/db/database-spans/)

如已为 etcd 配置 Kubernetes Context 和 Linux node_exporter，etcd 事件会显示 API Server 侧 etcd 调用耗时、当前成员的 WAL fsync 和后端提交耗时，以及对应的原始采样。根据配置核对成员和主机身份，结合时序和原始记录复核原因。
