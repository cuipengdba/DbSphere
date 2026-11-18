# 本机 Kubernetes 验收记录（开发版）

> 2026-11-18，单节点 kind `dbsphere-test`，Namespace `dbsphere-smoke`，PostgreSQL 18 测试数据库。此记录只证明列出的本机故障路径，不是生产 Kubernetes 或 V1.0 正式版验收。PG14–18 多节点矩阵另见[独立验收记录](KIND_MATRIX_ACCEPTANCE.md)。

## 2026-11-19 五段问题链路浏览器复验

将提交 `f7f58fe` 的 Go Linux 镜像 `dbsphere/kind-acceptance:trace-gaps` 加载到本机单节点 kind，并滚动更新 `dbsphere-go` Deployment。新 Pod `dbsphere-go-8b4d8559f-kjzrp` 为 `1/1 Ready`、零重启，`/readyz` 返回 `ready/postgresql18`。本次新 Pod 曾因既有 kind `kube-scheduler` 的 API leader lease 超时而 Pending；调度器恢复后自动完成调度和滚动更新，未用手工 Binding 完成验收。

Playwright Chromium 登录实际 Go B/S 页面，读取真实 OOM 事件 `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7`：页面按应用服务、数据库调用、PostgreSQL、会话与 SQL、Linux/K8S 显示五段。服务父 Span 与 PostgreSQL 客户端子 Span 分段出现，事件仍显示 24 个证据节点、47 条关系、8 条时间线记录和带证据的 OOM 根因候选；报告下载按钮可见。同一事件 API 的 `traceCoverage.unresolvedParentSpans=0`，这只说明当前事件窗口已读到的 Span 没有缺父链，不代表采样完整或覆盖全部请求。再读取没有应用 Trace 的 PG14 Patroni 事件 `c0c3e0dc-a8df-4d37-a86d-5464013d416b`，前两段明确显示“未接入 Trace”，数据库与 K8S 诊断仍可用。两条事件检查均无浏览器脚本或控制台错误；桌面和 390px 窄屏截图存于本机 `H:\CodexLS\outputs\DbSphere-kind-trace-flow.png` 与 `H:\CodexLS\outputs\DbSphere-kind-trace-flow-mobile.png`。窄屏无横向溢出；未接入 AI 模型时，事件的五段链路、规则候选和 Markdown 报告仍可读取，报告注明采样率和时钟偏移尚未验证。该提交的 [31 项 GitHub CI](https://github.com/cuipengdba/DbSphere/actions/runs/44382369126)已通过，包含 PG14–18、离线包和安全扫描作业。

本轮只验证单节点 kind 的实际页面与已有故障事件。控制面在滚动更新时再次波动，因此不能据此认定生产 Kubernetes 稳定性、持续负载、跨服务 Trace 传播或 V1.0 整体验收通过。

## 2026-11-19 事件工作台窄屏筛选复验

将 Go 镜像 `dbsphere/kind-acceptance:event-filter` 部署到同一 kind，`dbsphere-go` 新 Pod `dbsphere-go-78494c79bc-wjmnk` 为 `1/1 Ready`、零重启，`/readyz` 返回 `ready/postgresql18`。Playwright Chromium 在 390px 视口输入真实 OOM 事件完整 ID，最近 17 条事件筛选为 1 条；切到“处理中”后为 0 条，切回“已恢复”后为 1 条。点击事件后页面滚到同一事件的证据详情，五段链路仍可见，页面无横向溢出或脚本错误。截图保存在本机 `H:\CodexLS\outputs\DbSphere-kind-event-filter-mobile.png`。筛选只作用于 API 返回的最近 200 条事件，不等于全历史搜索；大规模历史分页仍待 V1.0 验收。

随后将 `dbsphere/kind-acceptance:event-filter-id` 滚动到同一 Deployment，新 Pod `dbsphere-go-856576fccf-hdgmm` 为 `1/1 Ready`、零重启。浏览器把状态设成“处理中”，此时已恢复 OOM 事件不在当前列表；输入其大写完整 UUID 并按回车，仍通过受权限约束的事件详情 API 打开原事件、自动滚到证据区，五段链路可见且无脚本错误。该测试验证“列表筛选无匹配时仍可按完整 ID 直达”；未验证超过最近 200 条的实库历史数据分页。

## 2026-11-19 Go 分区维护滚动复验

将提交 `d73d6c8` 的 Go 镜像 `dbsphere/kind-acceptance:partition-maintenance` 滚动部署到同一 Namespace；新 Pod `dbsphere-go-b74f5b78-6x6tc` 为 `1/1 Ready`、零重启，独立 PG18 存储 Deployment 仍为 `1/1 Ready`。`/readyz` 返回 `ready/postgresql18`。存储中采样、事件两张父表合计有 6 个当前与未来月分区，原有 17 个事件保留；Playwright Chromium 再以完整 ID 打开真实 OOM 事件，五段链路仍可见且无脚本错误。该测试只证明升级后现有数据可读、启动分区已建和页面未回归；运行超过一个月的后台维护由真实 PG18 时间推进测试及代码覆盖，未在 kind 等待一小时触发。此处未替换现有 kind PG18 存储镜像或数据卷，因此也不代表 PostgreSQL 基础镜像升级验收。

## 2026-11-19 Go 新镜像滚动验收

本机单节点 `dbsphere-test` 集群的 `dbsphere-smoke` Namespace 将 Go 镜像更新为 `dbsphere/kind-acceptance:collector-workers`，镜像包含托管资产模式、采集缺口面板及有上限的自动采集代码。`dbsphere-go` Deployment 最终为 1/1 Ready，`/readyz` 返回 `ready/postgresql18`；浏览器从替换后的 Go Pod 读取既有 PG14 Patroni 可用性事件 `c0c3e0dc-a8df-4d37-a86d-5464013d416b`、关联遥测事件和可展开的证据时间线，控制台无错误。PG14 K8S 资产详情显示数据库诊断 14/14、K8S Pod/Node/PVC 与 Patroni 已采集，Top SQL 缺少扩展时明确标为未采集。截图保存在本机 `H:\CodexLS\outputs\DbSphere-kind-coverage.png`。

滚动初段新 Pod 曾处于 Pending，原因是既有 kind 控制面的 `kube-scheduler` 持续重启，日志显示访问 API Server 更新 leader lease 超时。人工尝试更新 Pod 的 `nodeName` 被 API 拒绝；随后尝试创建 Binding 时 Pod 已由恢复的调度器自动分配，Binding 返回冲突。最终滚动由正常调度完成，未把手工绑定计作成功路径。此集群的控制面波动不能作为生产 K8S 稳定性验收。

## 本轮本机集群接入与最终代码复验

用户要求直接在本机搭建并验收 Kubernetes 环境。本机已有专用 `dbsphere-test` kind 集群，本轮复用其隔离的 `dbsphere-smoke` Namespace，没有修改 AgentSQL 集群。重新从 kind 控制平面导出仅供本地故障注入的管理员 kubeconfig，保存在 `H:\CodexLS\tmp\dbsphere-local-admin.kubeconfig`，未签发新的 ServiceAccount token，也未提交凭据。控制平面 Node 为 `Ready`；DbSphere Pod 继续使用已存在的 `dbsphere-reader` 投射凭据。`kubectl auth can-i` 核对该账号能读取目标 Pod，不能读取 Secret 或删除 Pod。

包含本轮 Patroni 采集和 PostgreSQL SQLSTATE 归类修复的 Go Linux 静态镜像 `dbsphere/kind-acceptance:patroni-final` 已加载到本机 kind，并滚动更新 `dbsphere-go` Deployment。新 Pod `dbsphere-go-7bdc759dd-x455k` 为 `Ready 1/1`，`/readyz` 返回 `ready/postgresql18`。B/S API 对真实 PG18/K8S 资产 `44a49ba3-b90f-4b20-afe0-42e6181cb2d7` 取得新样本 `b96058f4-558b-46b6-8a32-8ccc534a0ce9`，状态 `ok`、PG 版本 `180006`，关联实际 Pod `dbsphere-go-schedule-pg-597b745bd9-2r74s`、Node `dbsphere-test-control-plane` 与 Service `dbsphere-go-schedule-pg`。新增 Patroni 告警规则在 API 中可编辑；未配置 Patroni 的既有 K8S 资产继续正常采样。此处是**单节点**开发环境回归，不能代替生产可靠性或多节点验收。

## 最新本机复验：采样时效和探针故障闭环

2026-11-18，复用本机 `dbsphere-test` 集群；首次检查发现 API Server 的 etcd 健康检查失败。重启该测试控制平面后，Node 恢复 `Ready`，PG18 存储完成崩溃恢复并重新就绪。Go 后端新静态镜像 `dbsphere/kind-acceptance:freshness` 在同一 Namespace 滚动部署，`/readyz` 返回 `ready/postgresql18`。关闭自动采集时，浏览器将 4 个历史成功样本显示为“最近一次成功”，不把旧样本称作当前健康。

临时启用自动采集期间，本机 kind API Server 再次超时；4 个 K8S 资产出现“Kubernetes resource collection is unavailable”失败样本和独立的“采样数据陈旧”事件。API 恢复后，自动采集取得 4 个成功 PG18/K8S 样本，4 个陈旧事件均在各自原事件 ID 下转为 `resolved`。自动采集开关随后恢复为关闭。此处证明数据缺口和恢复可见；控制面反复超时使该单节点 kind **不适合作为可靠性或生产容量验收**。

再次在隔离的 `dbsphere-go-fault-pg` Deployment 注入 `/bin/false` readiness probe，缩容再扩容后，真实 PG18 Pod `dbsphere-go-fault-pg-c4fb9749b-wm6cs` 运行但 `Ready=False`，Service 无可用 Endpoint。浏览器手动采样在同一事件 `c1bf6870-274f-4d7b-8386-bde52a648bd3` 中记录 `PostgreSQL connection refused`、Pod 条件、探针 `Unhealthy` Event 与 PG 不可用告警；超过 60 秒的第二次采样增加 `k8s_pod_not_ready` 告警及低置信度“PostgreSQL Pod 未就绪”根因候选，候选注明匹配条件、样本 ID 和因果限制。恢复原探针并完成滚动后，成功采样 `65c2a085-d68b-4827-a67f-aedf2cdf0d39` 把**同一事件 ID**转为 `resolved`，原故障、候选与告警恢复记录仍可查看。此次事件时间窗没有应用 Trace，页面明确标为信号缺口；不能据此声称应用请求链路已在本场景验证。

新镜像的 Go 全量测试、`go vet ./...` 和 `node --check public/app.js` 本机通过。验收后重新轮换 kind 测试管理员密码，新密码仅在 `H:\CodexLS\outputs\DbSphere-kind-admin-password.txt`；新密码登录返回 200。测试存储使用 `emptyDir`，多节点、持久卷恢复、PG14–18 × K8S 矩阵、持续负载和生产集群仍未完成。

## 环境和边界

- Go 后端与静态前端运行在 Linux 容器中，通过本机 `127.0.0.1:18090` 端口转发访问；采集目标和长期数据存储分别是两个 PG18 Deployment。测试存储用 `emptyDir`，Pod 被替换会丢失数据，不能据此声称持久化验收。
- Go Pod 使用 `dbsphere-reader` ServiceAccount 的 Pod 内投射凭据；它可只读获取目标 Namespace 的 Pod、Event、Service、PVC、PV、Node 及 Pod/Node 资源指标。实际 impersonation 检查 `get secrets`、`delete pods` 均返回 `no`。故障注入由测试集群管理员进行，不授予 DbSphere 采集器写入集群的权限。
- 已从正常目标采到 PG18、Pod、Service、PVC/PV、Node 和 metrics-server 的 Pod/Node CPU/内存。Kubernetes 资源证据与 PostgreSQL 采样写入独立 PG18 存储。

| 故障注入和实际观测 | 同一事件 ID | 根因候选与恢复证据 |
| --- | --- | --- |
| PG18 探针持续失败：Service 无就绪端点；PG 连接失败，Pod `Ready=False`，出现 `Unhealthy` Event。 | `37821d0d-9309-4582-a37d-90314c49abef` | `k8s_pod_not_ready` 低置信度；修复探针并采到成功样本 `6b60df49-f5a0-4cb5-8bf6-3ba6b8dbaf3e` 后，原事件关闭，人工恢复验证和管理审计关联该事件。 |
| PG18 Pod 内存限制 16 MiB：容器实际 `OOMKilled`，PG 连接失败，K8S 终止状态与 OOM 告警进入同一事件。 | `58d989b1-ecaa-4e65-acb6-ff50447fd791` | `k8s_oom_killed` 中置信度；内存限制调回 256 MiB 后，成功样本 `fefc1085-8c94-414e-9fae-811458a9b379` 关闭原事件。 |
| PG18 Pod 指向不存在的 Node 标签：实际 `Pending`、`PodScheduled=False/Unschedulable` 和 `FailedScheduling` Event；Service 无端点，PG 连接失败。 | `109fcab1-f7c1-456b-aa13-9899a5b23c2f` | `k8s_scheduling_event`、`k8s_scheduling` 均为低置信度；移除错误 Node 选择器后，成功样本 `3b396529-04b1-4f3b-aa42-de4955391dd9` 关闭原事件，已删除的旧 Pod 名自动解析到新 Pod。 |

### 应用连接失败 → PG Pod OOMKilled（场景 B）

使用[Go HTTP 示例应用](../examples/trace-lock-app/README.md)的静态 Linux 镜像，在同一 Namespace 运行真实 `/checkout` 请求，OTLP/HTTP JSON 通过独立令牌发往 kind 内 DbSphere。基线请求成功，服务父 Span 和 PG 客户端 Span 经 Go 接口写入独立 PG18 存储，带真实后端 PID。

测试管理员把 PG18 Deployment 限制到 16 MiB，实际新 Pod `dbsphere-go-oom-pg-67576dd6b7-gzz8h` 出现 `OOMKilled`；Go 应用请求返回 HTTP 502，并导出 Trace `e46e8da8d03862799d72df932d5dd51c` 的两个 `error` Span（连接失败时无后端 PID）。DbSphere 对映射到该 Pod 的资产采样 `18b2921a-0908-4238-9b6c-b5e6491e0814`，将 PG 连接失败、K8S 容器终止状态、OOM 告警和中置信度 `pod_oom_correlated` 候选写入**同一事件** `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7`；该事件图包含失败请求的服务 Span → PG 客户端 Span → PG 资产，以及候选到原始 K8S/PG 证据的边。

将内存限制恢复为 256 MiB 后，新 Pod 就绪；成功采样 `89e858aa-7593-4ee0-afb1-c2ee87ef9301` 自动把已删除的旧 Pod 名解析到新 Pod，原事件变为 `resolved`，恢复证据仍挂在该 ID。应用再次请求返回成功。场景 B 目前只在单节点 kind 本机测试，尚无多节点/生产 Operator、真实业务应用或自动 CI 演练；事件时间窗还包含故障前一次健康 Trace，因此影响面数值必须按错误 Span 与时间逐项核对。

更新 Go 静态镜像为 `dbsphere/kind-acceptance:trace-impact` 后，在同一个已恢复 OOM 事件 `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7` 上复验：服务 `dbsphere-lock-demo` 的同期观测为 3 条 Trace、6 条 Span，**仅 1 条**带数据库错误的 Trace `e46e8da8d03862799d72df932d5dd51c` 被标为故障信号关联；另 2 条正常同期 Trace 未算入。管理员下钻原始脱敏 Trace 读到 2 条 `error` Span，Markdown 复盘也包含“故障信号关联 1 条”及该 Trace 链接。该分类证明事件页不再把所有同窗请求都称作故障影响；请求错误与 OOM 的因果关系仍须结合事件时间、Pod 状态和人工复核判断。

Playwright 实际浏览器登录同一 Go B/S 页面，从“问题事件”打开上述 OOM 事件，展开“查看应用请求关联证据”，看到“同期 Trace 3 条 · 故障信号关联 1 条”和准确的原始 Trace 链接；点击后浏览器读到 2 条错误 Span JSON。功能调用无脚本错误；导航到原始 JSON 时浏览器额外请求 `/favicon.ico` 返回 404，不影响 Trace 下钻。[该提交的 22 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44278562315)全部通过，覆盖 PG14–18 与既有离线部署门禁。Playwright 所需 Node/npm 只安装在本机开发工具目录，不属于 Go 静态部署包。

管理员密码轮换复验：Go 开发版曾只在空库启动时初始化管理员密码，修改 K8S Secret 后新密码登录返回 401。补充显式 `rotate-admin-password` 静态命令后，将镜像 `dbsphere/kind-acceptance:admin-rotate` 装入 kind，在使用新 Secret 的 Go Pod 内执行该命令并滚动重启；旧密码登录返回 401，新密码登录为 `admin`、可读取原有 4 个资产，`/readyz` 仍报告 `postgresql18`。新密码只保存在本机 `H:\CodexLS\outputs\DbSphere-kind-admin-password.txt`，未进入仓库。[22 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44282697078)进一步通过 Go 离线包实际轮换、旧密码拒绝和旧会话失效检查。

### 标准 OpenTelemetry Collector 转发

在同一 Namespace 运行官方 `otel/opentelemetry-collector:0.165.0` Linux 镜像。其 OTLP HTTP Receiver 监听 `:4318`，`otlp_http` Exporter 以 JSON、无压缩和独立写入令牌发往 Go DbSphere `/v1/traces`；配置在[无密钥示例](../examples/trace-lock-app/otel-collector.yaml)。将示例应用的 OTLP 目标改成 Collector Service 后，真实 `/checkout` 请求成功，返回 Trace `f331080ca67953004cd1adb745d14dfd` 和 PG 后端 PID `1212`。管理员从 DbSphere 的 PG18 存储读取同一 Trace，确认服务父 Span → PG 客户端 Span 共 2 条，均关联受管 PG 资产，客户端 Span 保留实际 PID。Collector 镜像配置验证也通过。此项只证明本机标准 Collector 的 Trace JSON 转发和资产关联；未验证生产 TLS、队列持久化、第三方 SDK 自动埋点或负载下丢包率。[Exporter 配置依据](https://github.com/open-telemetry/opentelemetry-collector/blob/main/exporter/otlphttpexporter/README.md)。

另以[官方 Go SDK 验收程序](../examples/otel-sdk-probe/README.md)的 OTLP HTTP/Protobuf Exporter 向 Collector 发送真实 PG18 查询的父子 Span，Collector 再以 JSON 发给 DbSphere。首次实测只保存数据库子 Span，暴露了分批导出时父 Span 先后到达及并发提交的缺口。Go 后端新增脱敏短期待关联表、父链补全和提交后二次关联，PG18 测试分别覆盖“父先子后”“子先父后”及并发提交后的修复。新静态镜像在 kind 内复验：Trace `1cbbd2b6fb9addc7f764b1f7dde1d508` 读回服务父 Span 和 PG 客户端子 Span，后端 PID `6327`；归档到仓库的可复现程序再生成 Trace `5cab080d463f1b775b5c7e7c11c40fd5`，同样读回 2 条父子 Span、唯一受管 PG 资产和实际 PID `6713`。由于本机 controller-manager 失去 Leader Lease 时 Deployment 更新受阻，本项后段验收用管理员直接创建的临时测试 Pod，DbSphere 仍使用原只读 ServiceAccount；这不算生产高可用验收。其他语言 SDK 和跨服务自动传播仍待测试。

## 判定

### 2026-11-18 本机复验

复用本机已运行的单节点 `dbsphere-test` kind 集群，重新导出管理员 kubeconfig，仅用于测试故障注入；DbSphere Go Pod 仍使用 `dbsphere-reader` 的 Pod 内只读凭据，没有签发新 ServiceAccount token。`kubectl auth can-i` 复核该账号可 `get pods`、`list events`、`get nodes`，不可 `get secrets` 或 `delete pods`。Go `/readyz` 返回 `ready/postgresql18`；真实资产新采样 `784ee043-5bb6-4db4-8b2a-4dfc4fbc67eb` 为 `ok`，读到 PG 18.6、就绪 Pod、Service、绑定 PVC/PV、Node 与 `metrics.k8s.io` 的 Pod/Node CPU 和内存。PVC 实际文件系统用量仍未配置 Prometheus，样本明确显示 `not_configured`。

在隔离测试 Deployment `dbsphere-go-fault-pg` 上将 readiness probe 临时改为 `/bin/false`，缩容再扩容使 Service 仅指向未就绪 PG18 Pod。真实 Pod `dbsphere-go-fault-pg-c4fb9749b-phnpv` 变为 `Ready=False`，出现 `Unhealthy` Event；Go 采样 `f2a077df-748e-4989-9ffc-813c9f93b762`、`6aa8a420-02d1-45d1-9b29-3081628902be`、`bca2e808-8560-4baa-b6e7-fd7793a05911` 均报 PG 连接失败。**同一事件** `4f76762c-191f-4fdc-bdad-77cc257b4b4d` 的图关联 PG、Service、Pod、Node 和样本；持续 60 秒后 `k8s.pod_not_ready` 告警生成有证据与阈值的低置信度 `k8s_pod_not_ready` 候选。恢复原探针并完成 Deployment 滚动后，新 Pod 就绪，成功 PG18/K8S 样本 `0f07c67e-eb47-4331-ae7d-6e491267d40e` 将**原事件 ID**置为 `resolved`，同时保留故障、告警恢复及根因候选证据。演练未涉及生产集群，也未验证该场景的应用 Trace；本机控制面曾有较高重启计数，不作为可靠性测试环境。

三种故障均在**一个问题、一个事件 ID**下记录数据库故障、K8S 状态、规则告警和恢复采样。根因输出是带证据与不确定性的候选，不把关联当作已证实的唯一原因。浏览器已查看探针故障的事件时间线、候选与审计；调度故障的 API 证据及同 ID 恢复已核对。

更新 Go 验证镜像后重新读取上表的真实 OOM 事件：事件仍为 `resolved`，只显示一条 `pod_oom_correlated` 中置信度候选，候选关联采集失败、K8S 状态、OOM 告警、原始样本等 5 条支撑证据，并保留匹配条件。镜像滚动期间本机 kind 的 API/etcd 短暂超时，恢复后才完成本项复核；该中断不算生产可靠性验收。

另用原资产中**已删除的旧 Pod 名**再次注入 OOM，全程不修改资产映射：失败样本 `cb069214-696a-4846-99be-6c98a5be6c56` 只读解析到唯一未就绪的新 Pod，看到 `CrashLoopBackOff`/`OOMKilled`，创建事件 `4b72b565-ebcb-4203-9f03-5069bebc93fa`；恢复资源限制后样本 `6ceb5150-ddb2-4dd5-a4dc-f636ad23b781` 自动解析到健康 Pod，并把同一事件置为 `resolved`。对应的 TLS 模拟 API 回归测试也覆盖唯一未就绪替换 Pod；多 Pod 仍报告映射歧义。

加入 Kubernetes `continue` 安全分页后，以提交 `a3660b9` 构建的新 Go 静态镜像在同一只读 ServiceAccount 下重新采样：样本 `ba7f6eda-536a-443e-9a0e-6a9a10507443` 为 `ok`，旧 Pod 名仍解析到当前唯一健康 Pod，`metrics.k8s.io` 与 Service 证据可读。TLS 模拟 API 测试使 Deployment、ReplicaSet 和 Pod 都跨到第二页，验证找到唯一目标并拒绝循环令牌；[22 作业 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44258811967)全部通过。真实 kind 的资源数未超过单页，所以此处只证明新代码在实际集群没有回归，实际千项规模仍待容量验收。

尚未验收多节点和生产集群、PVC 压力等其余故障、第三方 SDK、持续负载、持久存储故障及完整 PG14–18 × K8S 组合。V1.0 的八类故障和两类应用链路门槛仍未完成独立环境及生产级验收；见[开发任务清单](DEVELOPMENT_TASKS.md)。

### 2026-11-19 Go 保留期代码滚动复验

提交 `cedd75c` 新增 Go/PG18 可选历史数据保留期，[31 项 Linux CI](https://github.com/cuipengdba/DbSphere/actions/runs/44397517514)全部通过。以 `dbsphere/kind-acceptance:retention-cedd75c` 在原单节点 kind Namespace 滚动更新 Go Deployment；新 Pod `dbsphere-go-7bf76986bd-vrdf5` 1/1 Ready、0 次重启，`/readyz` 返回 `ready/postgresql18`。原 OOM 闭环事件 `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7` 仍为 `resolved`，读回 8 条证据和 1 条根因候选；事件列表仍有 17 条，PG18 存储状态显示 122 条样本。测试集群未设置样本或事件保留期，控制台 API 两项均为 `null`，本次只证明关闭自动清理时升级不丢已核对数据。清理逻辑另由隔离 PG18 集成测试验证，生产容量与备份恢复仍待验收。

另在同一测试 Namespace 临时摘除 PG18 Service 的 Pod selector。原 Go 进程仍持有有效的 PG18 TCP 连接，因此 `/readyz` 继续返回 200；这一步不能算真实存储失联。保持 Service 无端点时滚动重启 Go Deployment，新 Pod `dbsphere-go-68b9c878c6-2nhlv` 未就绪，前次启动日志记录连接 `dbsphere-go-store:5432` 被拒绝。恢复原 Service selector 后，Deployment 完成滚动并就绪（该 Pod 曾重启 1 次）；API 再次读回同一事件的 8 条证据、1 条候选、122 条样本和 17 个事件。存储 Pod 始终运行，避免其 `emptyDir` 测试卷被删除。此演练仅覆盖新进程启动时 PG18 无法连接及恢复，未覆盖运行中 TCP 断连、持久卷损坏或生产备份恢复。
