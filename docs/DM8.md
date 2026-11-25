# 达梦 DM8 纳管

DbSphere 通过内置 Go 探针调用现场安装的达梦 DIsql 客户端，采集 DM8 实例状态、会话、锁等待、数据文件和 Data Watch。存储、告警、事件调查和报告沿用独立 PostgreSQL 18。Linux 多架构 DbSphere 安装包自带探针命令，DIsql 和数据库授权由部署方准备。

## 准备采集账号

在目标 DM8 上创建专用账号并单独授予动态视图读取权限。探针逐项显示可用信号。锁等待需要同时读取 `V$TRXWAIT`、`V$TRX`，表空间和文件利用率需要 `V$DATAFILE` 与 `V$TABLESPACE`。

```sql
GRANT CREATE SESSION TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$INSTANCE TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$SESSIONS TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$TRX TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$TRXWAIT TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$DATAFILE TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$TABLESPACE TO DBSPHERE_MON;
```

需要 Data Watch 与原生性能诊断时，按实际构建添加以下视图授权。SQL 历史还需要 DBA 在数据库上启用对应统计。

```sql
GRANT SELECT ON SYS.V$RLOG TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$ARCH_STATUS TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$ARCH_SEND_INFO TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$ARCH_APPLY_INFO TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$RAPPLY_PARALLEL_INFO TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$DMWATCHER TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$SYSTEM_EVENT TO DBSPHERE_MON;
GRANT SELECT ON SYS.V$SQL_STAT_HISTORY TO DBSPHERE_MON;
```

## 启动探针

在已有 DIsql 的 Linux 主机上使用与主机架构匹配的 DbSphere Go 程序。探针只接收已配置的目标地址与端口。使用随机生成的至少 24 字符探针密钥。下面是探针与原生 DbSphere 服务同机、达梦连接也使用本机地址时的配置。

```bash
export DBSPHERE_DM8_DISQL=/opt/dmdbms/bin/disql
export DBSPHERE_DM8_PROBE_TARGETS=127.0.0.1:5236
export DBSPHERE_DM8_PROBE_TOKEN='替换成随机生成的长密钥'
./DbSphere-go-3.6.0-linux-amd64 dm8-probe
```

默认仅在 `127.0.0.1:9928` 监听。容器版可连接达梦主机上的 HTTPS 探针。在该主机上配置 `DBSPHERE_DM8_PROBE_LISTEN`、`DBSPHERE_DM8_PROBE_TLS_CERT` 和 `DBSPHERE_DM8_PROBE_TLS_KEY`。证书主机名必须匹配 DbSphere 中配置的探针地址，DbSphere 需信任其签发 CA。探针到 DM8 的首版采集使用受信网络连接。

在 DbSphere 的环境文件配置 `DBSPHERE_DM8_PROBE_URL=http://127.0.0.1:9928`（仅原生同机）或 `https://探针主机:9928`，同时填写相同的 `DBSPHERE_DM8_PROBE_TOKEN`，重启 DbSphere 服务。

## 纳管与调查

在「资产与实例 → 纳管实例」选择「达梦 DM8」，填入探针授权清单中的目标地址和端口及专用监控账号。采集成功后可在资产详情查看数据文件和锁等待链，也可在「实时观测」按达梦引擎筛选趋势。增强包提供达梦 Grafana 专题。

锁等待事件记录等待与持锁事务 ID，以及采样时可解析的两侧会话 ID。查看事件根因候选后，可打开原始采样。成功采样明确观测到等待解除时，恢复记录写入同一个事件。已导入且绑定该资产的达梦日志可从「查找同窗日志」查看并采纳。

Data Watch 主库和备库作为独立资产纳管，使用相同集群名。资产详情的「主备拓扑与复制证据」显示实例身份、守护组、归档状态、可见的收发和回放位点及角色时间线。对端回放视图按数据库刷新周期更新，实时追平以备库直采为准。可以选填 Linux Exporter 或 Kubernetes Context、Namespace 与 Pod 或 Workload，用同一原始采样核对基础设施。

已验收 DM8 Linux x86_64 构建 `03134284604-20260707-335949-20228` 和两节点 Data Watch 实时主备。 [达梦动态性能视图](https://eco.dameng.com/document/dm/zh-cn/pm/dm8-admin-manual-appendix2.html) · [Data Watch 视图](https://eco.dameng.com/document/dm/zh-cn/pm/data-watch-appendix.html)
