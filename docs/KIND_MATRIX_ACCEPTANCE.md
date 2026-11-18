# 本机多节点 PostgreSQL 14–18 × Kubernetes 验收

> 2026-11-18 开发版验收。四节点 kind `agentsql-v350` 的独立 `dbsphere-matrix` Namespace；没有修改该集群原有业务 Namespace。本记录只覆盖下述实际检查，V1.0 正式发布门槛仍未完成。

## 拓扑与结果

| 组件 | 节点 | 验收 |
| --- | --- | --- |
| DbSphere Go 静态前端/API | control-plane | `/readyz` 返回 `ready/postgresql18` |
| 独立 PostgreSQL 18 长期存储 | worker2 | `dbsphere-store-data` PVC 为 Bound；Go Pod 重启后资产和样本保留；存储 Pod 替换后再次采样成功 |
| PostgreSQL 14、15 目标 | worker | 只读 `pg_monitor` 账号连接、版本识别、真实样本、Pod/Service/Node 关联通过 |
| PostgreSQL 16、17、18 目标 | worker3 | 同上；PG18 的 `pg18-data` PVC 为 Bound |

五个目标的样本状态及 Kubernetes 证据均为 `ok`；Pod 名与对应 Deployment、Service 一致，Node 分布在两个工作节点。验收程序核对 `dbsphere-reader` ServiceAccount 可读取目标 Namespace 的 Pod、Event、PVC 和集群 Node/PV；不能读取 Secret、删除 Pod 或列出原有 `agentsql` Namespace 的 Pod。DbSphere Go 使用 Pod 内投射的只读 ServiceAccount 凭据，故障注入或资源部署使用独立管理员 kubeconfig。管理员 kubeconfig 和随机测试密钥没有提交到仓库。

本机 Playwright 浏览器登录后，总览显示五个 K8S 资产和五个“最近一次成功”；从总览直接进入 PG18 资产详情，显示五版资产、PG14 EOL 标记、PG18 深度指标，以及 Pod、Service、Node、PVC/PV 与 Kubernetes Event。桌面和 390 px 窄屏页面可读，浏览器控制台没有错误。截图仅保存在本机 `H:\CodexLS\outputs\DbSphere-matrix-browser.png` 与 `H:\CodexLS\outputs\DbSphere-matrix-browser-mobile.png`。此集群没有注册 `v1beta1.metrics.k8s.io` APIService，因此 Pod/Node CPU 和内存资源指标未在本次多节点验收中取得；页面明确显示资源指标缺口。初始 Role 只授予 `list pods.metrics.k8s.io`，浏览器检查时发现 Pod 指标请求缺少 `get`；清单已补齐 `get`，并在集群的 ServiceAccount 规则中核对。该集群未部署 metrics-server，不能把权限修正写成指标采集通过；单节点集群的真实 metrics-server 验收另见[本机 kind 验收记录](KIND_ACCEPTANCE.md)。

本机脱敏结果保存在 `H:\CodexLS\outputs\DbSphere-matrix-acceptance.json`。仓库脚本：[`kind-matrix-manifest.mjs`](../scripts/kind-matrix-manifest.mjs)、[`kind-matrix-secrets.mjs`](../scripts/kind-matrix-secrets.mjs)、[`kind-matrix-acceptance.mjs`](../scripts/kind-matrix-acceptance.mjs)。验收程序可重复运行，已有资产按名称复用，并写入新的真实样本。

## 独立 Linux CI 复验

[GitHub Actions 运行 44313908935](https://github.com/cuipengdba/DbSphere/actions/runs/44313908935) 的 `kind-pg14-18-matrix` 作业在全新 Ubuntu runner 上创建四节点 kind 集群、独立 Namespace 和随机测试凭据；Go 服务与长期 PG18 存储分置 control-plane/worker2，PG14/15 在 worker、PG16–18 在 worker3。五版只读 `pg_monitor` 样本均包含对应 Pod、Service 和 Node；PG18 目标与监控存储 PVC 均为 Bound。作业核对 `dbsphere-reader` 仅可读取所需 Namespace 与 Node/PV 资源，不可读取 Secret、删除 Pod 或读取另一 Namespace 的 Pod。

作业先保存五版原始样本，再分别替换 Go Pod 与 PG18 存储 Pod；重启后复跑完整矩阵，逐版确认资产 ID 不变、新样本 ID 与旧样本 ID 不同，且旧样本和新样本均可从 PG18 API 读回、版本与 K8S 证据仍为 `ok`。例如 PG18 资产 `8c37fc8b-e2a1-4fc4-bd9b-a95f6a49c90f` 在替换前后的样本分别为 `bcc6051a-ee3b-4b5a-b984-ee52494837a3` 和 `af42ec22-275d-4b94-b4bf-75c954a0b2da`。验收程序见[`kind-matrix-verify-persistence.mjs`](../scripts/kind-matrix-verify-persistence.mjs)。此前[首轮独立 CI](https://github.com/cuipengdba/DbSphere/actions/runs/44312873082)仅验证重采样和资产 ID，未逐条读回旧样本；上述第二轮补足了这一点。

独立 CI 没有安装 metrics-server，也没有注入 K8S 故障或执行生产规模与高可用测试。这些结果不能代替下方列出的未完成边界。

## 复现

需要健康的 kind 控制平面、至少三个 worker、`kubectl`、`kind`、Docker、Node.js 及已有的 Go 静态镜像。以下用 PowerShell，`$cluster` 可换成新建的三 worker kind 集群名；脚本默认 `agentsql-v350`。请把临时 kubeconfig 与密钥保存在仓库外。

```powershell
$cluster = 'agentsql-v350'
$kube = 'H:\CodexLS\tmp\dbsphere-matrix-admin.kubeconfig'
$secret = 'H:\CodexLS\tmp\dbsphere-matrix-secret.json'
$credentials = 'H:\CodexLS\outputs\DbSphere-matrix-credentials.json'
$manifest = 'H:\CodexLS\tmp\dbsphere-matrix-manifest.json'

docker build -f Dockerfile-go -t dbsphere/kind-matrix:local .
kind load docker-image dbsphere/kind-matrix:local --name $cluster
node scripts/kind-matrix-manifest.mjs $cluster $manifest
node scripts/kind-matrix-secrets.mjs $secret $credentials
kubectl --kubeconfig $kube create namespace dbsphere-matrix --dry-run=client -o yaml | kubectl --kubeconfig $kube apply -f -
kubectl --kubeconfig $kube apply -f $secret
kubectl --kubeconfig $kube apply -f $manifest
kubectl --kubeconfig $kube -n dbsphere-matrix get pods,pvc -o wide
```

Namespace 必须先于 Secret 与其他对象建立。若使用新集群，先从其 `kind get kubeconfig --name $cluster` 导出管理员 kubeconfig；只交给本地部署和故障注入命令，DbSphere Pod 只使用清单中的 `dbsphere-reader`。等待所有 Pod 就绪，在另一个终端运行：

```powershell
kubectl --kubeconfig $kube -n dbsphere-matrix port-forward svc/dbsphere-go 18091:8080
```

然后运行验收并保存不含密钥的结果：

```powershell
node scripts/kind-matrix-acceptance.mjs http://127.0.0.1:18091 $credentials $kube $cluster > H:\CodexLS\outputs\DbSphere-matrix-acceptance.json
```

若 Node.js 只用于测试，不需要将其安装到 DbSphere Go 容器或正式版运行环境。

## 环境波动与边界

首次尝试新建独立双节点 `dbsphere-v1-matrix` kind 集群时，`kubeadm init` 中 API Server 拒绝连接；kind 自动删除了新节点。随后复用已有四节点集群。该集群的 controller-manager 和 scheduler 在本轮部署前已有数百次重启，API 曾超时。首次 PG14 采样遇到初始化期的 PostgreSQL `statement_timeout`，组件就绪后五版复跑通过。PG18 存储滚动期间，控制面延迟使 `rollout status` 超时，一次采样返回 503；实际新 Pod 从原 PVC 启动，随后完整矩阵再次通过。

这里证明了本机多节点跨节点访问、只读纳管、PG14–18 采集、Kubernetes 拓扑关联，以及存储 Pod 替换后的数据保留。没有证明生产集群可靠性、连续负载、高可用切换、备份恢复、所有 PostgreSQL 部署形态或八类故障全链路。单节点同事件故障闭环的实际证据见[本机 kind 验收记录](KIND_ACCEPTANCE.md)。
