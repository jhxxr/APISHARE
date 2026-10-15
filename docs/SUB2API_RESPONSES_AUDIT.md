# Codex Responses 兼容排查

APISHARE 的常规 Codex 工具调用可以通过 Responses 转入 Chat、Anthropic 或 Gemini 的函数接口。自由文本工具、命名空间、工具发现和多轮历史需要双向转换；仅能接收 `/responses` 或返回 HTTP 200，不能证明一个上游具备这些能力。只支持 Chat 的供应商应明确配置为 OpenAI Chat Completions。

本次集中补齐了完成事件的内容恢复、并行工具流的收尾、推理别名和历史、委派任务输入、参数 Schema、原生 Responses 的 SSE 聚合与故障识别，并增加原生 `/v1/responses/compact` 转发。强制签名、加密状态、服务端工具和缓存细分仍有独立的能力边界。

## 排查范围

截至 2026-10-14，对照 sub2api 提交 `9c2cca9e627291bc719bada3d57de23b625a4180`，索引了 639 条包含 Responses 的公开 Issue，并检索其 apicompat 包中 385 个 Responses 测试定义。标题索引用于定位问题；表内处理结论来自相关源码、测试定义和当前 APISHARE 代码。检索数量不等于受影响数量，也不表示逐条运行了上游测试。

本轮新增 18 个回归测试函数，连同前两轮共新增 33 个。静态检查与编译通过，按用户要求未执行这些测试、真实模型调用或新的 Codex 端到端测试。下表的“代码覆盖”表示当前实现已有对应处理，尚不等于运行验证。

- [公开问题索引](sub2api-issue-index.json)
- [sub2api 固定版本的协议转换器](https://github.com/Wei-Shaw/sub2api/tree/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/apicompat)
- [集中回归定义](../cmd/apishare/protocol_audit_test.go)
- [前两轮流式回归定义](../cmd/apishare/responses_stream_compat_test.go)和[图片回归定义](../cmd/apishare/responses_media_test.go)

## 工具定义和参数

| 编号 | 场景 | 当前处理与适用范围 |
|---|---|---|
| 01 | apply_patch 和 exec 等 custom 工具 | 代码覆盖。转为带 input 字符串的函数，回程恢复 custom_tool_call；普通函数不误改成 custom。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5659) |
| 02 | namespace 中包含 custom 子工具 | 代码覆盖。保留子工具类型、原名和命名空间，避免 exec 变成客户端找不到的普通函数。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7241) |
| 03 | 长工具名和非法字符 | 代码覆盖。顶层和命名空间工具均使用合法短名及哈希映射，回程恢复声明身份；转换后的重名明确报错。[实现](../cmd/apishare/responses_tools.go) |
| 04 | additional_tools 里才有工具 | 代码覆盖。合并到有效工具列表，不能只读取顶层 tools。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6653) |
| 05 | tool_search 发现新的 App 或 MCP 工具 | 代码覆盖。将发现的 tools 提升为下一轮可调用定义，并恢复调用的 namespace。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6584) |
| 06 | tool_search_output 没有 output 字段 | 代码覆盖。保留 tools 发现结果供模型阅读。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5867) |
| 07 | 指定工具和 allowed_tools 子集 | 代码覆盖。检查声明归属并转换选择；空子集、未声明工具和歧义明确报错。[实现](../cmd/apishare/responses_tools.go) |
| 08 | 模型遗漏或误加 custom 的命名空间 | 代码覆盖。仅修复唯一且无歧义的 custom 别名；明确声明的同名普通函数优先。[实现](../cmd/apishare/responses_tools.go) |
| 09 | 参数根级 oneOf 或 type 缺失、为 null | 代码覆盖。补根对象约束，保留 oneOf、引用、定义和分支约束；不把联合类型削成空 Schema。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5820) |
| 10 | strict 的默认值在协议间不同 | 代码覆盖。显式 true/false 保留；转换到 Responses 时未声明 strict 的函数使用 false，避免自动强化原本非严格的 Schema。[协议说明](https://developers.openai.com/api/docs/guides/function-calling) |
| 11 | Gemini Schema 与 JSON Schema 不同 | 代码覆盖。入站规范化 Schema 类型，出站使用 parametersJsonSchema；不随意删除 defs 或 ref。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/3143) |
| 12 | 自由文本 grammar 和工具选择概率 | 有能力边界。转换后 grammar 是说明提示，无法保证供应商强制执行语法，或保证模型一定选择 apply_patch；模型可以选择 shell。[实现](../cmd/apishare/responses_tools.go) |

## 多轮历史和任务输入

| 编号 | 场景 | 当前处理与适用范围 |
|---|---|---|
| 13 | 一个 assistant 批次有多个调用 | 代码覆盖。合并调用、按调用顺序紧接完整结果，避免中间插入普通消息破坏供应商校验。[实现](../cmd/apishare/responses_history.go) |
| 14 | 结果乱序、重复或孤立 | 代码覆盖。按所属批次配对；同一批次结果采用最后一份，孤立结果不转为伪造调用。[实现](../cmd/apishare/responses_history.go) |
| 15 | 后续批次复用 call_id | 代码覆盖。按批次和 ID 双重配对，不把后续结果放到前一批；同一批重复调用 ID 明确报错。[实现](../cmd/apishare/responses_history.go) |
| 16 | 取消或重连留下未回答的调用 | 代码覆盖。仅转换路径清理未配对的调用，保留已有普通内容；原生历史保持完整。[相关线索](https://github.com/Wei-Shaw/sub2api/issues/5850) |
| 17 | 历史函数参数是对象或 JSON 字符串 | 代码覆盖。两者均可转换；截断的历史函数及其结果不作为完整调用回放。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2913) |
| 18 | 工具参数带大整数 ID | 代码覆盖。使用 JSON Number 保留原数值，避免对象参数经 float64 后改变 ID；普通采样数字另行读取。[实现](../cmd/apishare/protocol_compat.go) |
| 19 | 工具结果是文本内容数组 | 代码覆盖。保留多段文本，不能只读取 string output。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2913) |
| 20 | Codex 委派 bootstrap 没有 call_id | 代码覆盖。仅对 codex_app/codex_tui、create_thread/send_message_to_thread 和合法 codex_delegation 信封转为 user 输入；普通无 ID 结果仍拒绝。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6402) |
| 21 | agent_message 包含明文任务 | 代码覆盖。保留可读内容为 user 输入；真正的 encrypted_content 不按明文拼接，需原生支持。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6887)、[加密语义](https://developers.openai.com/api/docs/guides/responses-multi-agent) |
| 22 | 中途插入 system 或 developer | 有能力边界。转换时合并系统指令，不能完整保留原生生效位置；会话指纹保留初始上下文。原生保持原位置。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7400) |

## 图片和其他内容

| 编号 | 场景 | 当前处理与适用范围 |
|---|---|---|
| 23 | view_image 图片变成 Base64 文本 | 代码覆盖。将图片放到完整工具结果批次后的 user 图片消息，原结果保留文本和位置提示，不把图片当普通文本计入上下文。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/4956) |
| 24 | 图片结果穿插普通消息或复用调用 ID | 代码覆盖。在历史配对后处理，按 batch/call_id 归属；孤立结果不携带图片。[实现](../cmd/apishare/responses_media.go) |
| 25 | 图片被包在 JSON 字符串或整段 data URL 中 | 代码覆盖。仅提取已识别的图片内容节点；无图片的普通 JSON 文本保持原样，不猜测任意 URL 字段。[实现](../cmd/apishare/responses_media.go) |
| 26 | 原生 custom 工具返回图片数组 | 原生支持。完整请求原样透传，不将数组字符串化；模型映射和已声明的思考强度适配按原规则处理。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6240) |
| 27 | Gemini 远程图片缺少 MIME | 明确拒绝该转换。可以使用带 MIME 的 base64 图片；不会猜测 MIME 或下载远程媒体。[实现](../cmd/apishare/protocol_request.go) |
| 28 | PDF、音频、视频和供应商文件 ID | 明确拒绝不支持的跨格式转换，原生接口按上游能力处理；不能静默丢附件。[相关线索](https://github.com/Wei-Shaw/sub2api/issues/5485) |

## 流式事件和结束语义

| 编号 | 场景 | 当前处理与适用范围 |
|---|---|---|
| 29 | 有文本 delta 但缺内容项生命周期 | 代码覆盖。输出 item、content part、delta、done 和终止事件，包含文本 annotations。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2875)、[annotations 线索](https://github.com/Wei-Shaw/sub2api/issues/5140) |
| 30 | 文本或推理只在 done 里出现 | 代码覆盖。读取文本、推理、refusal、content part 和 output item 的完成内容。[上游回归定义](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9/backend/internal/pkg/apicompat/responses_to_anthropic_text_recovery_test.go) |
| 31 | delta 只给前缀，done 补齐正文或参数 | 代码覆盖。补发尚未发送的后缀，重复完整快照不重复追加；与已发送内容矛盾的完成值判失败。[实现](../cmd/apishare/protocol_stream.go) |
| 32 | 最终 output 为空但前面已流出内容 | 代码覆盖。保留累计内容，不能用空终止快照覆盖它。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/1493) |
| 33 | response.done 别名或顶层 usage | 代码覆盖。兼容完成别名、顶层用量和 incomplete 状态，避免因等待另一种终止名称而报断流。[上游回归定义](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9/backend/internal/pkg/apicompat/anthropic_responses_test.go) |
| 34 | 工具输出项 ID 与 call_id 混用 | 代码覆盖。按类型生成 msg/rs/fc/ctc/tsc 输出 ID，事件和最终快照引用一致，call_id 保持配对身份。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6146) |
| 35 | 空 role 或空 content 起始 chunk | 代码覆盖。转换路径不因空前导事件提交 SSE；真实文本、思考和可下发工具内容才启动输出。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7222) |
| 36 | 工具名分片、迟发、参数增量没有 name | 代码覆盖。延迟宣布缺名工具，保留原名；参数增量不重新发送空 name。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5631) |
| 37 | Anthropic 初始 input 与后续 delta 重复 | 代码覆盖。初始非空 input 作为 seed，出现实际参数 delta 时采用 delta，避免生成两个 JSON 对象拼接的参数。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/6336) |
| 38 | 并行工具产生错误块索引或交错生命周期 | 代码覆盖。向 Anthropic 输出时首个工具的参数逐片发送，其他交错工具、正文和思考暂存后顺序发送；全部参数校验通过才关闭工具并成功结束，错误时不重放。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/4193)、[事件契约](https://platform.claude.com/docs/en/build-with-claude/streaming) |
| 39 | Read 工具参数等待特定 done 事件 | 代码覆盖。没有按工具名字设置特殊过滤；首个 Anthropic 工具参数即时转发，不依赖 Read 特例。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/4114) |
| 40 | 新生成的参数 JSON 非法或自由文本输入截断 | 代码覆盖。校验后才完成工具项；发生错误不下发可执行的 custom 完成项，不补成功结束。[实现](../cmd/apishare/protocol_stream.go) |
| 41 | max_tokens 耗尽或内容拒绝 | 代码覆盖。保留 length/content_filter/incomplete；不把推理文字伪装成最终回答，不把额度耗尽一律判为网关故障。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/4141) |
| 42 | Gemini 明示生成了非法调用或缺签名 | 代码覆盖。MALFORMED_FUNCTION_CALL、UNEXPECTED_TOOL_CALL、MISSING_THOUGHT_SIGNATURE 等明示失败不会被映射成成功 STOP。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7081)、[官方状态定义](https://ai.google.dev/api/generate-content?hl=en) |
| 43 | SSE 压缩积压推理事件 | 代码覆盖。流式请求默认 identity 编码，可由上游自定义请求头覆盖；网关静态资源 gzip 不作用于 SSE。[报告更正](https://github.com/Wei-Shaw/sub2api/issues/3603) |
| 44 | 上游返回 JSON，客户端要求 SSE | 代码覆盖。将完整结果包装成目标客户端事件流。[实现](../cmd/apishare/proxy.go) |
| 45 | 客户端非流式，上游仍返回 SSE | 代码覆盖。转换路径聚合后再提交 JSON；原生 Responses 也聚合，并保留原生 ID、加密字段、hosted 项和 compaction 项。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7978) |
| 46 | 上游只给前导事件后断流或报错 | 代码覆盖。原生与转换路径均保留未输出实际消息时的有界重试；Messages 控制心跳不锁定上游，工具流一旦开始下发则禁止重放。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5800) |
| 47 | 流已开始后断开，或原生流内 response.failed | 代码覆盖。停止透明重放，记录失败并计入健康状态；原生 SSE 保持字节内容，不把缺终止事件算成成功。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/3043)、[健康线索](https://github.com/Wei-Shaw/sub2api/issues/3084) |
| 48 | 大响应或大量小帧绕过限制 | 代码覆盖。请求入口为 32 MiB，JSON 响应和非流式 SSE 聚合上限为 64 MiB，SSE 单帧为 32 MiB；累计转换参数和 Responses 输出也有边界。原生已提交的长流不是全响应缓冲。[实现](../cmd/apishare/protocol_stream.go) |

## 推理和路由

| 编号 | 场景 | 当前处理与适用范围 |
|---|---|---|
| 49 | reasoning_content、reasoning、reasoning_details 别名 | 代码覆盖。选择一个可读表示，不重复拼接同一推理，也不把加密 details 当正文。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5302) |
| 50 | Anthropic thinking 回放到 Chat 时丢失 | 代码覆盖。明文作为 reasoning_content 传入，不复制供应商签名和 redacted_thinking。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5528) |
| 51 | Responses summary 为空但 content 有明文 | 代码覆盖。summary 优先、content 兜底；连续 reasoning 项保持顺序，各自保存 item 身份。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5520) |
| 52 | 不透明 reasoning 历史的明文恢复 | 代码覆盖。按 API Key、客户端模型、上游与映射模型隔离；每个 ID 只缓存自己的明文，混合明文与不透明项逐项恢复。[实现](../cmd/apishare/responses_reasoning.go) |
| 53 | 缓存未命中、淘汰、重启或新 user 轮次 | 有能力边界。同一用户轮次可回用已存在推理，不归到未知 ID 下；缓存续期 7 天、最多 1024 项/8 MiB，重启清空；不能解密或保证恢复所有供应商状态。[实现](../cmd/apishare/responses_reasoning.go) |
| 54 | 重试复用上一上游的模型或 effort | 代码覆盖。每次从原始请求和目标模型重新映射，不复用前一个候选的转换结果。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2897) |
| 55 | 本地转换不兼容阻断原生候选 | 代码覆盖。跳过不兼容候选，本地失败不占网络重试预算，也不计费。[实现](../cmd/apishare/proxy.go) |
| 56 | endpoint 自动探测导致协议反复切换 | 代码覆盖。由上游配置固定协议；不根据一次 HTTP 200 自动判断工具能力，也不静默改用户模型名。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5371) |
| 57 | 真实 usage 为 0 或 Anthropic 开始事件已有输出用量 | 代码覆盖。按字段是否存在识别真实 0；保留已观测的开始用量，后续累计值覆盖而非相加。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7873) |
| 58 | 独立 Responses compact 端点没有路由 | 原生支持。新增 /v1/responses/compact；仅选原生 Responses/旧配置的对应端点，排除 Chat-only 粘性候选；原样保留整个压缩结果窗口。[官方契约](https://developers.openai.com/api/docs/guides/compaction) |

## 需要独立能力的场景

| 编号 | 场景 | 当前边界与后续方案 |
|---|---|---|
| 59 | Chat-only 上游的远程压缩和 compaction_trigger | 明确不模拟供应商加密压缩。需要原生 Responses 上游，或另行实现并验证本地长会话摘要策略；不能只改 endpoint 或制造 compaction 项。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5648) |
| 60 | 加密 agent_message、reasoning、previous_response_id 或 conversation | 原生保留；跨格式明确拒绝不能表示的状态。恢复只能依赖兼容提供商和原始完整历史，不能将密文当明文或把不同账号状态任意混用。[相关线索](https://github.com/Wei-Shaw/sub2api/issues/7506) |
| 61 | 强制 thoughtSignature 的 Gemini 工具多轮 | 待扩展签名回放缓存。原生 Gemini 保留签名；Responses 转 Gemini 不能承诺此类模型闭环，需要按 Key/上游/模型/调用绑定保存和恢复原始签名。[官方要求](https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures) |
| 62 | web_search、file_search、image_generation 和服务端 tool_search | 原生按供应商能力支持；转换路径明确拒绝。需要独立服务端工具执行桥与真实结果事件，不能只注入函数名。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5155) |
| 63 | JSON Schema 结构化回答、文件检索、音视频结果 | 当前不做通用跨供应商等价转换，保留原生能力。结构化回答应专门适配和验证，不能静默删 response_format/text.format。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/5518) |
| 64 | prompt cache 控制、细分 token 与分档计费 | 主要缓存读写、推理和工具提示分桶已接入协议、计价、日志和额度，支持明确的 1 小时缓存创建；保留价格快照并原子扣费。缓存控制语义、长上下文分档及服务端工具等费用仍待扩展。[计费更新](SUB2API_BILLING_AUDIT.md)、[口径说明](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) |
| 65 | 空 completion 被当成静默拒绝 | 待定义检测策略。只输出推理、输出额度耗尽、真实 0 用量均可能有合法结束语义；不能把所有空回答统一切换上游。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2556) |
| 66 | OAuth、WebSocket、订阅额度、指纹和自动续接 | 不适用当前 API Key/HTTP 架构。若以后增加这些入口，需要单独实现续接锚点、预热、断线恢复和状态隔离，不能直接移植订阅账号逻辑。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/2098) |
| 67 | Azure 旧版 deployment/api-version 和专有接口 | 需要专有 URL/鉴权适配；当前标准 OpenAI URL 与自定义请求头不能代表所有 Azure 接口版本都已支持。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/1292) |
| 68 | Codex 的模型目录、工具偏好和上下文窗口 | 客户端模型目录需要与实际供应商匹配；标准 /models 列表不能保证 Codex 的代码模式、搜索或最大窗口配置正确。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7042) |
| 69 | 模型在列表里但返回没有可用上游 | 上游可用性问题，需要提供商能力和额度信息；格式转换不能保证模型资源存在，也不能凭模型列表宣称工具成功。[问题线索](https://github.com/Wei-Shaw/sub2api/issues/7285) |
| 70 | 单 Key 并发限制和 429 | 配额与调度问题。网关不能提高供应商真实并发上限；排队、退避和 Retry-After 传播应独立设计，避免客户端与网关同时多次重放。[相关线索](https://github.com/Wei-Shaw/sub2api/issues/6530) |

## 接入和后续验证

DeepSeek 等 Chat-only 上游应使用显式 Chat 配置，Codex 保持 Responses，服务端搜索按上游能力配置。当前代码支持普通函数、custom、namespace 和客户端工具发现的转换；函数参数由 JSON 包装，自由文本原文在完整校验后还原。Claude Code 的 context_management 等实验字段仍不能通用转换，原生 Anthropic 可保留，现有关闭实验 beta 的配置仍适用。

原生压缩只负责路由和完整传递。压缩结果中的保留消息、工具、reasoning 和 compaction 应作为完整窗口继续发送，不能只截取一个 compaction item。带加密状态的会话需要保持提供商兼容性；普通 Chat 转换没有解密或迁移这类状态的能力。

恢复运行验证时，应优先执行本报告的 33 个回归函数、后续全格式回归及原有协议矩阵，再核对 Codex 的长任务：多文件读写、并行工具、补丁、失败后修正、取消后重发、图片工具结果和压缩后继续对话。真实模型是否正确调用 apply_patch、多轮推理质量和上游额度限制需要单独观察；静态检查不能替代这些结果。
