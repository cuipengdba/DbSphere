# DbSphere v4.4.0 openGauss 只读纳管

- 新增 openGauss 资产类型，支持单条和批量接入、首采以及原资产复检。首采保留会话、事务、容量、锁等待和各项权限状态。
- 对当前数据库的锁等待生成规则事件，记录等待会话、可见的阻塞会话和锁模式。触发及恢复采样均可返回原事件。视图缺采期间保持原等待事件开启。
- 资产详情、实时观测、大屏和 Grafana 增强包提供 openGauss 指标。采集和规则事件无需配置 AI 模型。

[接入手册](OPENGAUSS.md) · [支持矩阵](SUPPORT_MATRIX.md) · [部署手册](OFFLINE_DEPLOYMENT.md)
