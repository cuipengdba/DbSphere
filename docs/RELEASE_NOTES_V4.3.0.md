# DbSphere v4.3.0 SQL 性能退化与复测

- PostgreSQL 和 MySQL 8.4 事件新增同指纹双窗口对比。按相邻原始样本计算调用次数和耗时增量，标记低样本量、统计重置、实例变化与缺采。复测结果可从原始样本返回同一事件。
- 支持导入脱敏 EXPLAIN JSON 并并排核对节点和估算行数。PostgreSQL 计划携带 Query Identifier 时按原始 64 位数值核对。MySQL 计划按操作人确认的对应关系展示。
- PostgreSQL 慢日志按同库 Query ID 统计。MySQL 可选择包含库名的已导入慢日志记录，按操作人核对的日志指纹统计。分位数使用已导入的记录。
- 管理员可按组织配置 SQL 原文保留 0–30 天。默认关闭，原文由管理员手动附加，读取记录到审计。
- 证据助手可将复测聚合数据及脱敏计划差异交给独立模型审核，并校验结论的同事件引用。模型停用或引用无效时显示规则核查。

[SQL 性能对比与复测](SQL_PERFORMANCE_COMPARISON.md) · [诊断报告模板](SQL_PERFORMANCE_REPORT_TEMPLATE.md) · [支持矩阵](SUPPORT_MATRIX.md) · [部署手册](OFFLINE_DEPLOYMENT.md)
