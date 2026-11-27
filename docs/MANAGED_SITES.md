# 多站点接入与委派运维

在每个站点部署 DbSphere 和独立 PostgreSQL 18 监控库。资产凭据、采样、日志和规则事件保存在站点。站点按原配置执行采集和告警，中心仅接收所选组织的资产健康分布、事件编号与状态、近期恢复和签收数量。中心页面显示站点观测时间、中心接收时间与断联状态。

1. 在中心的接收配置中登记站点标识和双向 TLS 客户端证书指纹。证书文件及接收配置路径沿用站点采集手册。[站点采集配置](SITE_COLLECTION.md)
2. 在中心打开「站点管理」，登记同一标识、客户组织、站点控制台 HTTPS 地址及资产容量。可委派该组织下的一个已启用值班员。该账号可以处理被委派站点的升级记录。进入原站点时使用原站点账号登录。
3. 为每个站点启动 `dbsphere site-summary /etc/dbsphere-sites/summary.json`，配置环境使用站点的 `DBSPHERE_DATABASE_URL`、管理员初始化参数及采集周期。`localVersionUrl` 使用同机回环地址，读取实际运行的控制台版本。

```json
{
  "endpoint": "https://center.example.org:8444/v1/site-summary",
  "certificateFile": "/etc/dbsphere-sites/client.crt",
  "keyFile": "/etc/dbsphere-sites/client.key",
  "caFile": "/etc/dbsphere-sites/server-ca.crt",
  "organization": "00000000-0000-4000-8000-000000000001",
  "localVersionUrl": "http://127.0.0.1:8080/api/version",
  "intervalSeconds": 30
}
```

中心保留最后接受的摘要。断联后，站点在恢复连接时上报最新汇总，完整采样和事件历史可在本地回查。超过三个上报周期未取得有效摘要时显示断联。容量超限时中心显示最近拒绝的资产数，可通过收窄汇总范围或调整限额恢复上报。

升级前为站点登记批次、顺序和目标版本。一个批次从顺序 1 开始，各顺序连续。前序站点全部核验通过后，才可标记后续站点开始升级。值班员在站点运行对应离线安装包，并在中心登记结果。升级和回退核验要求状态变更后的最新有效摘要显示预期版本。核验保留摘要 ID、操作人和说明。
