# DbSphere 快速开始

用标准离线包在 Linux 安装 DbSphere，纳管第一个 PostgreSQL 实例，再从监控大屏进入同一个问题事件。需要 Grafana 专题时，选择增强离线包。

## 1. 选包并安装

目标机需要 Bash、Docker Engine 和 Compose v2。标准包和增强包都自带 Go 服务、静态前端和独立 PostgreSQL 18 监控库。增强包另外提供 Prometheus 和 Grafana OSS。安装阶段可以断网，运行时需要能访问被监控实例。

在能联网的机器打开[最新正式版](https://github.com/cuipengdba/DbSphere/releases/latest)，下载对应架构的 `.run` 和同名 `.run.sha256`，复制到 Linux 服务器的同一个目录。`uname -m` 显示 `x86_64` 时选 `amd64`，显示 `aarch64` 时选 `arm64`。以下以 v3.16.0 的标准包为例，按手头的安装包调整文件名。

```bash
bundle=DbSphere-go-offline-3.16.0-linux-amd64.run
sha256sum --check "$bundle.sha256" && bash "$bundle"
curl -fsS http://127.0.0.1:8081/readyz
```

就绪响应中应包含 `"status":"ready"` 和 `"storage":"postgresql18"`。使用增强版时，将 `bundle` 改成 `DbSphere-go-observability-3.16.0-linux-amd64.run`。已有独立 PG18 或需要原生 systemd 安装时，见[部署手册](OFFLINE_DEPLOYMENT.md)。

## 2. 首次登录

在部署机打开 `http://127.0.0.1:8081/`。服务器没有桌面时，在自己的电脑建立 SSH 转发，再打开相同的本机地址。把 `user@server` 换成服务器的 SSH 登录地址。

```bash
ssh -N -L 127.0.0.1:8081:127.0.0.1:8081 user@server
```

全新安装且未预设管理员密码时，用 `admin` 和初始密码 `1234567890123` 登录，按页面提示设置至少 12 字节的新密码。已预设密码或从旧版升级的安装使用自己的管理员密码。左下角显示正在运行的版本，右上角可切换深浅主题。

## 3. 纳管第一个 PostgreSQL 实例

使用数据库管理员账号在目标库创建专用采集账号。下面是 `psql` 中的示例，业务库名为 `postgres` 时可直接使用最后一条语句，其他库名按实际替换。

```sql
CREATE ROLE dbsphere_monitor LOGIN;
\password dbsphere_monitor
GRANT pg_monitor TO dbsphere_monitor;
GRANT CONNECT ON DATABASE postgres TO dbsphere_monitor;
```

1. 打开「资产与实例 → 纳管实例」。
2. 填写资产名、主机、端口、数据库、监控用户名和密码。主机填写 DbSphere 服务能访问的地址。容器部署时，从容器内部访问的 `127.0.0.1` 指向当前容器。
3. 按目标选择 Linux 主机、Kubernetes 或托管 PostgreSQL。启用证书验证时，使用与服务器证书匹配的主机名和受信 CA。可给相关实例填写同一个集群名，方便横向对照。
4. 保存后，在资产列表点击「采集」，再点「查看」核对采样时间、连接数和采集覆盖。确认数据库侧采集正常后，可接入 Linux Exporter、Kubernetes 只读 API 或其他数据源。Top SQL 还需要在目标数据库配置 `pg_stat_statements`。

## 4. 看大屏并沿同一个事件查证据

1. 打开「监控大屏」，按环境或集群筛选。引擎卡片分别显示近期采样、缺采、采集失败和端点失联，点击卡片可缩小实例与事件范围。点主机或实例卡片可切换当前资产，点任一小图可放大查看曲线、刻度和原始采样入口。大屏工具栏可以切换主题和进入全屏。
2. 发现异常后点击「优先处理」中的事件，或进入「问题事件」筛选。记下事件 ID，在「根因与证据」对照触发点、阻塞、SQL、采样以及可用的主机或应用证据。
3. 打开原始采样或日志上下文，再返回同一事件。管理员可展开「查找同窗日志」，采纳一条相关日志并点击「查看原文」。在「处置」记录核查结论，在「时间线与报告」核对恢复证据并导出报告。AI 可选，规则分析可直接使用。
4. 在大屏选择「历史基线」对照前一窗口、昨日或上周。用「选择变更前后对比」按变更记录对齐两个等长窗口。虚线表示基线，可以打开对齐时刻的原始采样。
5. 设置资产范围、指标组和布局后，将值班视图保存到个人或团队。个人视图随账号跨浏览器恢复，团队视图可通过「复制团队视图链接」分享给团队成员。接收者按自己的资产权限查看。已有浏览器视图可通过「导入浏览器视图」迁移。

增强包可从大屏或「实时观测」打开 Grafana 观测专题，沿用 DbSphere 的登录会话与选定时段。分析日志文件时，管理员打开「日志分析」，依次新建项目、登记实例、建立批次，再上传日志。实例关联已有资产后，可从日志调查进入同一事件。

## 5. 首次使用检查

| 现象 | 检查位置 |
| --- | --- |
| 浏览器无法打开 | 在部署机先检查 `/readyz` 和 `docker ps`。从其他电脑访问时，检查 SSH 转发和本地端口。 |
| 数据库采集失败 | 核对容器到数据库的网络、登录权限、TLS 证书和主机名。在资产详情展开「纳管体检」。 |
| 大屏暂无曲线 | 先跑一次采集，确认当前资产和时段。速率及差值类指标需要相邻有效采样。 |
| 主机、K8S 或 SQL 专题没有指标 | 查看采集覆盖，按对应专题补齐 Exporter、只读 API 或统计扩展。 |
| Grafana 专题不可用 | 确认运行增强离线包且使用平台管理员账号。原生监控和事件证据仍可用。 |

[部署与升级](OFFLINE_DEPLOYMENT.md) · [监控工作台](MONITORING_WORKSPACE.md) · [事件调查](INCIDENT_INVESTIGATION.md) · [日志分析](LOG_ANALYSIS.md) · [支持矩阵](SUPPORT_MATRIX.md)
