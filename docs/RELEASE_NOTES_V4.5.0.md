# DbSphere v4.5.0 数据库变更治理

- 支持在 PostgreSQL 和 MySQL 资产上配置组织或资产级变更窗口，登记外部工单，由与申请人不同的审批账号批准或驳回。
- 上线前保存只读采样和权限覆盖。外部变更回执校验组织、时间窗、审批和基线，重复回执幂等返回。
- 上线后、回退后分别记录人工复核和原始采样。关联事件保留外部回执及采样回链，可继续查看同窗告警并对比 SQL 指纹。

[变更治理](CHANGE_GOVERNANCE.md) · [外部运维接入](OPERATIONS_INTEGRATION.md) · [支持矩阵](SUPPORT_MATRIX.md)
