# 站点采集与独立模型接入

v4.2.0 支持由站点的 Go 进程采集 PostgreSQL、MySQL 和 Redis。站点通过 TLS 1.3 向中心的独立接收端口上报，数据库凭据留在站点。兼容性按版本和拓扑记录。[支持矩阵](SUPPORT_MATRIX.md)

## 开通站点

1. 为接收端签发匹配访问地址的服务端证书，为每个采集器签发可用于客户端认证的证书。计算客户端叶证书 DER 的 SHA-256。中心将证书指纹登记到站点。一个站点可登记多个指纹以平滑更换证书。
2. 中心设置 `DBSPHERE_SITE_RECEIVER_CONFIG` 指向接收配置文件。原生安装可在服务配置中指定绝对路径。Compose 安装设置 `DBSPHERE_SITE_CONFIG_DIR` 指向存放 `receiver.json` 与证书文件的目录，配置中的证书路径使用容器内的 `/run/dbsphere-sites/`。授予容器进程运行账号（UID/GID `65532:65532`）遍历此目录及读取配置、证书和私钥的权限，限制其他账号读取私钥。接收进程监听 `0.0.0.0:8444`。设置 `DBSPHERE_SITE_BIND_IP` 为中心站点网络的宿主机地址，并按需设置 `DBSPHERE_SITE_PORT`。
3. 在中心新增资产，保存资产 UUID。将数据库账号密码留在站点的权限受限文件内。设置环境后，在中心执行 `dbsphere assign-site ASSET_UUID east`。
4. 在站点保存采集配置并执行 `dbsphere site-collector /etc/dbsphere-sites/collector.json`。可用系统的服务管理器运行同一命令。确认资产页显示站点、最后上报时间与待补送数量。

接收配置示例中的指纹须替换为站点证书的实际 SHA-256。

```json
{
  "listen": "0.0.0.0:8444",
  "certificateFile": "/run/dbsphere-sites/server.crt",
  "keyFile": "/run/dbsphere-sites/server.key",
  "clientCAFile": "/run/dbsphere-sites/site-ca.crt",
  "intervalSeconds": 30,
  "sites": {"east": ["REPLACE_WITH_CLIENT_CERTIFICATE_SHA256"]}
}
```

采集配置使用中心分配的同一资产 UUID，采样间隔与中心缺采判断保持一致。

```json
{
  "endpoint": "https://collector.example.internal:8444/v1/observations",
  "certificateFile": "/etc/dbsphere-sites/client.crt",
  "keyFile": "/etc/dbsphere-sites/client.key",
  "caFile": "/etc/dbsphere-sites/server-ca.crt",
  "spoolDirectory": "/var/lib/dbsphere-site/spool",
  "maxBytes": 1073741824,
  "maxFiles": 10000,
  "intervalSeconds": 30,
  "targets": [
    {"assetId":"00000000-0000-4000-8000-000000000003","engine":"postgresql","host":"db.site.internal","port":5432,"database":"orders","username":"dbsphere_monitor","sslMode":"require","passwordFile":"/etc/dbsphere-sites/orders.secret"}
  ]
}
```

## 断线、恢复与换证

- 每条观测先写入权限为 `0700` 的本地缓冲目录，收到中心确认后删除。重复提交用站点身份和观测 ID 去重。上报延迟和源端采样时间分别记录。中心将迟到样本保留为历史证据，当前状态由时间更新的采样决定。
- 缓冲达到文件数或字节上限后，保留已入队观测并在站点日志记录后续缺采。补送窗口为 72 小时。两次采集间隔内未收到新鲜观测时，中心根据缺采规则登记告警。恢复后从同一事件查看原始样本。
- 证书更换时先登记新指纹并重启接收端，再更新站点证书并重启采集进程，验证新证书后移除旧指纹并重启接收端。回切中心本地采集时运行 `dbsphere assign-site ASSET_UUID local`。变更数据库地址或账号前先回切，修改后重新分配。
- 验收时记录站点、目标版本、每个引擎的首采时间、断开及恢复时刻、峰值缓冲条数、缺采事件 ID、恢复采样 ID 和证书撤销结果。站点模式首批覆盖数据库层信号。主机与 Kubernetes 观测继续由既有采集路径提供。

## 接入模型

在「系统 → 模型接入」选择本地模型或客户 HTTPS 网关，填写 Chat Completions 地址、模型 ID、授权组织、每日调用上限和证据审核超时。留空组织表示平台级授权。每日上限按组织与 UTC 日期计数，失败调用也计入。界面连通性检查只发送固定测试文本。

本地模型地址使用环回地址。客户网关由部署端设置 `DBSPHERE_AI_ALLOW_REMOTE=true`，并在 `DBSPHERE_AI_ALLOWED_ORIGINS` 中登记 HTTPS 来源。内网证书引用通过 `DBSPHERE_AI_CA_REFERENCES` 配置为名称到 Base64 PEM 的 JSON 对象。密钥引用通过受控的 `DBSPHERE_AI_KEY_REFERENCES` 配置为名称到密钥的 JSON 对象。引用在模型表单中选择或填写，按部署方式保护环境配置文件。修改部署引用后重启服务。独立关闭模型后，事件审核继续显示规则核查结果。

实测本地模型为 llama.cpp b7256、Qwen2.5-0.5B-Instruct Q4_K_M，使用 Intel Xeon Silver 4214R CPU，在无外网路由的验收网络完成模型连通与事件证据审核。模型服务、权重与其许可证随推理环境独立管理。[支持矩阵](SUPPORT_MATRIX.md)
