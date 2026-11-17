# V0.3 告警通知

V0.3 当前实现了 PostgreSQL、Linux、Kubernetes、PgBouncer 受控规则告警；同一类故障按资产归并至一个事件 ID。浏览器可查看当前告警、修改规则、确认事件，以及创建或撤销静默和维护窗。静默期间持续保存事件及证据，不生成对外通知。

## Webhook

在 `deploy/compose/.env` 或 Linux 原生进程环境中设置：

```env
DBSPHERE_WEBHOOK_URL=https://your-webhook.example.net/dbsphere
DBSPHERE_WEBHOOK_SECRET=replace-with-a-random-secret
DBSPHERE_WEBHOOK_MIN_SEVERITY=warning
```

Webhook 使用 POST JSON，正文含 `deliveryId`、`eventId`、`assetId`、告警规则 ID、级别、状态和观测时间；不包含数据库密码、连接串或原始 SQL。`X-DbSphere-Delivery-Id` 可供接收方去重；设置密钥后，`X-DbSphere-Signature` 为 `sha256=` 加正文的 HMAC-SHA256。默认要求 HTTPS；仅在受信任的内网调试中可显式设置 `DBSPHERE_WEBHOOK_ALLOW_HTTP=true`。Webhook 返回非 2xx 会进入重试。

## 邮件

```env
DBSPHERE_SMTP_HOST=smtp.example.net
DBSPHERE_SMTP_PORT=587
DBSPHERE_SMTP_USER=dbsphere@example.net
DBSPHERE_SMTP_PASSWORD=replace-with-app-password
DBSPHERE_SMTP_FROM=dbsphere@example.net
DBSPHERE_SMTP_TO=oncall@example.net
DBSPHERE_EMAIL_MIN_SEVERITY=critical
```

465 端口使用连接初始 TLS，其余端口要求 STARTTLS。SMTP 用户名和密码必须一起配置；若服务器不要求认证，可同时省略。`DBSPHERE_NOTIFY_MIN_SEVERITY` 设定两通道默认级别，单通道设置覆盖默认值。邮件和 Webhook 可只配置其一；未配置的通道将显示为“未配置或未达到路由级别”。不要把 `.env` 提交到 Git。

自动化测试另用 `DBSPHERE_SMTP_ALLOW_PLAIN=true` 和本机回环地址验证真实 SMTP 会话；此选项拒绝非回环主机，不用于正式邮件连接。

## 静默、升级和重试

控制台支持全局、按资产或按规则的最长 30 天时间窗。新建或撤销后，告警状态在下一次采样时更新；已有事件和证据不删除。未确认的**严重**事件持续超过 `DBSPHERE_ESCALATE_AFTER_MINUTES`（默认 15）后，在同一事件 ID 下生成一次升级证据并再次通知。确认事件或有效静默会抑制升级。

告警触发、解除静默、恢复和升级时，通知记录与证据写入同一存储事务。两个通道独立投递；失败后按 30 秒、60 秒、120 秒、240 秒退避，累计 5 次失败标记为失败。控制台“通知投递”显示状态、次数和不含敏感连接信息的错误摘要；管理员可手动重试失败记录。接收方应使用投递 ID 去重；当前单实例部署中的进程重启会保留待投递记录。多副本并发投递和生产 SMTP 服务验收仍待完成；本机 SMTP 协议会话已通过自动化测试。
