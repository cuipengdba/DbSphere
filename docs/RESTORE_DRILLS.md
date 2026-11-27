# PostgreSQL 备份恢复演练

DbSphere 读取 pgBackRest 2.60.1 导出的仓库清单。采样保存备份年龄、全量和差异备份的依赖、WAL 逐段核查结果，以及隔离恢复的事务证据。Linux amd64 和 arm64 的标准、增强和原生包均附带静态 pgBackRest。 [离线安装](OFFLINE_DEPLOYMENT.md#安装-pgbackrest)

## 接入备份证据

在业务库侧使用独立的 pgBackRest 配置和备份计划。按资产 UUID 将单个 stanza 的 `pgbackrest --stanza=名称 --output=json info` 导出到 `<资产 UUID>.json`，通过临时文件写入完整内容后再原子替换。给 DbSphere 服务账号最小读取权限。原生服务配置 `DBSPHERE_BACKUP_INFO_DIR`，容器部署配置 `DBSPHERE_BACKUP_INFO_HOST_PATH`。文件上限 1 MiB。

可在 JSON 中使用 `pgbackrest` 数组包装原输出，并加上 `lastAttempt`、`walChecks` 和 `restoreTests`。多个仓库或数据库世代的记录须分别标明 `repoKey`、`databaseId`。WAL 清单须给出同时间线的起止段号、段字节数与仓库实际列出的段号。未提供清单时，界面仅展示归档边界。 [字段与示例](MONITORING_WORKSPACE.md#备份与时间点恢复)

## 季度演练

1. 在 PostgreSQL 资产的「备份恢复演练」填写原风险事件 ID、恢复时点、隔离目标标识、回环地址和端口，以及目标 RPO 和 RTO。由独立审批账号审核。
2. 核对隔离数据目录、端口与访问边界后登记开工。还原操作由运维人员在隔离环境执行。使用独立的数据目录和监听端口，准备目标时点前后的事务标记。将恢复后的目标前标记存在和目标后标记缺失纳入核验。
3. 将 `targetId`、备份标签、仓库与数据库编号、恢复目标和起止时间、事务标记及其提交时间写入 `restoreTests`。`targetId` 须与批准目标一致，记录的开工时间须晚于登记开工。重新采集同一资产，在演练单登记样本和证据 ID。失败可登记原因并保留原风险事件关联。
4. 下载恢复签收报告，核对目标、实测 RPO 与 RTO、差异、入选的备份链和原始样本。确认原始采样可返回同一风险事件。备份目录、目标隔离和业务数据完整性由现场人员复核。报告中的实测值取自本次事务标记和计时范围。

监控 PostgreSQL 18 的恢复应使用独立实例和原部署密钥。恢复后从「系统状态 → 存储 → 恢复清单」对照资产、规则、原始采样、日志引用和同一事件。 [监控库备份](OFFLINE_DEPLOYMENT.md#双副本与备份)
