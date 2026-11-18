# 本机 PostgreSQL 18 备份、恢复与 PVC 重启验收

2026-11-19，在本机专用单节点 kind `dbsphere-test` 的 `dbsphere-smoke` Namespace 完成开发环境演练。本记录覆盖当前 Go 版独立 PG18 监控库的数据备份、恢复到新 PVC、存储 Pod 替换，以及 Go B/S 服务从恢复库读取原事件并继续采样。它不代表生产备份策略、异机灾备或 V1.0 正式版验收。

## 操作与实测结果

1. 原 `dbsphere-go-store` 使用 PostgreSQL 18.6 和 `emptyDir`。在运行中的 Pod 内执行 `pg_dump -Fc`，复制到本机 `H:\CodexLS\outputs\DbSphere-kind-pg18-restore-acceptance.dump`。归档为 126970 字节；本机与 Pod 内 SHA-256 同为 `93f12e7b2cb42628a4cdaef666d883b8294c6046f446e49932af41b1c47ca679`，`pg_restore --list` 可读取。
2. 在同一测试 Namespace 新建独立 `dbsphere-restore-store-data` PVC，`standard` local-path、1 GiB、`Bound`；新 PG18 Deployment `dbsphere-restore-store` 挂载该 PVC，原存储 Deployment 未改动。用 `pg_restore --no-owner --no-acl --exit-on-error` 将归档恢复到新库，命令成功。
3. 原库与新库在恢复后读取相同的 `server_version_num=180006`、DbSphere schema 21、7 个资产、129 条样本、17 个事件、343 条证据、172 条证据样本引用、15 条 Trace Span；事件 `50f7124d-cfa9-42eb-9ca8-d2f25982a4c7` 均有 8 条证据。
4. 删除新 PG18 Deployment 的第一个 Pod，新 Pod 从同一个 PVC 启动后，上述数据量完全相同。为恢复库创建独立的 `dbsphere-go-restore` Go Deployment 和 Service；它沿用原主密钥及管理员配置，只把数据库 URL 指向新 PG18 Service，自动采集保持关闭。恢复库的 Go `/readyz` 报告 `postgresql18`，登录、静态页面、原 OOM 事件及 Markdown 报告均可读取；从修正后的 PostgreSQL 只读资产取得新的 PG18/K8S 成功样本。
5. 在 Go 已连接恢复库时，再次删除并重建新 PG18 Pod。存储恢复 Ready 后，[本机验收脚本](../scripts/kind-live-acceptance.mjs)再次通过：同一 OOM 事件保持 `resolved`，8 条证据、24 个图节点、47 条关系、6 条 Span（3 条数据库客户端 Span）及 `pod_oom_correlated` 候选可读；新采样 `dad94ab4-531f-43af-9f72-c41d93412ebf` 成功，另一个资产的 PVC/PV、Pod/Node 指标也可读取。脱敏结果保存在本机 `H:\CodexLS\outputs\DbSphere-kind-restored-go-acceptance.json`。

验收中使用的临时清单及不打印密钥的 Secret 派生脚本分别留在本机 `H:\CodexLS\tmp\dbsphere-kind-pg18-restore.yaml` 与 `dbsphere-kind-restore-secret.mjs`；测试管理员 kubeconfig 也仅在 `H:\CodexLS\tmp`。未将任何测试密码或 kubeconfig 提交到仓库。

## 边界

这是单节点 kind 的 local-path PVC，不能证明节点损坏后可恢复。快照时自动采集关闭、数据量小，也未模拟并发写入、WAL/PITR、跨版本恢复、异机备份校验或真实生产 I/O。原 `dbsphere-go-store` 的 `emptyDir` 仍作为独立旧实例运行；本次没有把其 Service 切换到恢复库。V1.0 存储门槛继续保持未完成。
