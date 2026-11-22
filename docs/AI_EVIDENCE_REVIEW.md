# 事件证据助手

在 **根因与证据 → 证据助手** 查看规则核查。接入模型后，管理员和值班员可在当前事件请求审核。候选解释、反证与核查建议逐条引用证据。模型缺少核查项时，系统沿用已验证的引用补充规则核查，并标明来源。点击引用可定位事件时间线，带采样标识的记录可打开原始采样。获准查看事件的账号可以查看该事件的审核结果。

模型输入最多 18 条记录和 8,000 字节，包含证据 ID、类型、时间以及允许传输的数值字段。资产名称、事件标题、连接信息、SQL 文本、外部工单内容和人工备注在此请求中省略。组织每日模型请求额度沿用现有 AI 预算，审计记录输入字节数、证据数量、模型、引用和结论。模型输出中每条引用必须来自实际送审的证据，并在保存前核对其事件归属。

未接模型、模型不可用、引用验证失败或预算用完时，可查看同一事件的规则核查。证据不足在调用模型前反馈。证据助手沿用现有只读诊断权限，受控作业继续使用申请、审批和执行流程。

使用已有 `DBSPHERE_AI_CHAT_URL`、`DBSPHERE_AI_MODEL` 和可选 `DBSPHERE_AI_API_KEY`。模型服务支持 JSON Schema 时可设 `DBSPHERE_AI_EVIDENCE_SCHEMA=true`。审核超时默认 28 秒，可用 `DBSPHERE_AI_EVIDENCE_TIMEOUT_SECONDS` 设置为 1–120 秒。LLM 可运行在原生部署的回环地址，或在容器中与 DbSphere 共享网络命名空间。可在离线环境提前准备推理镜像和模型权重。

[llama.cpp Chat Completions 接口](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md#post-v1chatcompletions-openai-compatible-chat-completions-api)
