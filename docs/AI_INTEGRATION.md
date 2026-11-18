# Go 版可选 AI 诊断接入

当前为开发预览能力。**未配置模型时，PostgreSQL/Linux/Kubernetes 采集、规则告警、巡检、证据图与根因候选照常运行。** AI 只在管理员于事件页点击“AI 辅助分析”后调用；不会自动执行数据库或 Kubernetes 变更，也不替代人工确认。

## 配置

Go 进程或 Go Compose 的环境文件可设置：

```dotenv
DBSPHERE_AI_CHAT_URL=https://your-model-endpoint.example/v1/chat/completions
DBSPHERE_AI_MODEL=your-model-name
DBSPHERE_AI_API_KEY=replace-with-provider-token
DBSPHERE_AI_ALLOW_REMOTE=true
DBSPHERE_AI_DAILY_LIMIT=20
```

接口使用兼容 `POST /chat/completions` 的非流式 JSON 请求。连接外部 HTTPS 模型须显式设置 `DBSPHERE_AI_ALLOW_REMOTE=true`；本机 `http://127.0.0.1:端口/.../chat/completions` 无需该开关。Go Compose 中的 `127.0.0.1` 指 **DbSphere 容器本身**。模型地址与名称必须成对配置；缺失时 AI 关闭。API Key 只从服务端环境读取，不返回浏览器。当前不支持明文 HTTP 远端模型服务；这类部署应提供 HTTPS 入口。

## 发送内容与结果边界

- 发送同一事件 ID 的受控复盘摘要，最多约 12 KiB。包含事件状态、根因候选、证据 ID、样本 ID 和信号缺口；资产名称、应用服务名、人工备注和原始 SQL/参数不发送。外部模型调用仍可能暴露事件元数据，仅应在获得授权后启用。
- 模型必须返回 `summary`、`evidenceIds`、`uncertainty` 和 `suggestedChecks` 的 JSON。服务端校验引用的证据 ID 必须属于当前事件，拒绝虚构或重复 ID。结果在页面标记为“待人工核验”，不直接写成确定根因。
- 单次调用最长 12 秒，响应上限 32 KiB，同一事件 30 秒内最多请求一次。失败、超时或格式不合要求时，页面仍可使用确定性规则和原始证据。
- 每 UTC 自然日默认最多发起 20 次模型调用，可用 `DBSPHERE_AI_DAILY_LIMIT` 设为 1–1000。PG18 在模型请求前原子预留并审计额度，成功、失败和服务重启后的调用都计入上限；达到上限返回 HTTP 429，规则诊断与采集不受影响。跨实例共用同一个 PG18 预算。
- PG18 的该事件证据时间线记录模型名、调用成功/失败及引用的证据 ID；不保存发送给模型的完整内容、API Key 或模型回复。正式版仍需完成模型预算、保留期、更多兼容模型及生产链路验收。

## 验收范围

当前单元与 PG18 实库测试覆盖禁用默认值、HTTPS 远端显式开启、本机模拟模型、超时、虚构证据 ID 拒绝、鉴权/CSRF 与同一事件 ID 的调用审计。真实浏览器已在本机模拟模型下验收“事件 → AI 建议 → 引用证据 → 同事件审计”。真实外部模型服务和 AI 预警尚未验收。
