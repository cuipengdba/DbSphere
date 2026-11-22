# 外部运维系统接入

## 部署配置

容器化部署把下列变量写入安装目录的 `.env`，原生部署使用 systemd 服务的环境文件。应用重启后生效。通过 HTTPS 入口请求接口。

| 配置 | 用途 |
| --- | --- |
| `DBSPHERE_OPS_TOKEN` | 版本化运维 API 的 Bearer 凭据，长度 24–256 个字符。 |
| `DBSPHERE_OPS_ORGANIZATION` | 令牌和签名 Webhook 对应的组织 UUID，省略时使用默认组织。在组织目录可查看组织 ID。 |
| `DBSPHERE_OPS_WEBHOOK_URL` | 本组织的 HTTPS 接收地址。可同时保留已有全平台 Webhook。 |
| `DBSPHERE_OPS_WEBHOOK_SECRET` | 组织级 Webhook 的签名密钥，长度 24–256 字节。 |
| `DBSPHERE_METRICS_TOKEN` | Prometheus 抓取的独立 Bearer 凭据，长度 24–256 个字符。 |

## 版本化 API

请求使用 `Authorization: Bearer <DBSPHERE_OPS_TOKEN>`。成功响应使用 `schemaVersion: 1` 和 `data`。分页接口接受 `limit` 1–100、`cursor` 及 `q`，游标使用上页返回的 `nextCursor`。详细接口和写入请求都核验组织归属。

| 方法 | 路径 | 返回 |
| --- | --- | --- |
| GET | `/api/v1/assets` | 资产列表，支持 `runtime`。 |
| GET | `/api/v1/assets/{asset}` | 资产、运行环境和最新状态。 |
| GET | `/api/v1/events` | 事件列表，支持 `status`。 |
| GET | `/api/v1/events/{event}` | 事件状态、时间、控制台路径和最近 100 条外部证据。 |
| POST | `/api/v1/external-incidents` | 记录外部告警触发、恢复或工单关联，返回 `eventId`、`evidenceId` 和状态。 |

外部告警发出 `kind=alert`、`state=firing`，并传入来源、外部告警发生 ID、资产 ID 和标题。可指定已有 DbSphere `eventId`，也可由 DbSphere 创建事件。恢复时传入相同的来源、发生 ID 和资产，并使用 `state=resolved`。由外部告警创建的事件随该恢复消息关闭。已有数据库告警从自身规则判断恢复。一条外部告警恢复后再发生时使用新的外部发生 ID。

工单系统收到 Webhook 后可按原始 `deliveryId` 幂等创建工单，然后将工单号作为 `externalId` 回传，同时携带 Webhook 的 `assetId` 和 `eventId`。

```json
{
  "source": "support-desk",
  "externalId": "DB-2026-124",
  "kind": "ticket",
  "state": "linked",
  "assetId": "4f1d0c20-1fcb-44cc-befc-0337d4f49220",
  "eventId": "4b970469-b0a9-43bf-b1d4-8a303718b17a",
  "title": "连接等待跟踪",
  "url": "https://tickets.example.org/DB-2026-124"
}
```

首次记录返回 201。同一组织、来源、外部 ID、类型和生命周期阶段的重复请求返回原事件及证据 ID。将相同外部 ID 指向另一个资产或事件时返回 409。组织范围外的资源返回 404。

## 签名 Webhook

接收端用本组织密钥和原始 HTTP 请求体计算 HMAC-SHA256，核对 `X-DbSphere-Signature` 中的十六进制签名。使用 `Idempotency-Key` 或请求体中的 `deliveryId` 去重，处理完成后返回 2xx。DbSphere 对 429 和 503 读取 `Retry-After` 秒数或 HTTP 日期，并在重试时沿用同一投递 ID。投递状态与事件证据一起保存。

## Prometheus 抓取

```yaml
scrape_configs:
  - job_name: dbsphere
    metrics_path: /metrics
    scheme: https
    authorization:
      credentials_file: /etc/prometheus/secrets/dbsphere-metrics
    static_configs:
      - targets: ['dbsphere.example.org:443']
```

平台指标包括 PG18 可达性、单副本采集次数及最近批次耗时、采集失败，以及共享通知队列的待处理和失败数量。多副本部署可逐台抓取采集计数。共享通知队列使用单个抓取目标或按最大值聚合。数据库采样继续从资产监控工作台查看。

[Prometheus 文本格式](https://prometheus.io/docs/instrumenting/exposition_formats/) · [HTTP Retry-After](https://www.rfc-editor.org/rfc/rfc9110.html#name-retry-after)
