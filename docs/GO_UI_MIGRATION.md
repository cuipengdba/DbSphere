# Go 后端与分层工作台迁移计划

> 2026-11-17 新增需求。此文是开发和验收清单，不把迁移中的能力称为已发布。

## 强制架构

- V1.0 后端使用 Go。入口为 Linux `dbsphere` 可执行文件，优先 `CGO_ENABLED=0` 编译；静态前端资源编入同一程序，通过 HTTP API 与服务端交互。浏览器不需要 Node.js。
- DbSphere 的长期监控存储仍为独立 PostgreSQL 18；被监控实例兼容 PG14–18。Linux 主机、Kubernetes PostgreSQL、PgBouncer、备份和 OTel Trace 由只读或明确授权的适配器采集。
- Docker Compose 一键部署和 Linux x86_64 一键离线包以 Go 程序 + PG18 镜像为交付物。Go 单文件原生运行模式允许连接已有独立 PG18。离线目标机不需要 Node.js、Go 编译器或包管理器；容器模式仍需预装 Docker Engine/Compose。
- 已发布的 V0.1–V0.5 和 V0.6 开发预览使用 Node.js 后端，是迁移前历史产物。新架构达到对应功能对等和升级/回滚验证前，不发布 Go 版正式版本，更不将现有 Node 预览称为 Go 交付。

## Go 工程与功能对等清单

- [x] `go.mod`、`cmd/dbsphere`、`internal` 分层、嵌入静态资源、配置校验、健康/就绪接口、结构化日志、优雅退出；Linux amd64 `CGO_ENABLED=0` 静态构建通过。
- [ ] PG18 schema 迁移与版本管理；既有资产、样本、事件、证据、规则、巡检、通知、慢日志、计划和 Trace 可在 Go 版读取，升级不丢数据。
- [ ] 管理员登录、密码哈希迁移、Session/CSRF、凭据加密、审计、TLS 入口与权限边界在 Go API 中验收。
- [ ] PG14–18 只读采集、Linux 与 K8S 适配器、连接池/备份信号、告警/巡检、根因证据图、OTLP Trace、可选 AI 按现有 API 契约逐项迁移并做双实现对照。
- [ ] `go test`、`go vet`、五版 PG 实库、K8S kind、PG18 持久化、浏览器与离线升级/回滚共同作为 CI 发布门禁；发布包校验和与 SBOM。

当前 Go 进度：已从现有 Node 实现提取 PG18 基础 schema，Go 启动时在事务与 advisory lock 下迁移并创建当月/下月分区；空 PG18 实库验证了版本 13 和分区。管理员密码的 scrypt 哈希、AES-GCM 凭据、HMAC Session/CSRF 已用 Node 格式向量验证兼容，Go 登录、限流、同源检查与登出已实现。总览所需的十个只读列表/存储接口已经从真实 PG18 读取，包含 Node 历史库的已有数据。资产增删改及 PostgreSQL 只读采集已进入 Go，凭据不返回浏览器；PG14 本机实库验证了连接失败两次和恢复共用同一事件 ID。事件详情读取同一 ID 下的原始证据、采样与同窗 Trace，提供原始样本入口和人工确认；资产详情可读样本、时间序列、慢日志和计划。Go 支持 Linux PG 与可选 node_exporter 联合采集，初步接入 K8S Pod/Node/PVC/Pod 事件和 PgBouncer SHOW POOLS/STATS 的只读采集；十一条 PG/Linux/K8S/PgBouncer 默认规则具备阈值、持续时间、启停、静默识别和事件恢复，浏览器可修改这些规则。其余 Node 历史规则只读显示。K8S 的 Service、metrics.k8s.io、PVC 用量与其余规则仍未迁移；缺少 Pod 映射或凭据会标记采集错误，避免只采数据库就误报全局健康。**完整跨层采集、告警投递和巡检写入、完整根因候选、AI、Go 正式版离线交付仍未完成**，所以 Go 服务只用于开发验收。

## 页面信息架构

Go Linux node_exporter 联合采集现可在 PG 连接失败时保留主机指标，并把 Linux 节点连入同一事件 ID 的图。本机真实 PG14/node_exporter/PG18 演练和 [21 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44096143595) 已通过。K8S 深度信号与 PgBouncer 仍在适配器迁移队列。

Go K8S 第一段适配器只接受 ServiceAccount 或静态 bearer token kubeconfig，拒绝执行式凭据插件与关闭 TLS 校验。已用 TLS 测试 API 验证只读请求、按 Pod UID 限定事件、容器 OOM、Node/PVC 信息及错误状态；PG18 实库测试验证 PG 连接失败与近期同 Pod OOM 证据归入一个事件 ID，并在恢复时关闭。现有本地 kind 测试 token 过期，真实集群只读验收尚未通过；临时 token 签发被自动审批策略阻止，不能将模拟 API 测试记作 kind 验收。Go Compose 与离线包已支持可选的只读 kubeconfig 挂载。

Go K8S 告警第一段移植了 Pod 未就绪、Pod 未调度、近期调度/挂载事件、近期 OOMKilled 和 Node 压力五条规则。PG18 实库验证了 PG 连接故障恢复后，OOM 告警仍触发时同一事件 ID 保持开放，两个信号都恢复才关闭。Pod 重启增量、PVC 实际用量告警与通知投递仍未移植。

Go PgBouncer 适配器采用只读的简单查询协议读取 `SHOW POOLS` 和 `SHOW STATS`，仅保留映射数据库最多 100 条连接池/统计记录，不保存管理员 URL 或密码。本机真实 PgBouncer 1.26.1、PG14 与独立 PG18 联合采样测试和 [PgBouncer CI 作业](https://github.com/cuipengdba/DbSphere/actions/runs/44106034937)已通过；对应客户端等待规则进入同事件告警流程。

Go Top SQL 采集已对齐历史数据格式，扩展缺失、列不兼容或查询失败分别显示状态；只存指纹与统计量。资产页从 PG18 样本计算相邻窗口增量和退化倍数，本地 PG18 真扩展与样本退化测试已通过；五版扩展 CI 正在验收。慢日志和计划导入写入仍需迁移。

Go 只读 PostgreSQL 诊断现覆盖 14 个分区，PG14 与 PG18 实库及真实 advisory lock 阻塞关系通过本机测试；PG15–17 交由五版 CI 确认。采集指标写入 PG18 原始样本，事件图可显示等待 PID 与阻塞 PID；独立诊断失败显示不可用原因，仍需规则告警和完整故障演练。

Go 自动采集调度已实现：纯 Linux PostgreSQL 资产按配置间隔采集并在 PG18 写入样本/事件；默认 Compose 启用，连接外部 PG18 的 Go 开发配置默认关闭，需显式启用，避免与历史 Node 采集并行。K8S 和 PgBouncer 资产在对应适配器迁移前不进入 Go 调度。本机真实 PG18 演练验证自动连接失败生成同一事件入口，仍须五版与恢复矩阵 CI。

Go 部署开发路径已落地：`Dockerfile-go` 用静态编译程序与嵌入页面构建最小容器；`scripts/one-click-go.sh` 启动独立 PG18 或外部 PG18 配置；`scripts/build-go-offline-bundle.sh` 将应用和 PG18 镜像打入 Linux x86_64 自解压包，目标机以 `--no-build --pull never` 启动。[21 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44088758026)已验证离线安装和重装。代码与历史 Node 正式部署隔离；Go 升级回滚和功能对等仍须完成。

根因分析迁移补充：Go 已能从同一事件 ID 关联原始样本、应用服务与父子 Span、锁链、SQL 指纹、Linux 和 K8S 拓扑。已有候选包括历史 Pod OOMKilled 关联证据、连接数压力和锁阻塞，显示证据、时间顺序与不确定性；管理员确认/驳回写入该事件的证据时间线。PG18 实库样例已验证这些路径；**Go 自动跨层采集和完整规则矩阵仍未完成，证据不足显示“未定位”**。

页面以值班问题处理顺序组织：**总览 → 问题事件 → 资产与实例 → 告警与通知 → 巡检与报告 → 系统状态**。总览只显示待处理事件和资产健康；资产纳管及实例指标在资产页；事件页用左侧列表与右侧证据链、根因候选和时间线；规则阈值、静默与通知在告警页；模板/任务/报告在巡检页。一个问题的所有层继续使用同一个事件 ID。

此取舍参考本地 `E:\DbSphere\DeepLens\使用说明-V26ai1.0.md` 中的左侧菜单和资源/执行分区、[DBdoctor 官方介绍](https://www.dbdoctor.cn/)中的问题到 SQL 根因闭环、[Pigsty 官方仪表盘目录](https://pigsty.io/docs/pgsql/dashboard/)中的总览→集群→实例→数据库/查询分层。只借鉴信息组织与任务路径，不复用其代码、视觉资产或仪表盘。

## 页面验收

- [x] 桌面与窄屏均可直接进入目标任务；总览首屏可见待处理事件，规则表不占据首屏。本机真实浏览器在 1440px 和 390px 宽度检查，窄屏无横向溢出。
- [ ] 从总览事件进入详情不超过一次点击，事件 ID、证据、缺失信号和人工结论清晰；从详情能返回原始脱敏数据。
- [ ] 纳管、编辑、采集、规则、静默、巡检等原有操作在新导航下无回归；刷新和直接打开 `#incidents` 等地址仍显示正确页面。
- [ ] 真实浏览器在 PG18 存储和 K8S/PG 故障样例下验收可读性；无模型配置时完整工作。
