# 协议转换引擎

API Share 在 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 与 Google Gemini 四种格式之间做双向转换。入站与出站格式可以任意组合，例如用 Claude Code 驱动 DeepSeek，或用 OpenAI Codex 驱动普通 Chat Completions 模型。

```
Anthropic SDK / Claude Code (POST /v1/messages)
               │
               ▼
   [ API Share 协议标准化抽象层 ]
               │
               ▼
OpenAI / DeepSeek 上游 (POST /v1/chat/completions)
               │
               ▼ (流式 SSE / 思考过程 / 工具调用)
   [ 逆向保真组装与事件流重塑 ]
               │
               ▼
Anthropic 规范 SSE 增量推送 (message_start -> content_block_delta -> message_stop)
```

## 保真特性

### 1. Codex Responses 自由文本与工具转换

- 适配 Codex 客户端特有的 `custom` 自由文本工具（如 `apply_patch`）、`namespace` 命名空间以及客户端模式的 `tool_search`。
- 跨格式转发时自动将自由文本安全打包为标准函数入参，并在回程流式下发时完整恢复 `custom_tool_call`、原始命名空间、工具搜索结果与对应 `call_id`。

### 2. 多工具调用与历史严格配对

- 并行工具调用历史按实际批次自动配对，严格清理取消、重连后无结果的残缺调用。
- 重复的调用 ID 分组配对隔离，防止上下文混乱。

### 3. Claude Code 复杂系统指令兼容

- 自动识别并合并 Claude Code 在 Anthropic `messages[]` 内部动态插入的纯文本 `role: "system"`，按序聚合到目标接口系统指令，同时避免扰乱会话指纹。

### 4. 全模态视觉图片规范化互转

- 统一转译 OpenAI URL/data URL、Anthropic URL/Base64、Gemini inlineData/fileData。
- 针对 Responses 工具执行返回的结构化截图及数据，自动规范重构为目标模型的图片消息块。

### 5. SSE 流式断流保护与首包回滚

- 上游流式请求默认携带 `Accept-Encoding: identity`，消除代理压缩器导致的推理 SSE 积压延迟。
- 在首次向客户端输出实质性文本、推理思考或工具增量前，若遭遇上游断流或错误，不消耗客户端重试预算，静默无缝切换到备用上游；一旦产生实质输出后遇错则返回标准失败事件，严格避免工具重复执行。
- Messages 流式请求默认每 10 秒向客户端保活，覆盖等待响应头、原生转发和跨协议转换。Claude Code `2.1.193` 及以上版本在已打开的正文、思考或工具块内收到空增量，其他情况发送标准 `ping`。保活不计入输出或用量，也不延长上游空闲超时；只有心跳时仍可切换上游，实际消息开始后禁止重放。HTTP 状态已由心跳提交后，最终错误以 SSE `error` 事件返回，并在调用日志中记录失败状态。
- 转换为 Messages 时，首个工具调用的参数逐片发送；其他交错内容按顺序暂存，直到活动工具关闭后发送。参数不完整时返回失败，不发送成功结束事件。

### 6. 深度思考 / Reasoning 隔离与档位自适应

- 支持 Chat `reasoning_effort`、Responses `reasoning.effort` 与 Claude `output_config.effort` 的双向自适应映射（如 DeepSeek `xhigh -> max`, `medium -> high`）。
- 思维链明文缓存机制：针对返回的推理内容按 `Key + 客户端模型 + 上游 ID + 映射模型` 独立空间缓存（LRU 1024 项 / 8 MiB，TTL 7 天），多轮对话精准回放，杜绝密文伪装与跨租户泄露。

## 结构化输出

Claude 的 `output_config.format`（兼容旧版 `output_format`）会转换为目标协议的对应参数：

- Chat：`response_format.json_schema`
- Responses：`text.format`
- Gemini：`generationConfig.responseJsonSchema`

上游的「结构化输出转换」选项决定具体行为，详见[客户端接入指南](CLIENTS.md#结构化输出转换)。

## 已知边界

- 跨供应商的 thinking 签名与 redacted 数据不能伪造；原生 Gemini 的 `thoughtSignature` 仅在原生请求中保留，跨格式回放尚未实现。
- 长上下文分档、服务端工具按次费用、缓存存储费用、图片 / 音视频分模态价格尚未接入，详见[计费边界](SUB2API_BILLING_AUDIT.md)。
- 更细的协议差异与 sub2api 问题对照见[全格式审计](SUB2API_ALL_FORMATS_AUDIT.md)与[Responses 审计](SUB2API_RESPONSES_AUDIT.md)。
