# DbSphere V0.1.0 发行说明

Linux CI 与本地 PG/K8S 演练已通过。发行标签为 `v0.1.0`。

## 已提供

- B/S 控制台、管理员登录、CSRF 校验、PostgreSQL 资产管理和加密凭据。
- PG14–PG18 的基础指标与 11 个诊断分区；会话、等待、锁阻塞链、WAL、检查点、归档、autovacuum、表/索引容量和复制槽。
- 样本持久化、数据新鲜度、采集失败/恢复事件、同一事件 ID 下的证据时间线。
- Linux/K8S 运行位置，手工 Pod 映射，Pod 重启/OOM 状态、PVC 和 Pod 事件采集；有限的 OOM 根因候选。
- 无需 AI 模型即可登录、纳管、采集和查看事件。

## 验证范围

- PostgreSQL 14、15、16、17、18 容器：`pg_monitor` 只读账号完成基础与诊断查询，每版运行真实 advisory lock 阻塞链演练。
- Linux 原生 Node 进程在 Linux 容器中通过健康检查、PG 断连和重启持久化演练；Ubuntu GitHub Actions 作为最终发布门禁。
- kind 中 PG18 Pod + PVC：DbSphere B/S API 纳管、PG/K8S 联合采集及服务重启持久化通过；只读 ServiceAccount 可读目标 Pod/PVC/事件，无法读 Secret 或删除 Pod；测试容器重启被识别。

## 已知限制

- K8S Pod 映射需要手工维护；尚无 Node 指标、自动发现或 PVC 实际使用率。
- 无通用告警、巡检、SQL 指纹、应用 Trace、AI 接口、完整跨层根因引擎和 HA 控制。
- SQLite 单实例存储，尚未做长期规模、备份恢复和生产性能验收。
- 容器化一键部署和独立 PostgreSQL 18 长期监控存储纳入 V0.2，V0.1 仅提供手动启动方式。
- PG14 已到上游维护终点，控制台显示 EOL。

部署方式见[部署与接入](DEPLOYMENT.md)。V0.2–V1.0 任务见[全版本开发清单](DEVELOPMENT_TASKS.md)。
