# 四种协议兼容排查

APISHARE 的兼容能力需要分两层看：原生请求保留供应商字段，跨格式请求转换共同支持的文本、图片和函数工具。支持四个入口不代表各供应商的签名、托管工具、结构化输出和缓存计费可以互换。

本次把此前的 Responses 排查扩展到 Chat Completions、Anthropic Messages、Gemini，并检查 Embeddings 的路由边界。对照版本仍是 sub2api `9c2cca9e627291bc719bada3d57de23b625a4180`；依据包括公开问题、下列转换源码及相关测试定义。不是对全部 GitHub 问题或全部模型版本作无遗漏保证。

两轮扩展共新增 17 个回归测试函数，其中本轮新增 6 个，按用户要求未执行测试或调用真实模型；静态检查 `go vet ./...` 与程序编译通过。“已有处理”指代码层面，运行稳定性仍需后续验证。

- [Responses 的 70 项排查及公开问题索引](/F:/cpa/APISHARE/docs/SUB2API_RESPONSES_AUDIT.md)
- [本次回归定义](/F:/cpa/APISHARE/protocol_formats_audit_test.go)
- [原生三种格式的 SSE 聚合](/F:/cpa/APISHARE/protocol_native_buffer.go)
- [四种原生 JSON→SSE 包装](/F:/cpa/APISHARE/protocol_native_writer.go)、[思考配置](/F:/cpa/APISHARE/protocol_thinking.go)与[用量转换](/F:/cpa/APISHARE/protocol_usage.go)
- [本轮配置与协议回归定义](/F:/cpa/APISHARE/protocol_optimization_test.go)

## 本轮继续优化

原生流式请求遇到 JSON-only 上游时，四种协议都已增加 SSE 包装。Chat 按工具索引和 include_usage 生成块；Messages 顺序生成内容生命周期、签名和引用增量；Responses 保留原输出 ID、custom 原文、加密项及压缩项，生成内容、推理摘要、工具参数和终止事件；Gemini 发送原生完整响应帧。包装前验证终态、已识别工具参数和事件大小，失败时不提交响应头。上游已经一次性返回 JSON，这种包装不会缩短其首字延迟。

思考配置单独保存模式和固定预算。显式 disabled 按 sub2api 的优先级覆盖 effort；Gemini thinkingLevel 可以转为 effort，符合型号范围的固定预算可在 Messages/Gemini 间保留。预算不会被变成档位，也不自动增大用户输出上限。没有显式 effort 的 adaptive 转入仅有 effort 的协议、不能关闭思考的型号、未知型号预算及显示策略等仍明确拒绝。[sub2api 优先级实现](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/apicompat/anthropic_to_responses.go)、[Claude 模式定义](https://platform.claude.com/docs/en/build-with-claude/thinking)、[Gemini 型号范围](https://ai.google.dev/gemini-api/docs/generate-content/thinking)

用量转换开始保留缓存读取、缓存创建、推理和 Gemini 工具提示 Token，按源协议换算总数。OpenAI 已含推理的输出不再次加推理；Gemini 候选输出之外的 thoughts 单独计入；Gemini 输入已含缓存，转为 Messages 时拆分为普通输入与缓存读取，不伪造缓存创建。累计分项覆盖，缺失分项保留，显式 0 覆盖旧值。流式与非流式使用同一转换器；缓存创建在 OpenAI details 中使用兼容网关扩展字段 cache_creation_tokens，不能当成官方 OpenAI 缓存写入计费定义。

内部扣费仍使用原来的供应商基本计数和 In/Out 单价，尚未增加缓存、思考或工具提示的细分价格；因此客户端的标准化 usage 与当前扣费日志的基本计数可能不同。这一轮不宣称已经修好计费分项。

## 本次修正

| 格式 | 问题与处理 |
|---|---|
| Chat / Messages / Gemini | 非流式请求遇到 SSE 上游，先聚合有效终止事件，再以 JSON 返回。保留已识别的原生内容、身份和用量附加字段，不经共同表示丢弃供应商状态。Chat 读取工具结束后的 usage；Messages 累积 thinking.signature 和 citations，工具参数增量替换初始 input；Gemini 保留原生 parts 及其签名、图片和 grounding。 |
| Chat / Messages / Gemini | 跨格式历史统一按批次配对调用与结果。乱序结果放回对应调用后；同批重复结果取最后一份，后续轮次复用 ID 不串批。取消留下的无回复调用及孤立回复按已有 Responses 策略清理，原生请求不执行该清理。 |
| Chat / Messages / Gemini | 跨格式工具声明检查重名及强制选择的声明归属。未知工具或无工具却要求必调工具明确失败；auto/none 在无工具时不生成多余 tool_choice。 |
| Messages / Gemini | 重新解析工具参数以生成对象时继续使用 JSON Number，避免大整数 ID 在请求、非流式响应或 Gemini 流式最终工具项中舍入；Gemini 工具结果对象同样保留精度。Responses 客户端工具搜索参数的对象化路径也补齐。 |
| Gemini | 修复 generationConfig.topP 因 JSON Number 而丢失；保留 stopSequences。多候选返回按 index 0 选择，避免把数组首位的其他候选当作主回答。 |
| Chat / Gemini | 接受普通文本声明 response_format.type=text 与 responseMimeType=text/plain；JSON Schema 等仍需要原生支持，不能把普通文本设置误判成结构化输出。 |
| Gemini | 多候选请求、cachedContent、continuationToken、safetySettings，以及 topK、responseModalities、mediaResolution 在转换路径明确拒绝，不再静默丢弃。thinkingConfig 已按模式和已知型号能力部分转换，includeThoughts 等显示配置仍无等价映射。原生请求仍保留。 |
| Gemini | promptFeedback.blockReason 的无候选响应在转换路径明确提示阻断；原生响应保留反馈，并识别为合法的流式终态。TOO_MANY_TOOL_CALLS 与此前的非法工具生成原因均不再冒充普通结束。 |
| 四种原生对话 | 非流式 HTTP 200 中明确的 error、failed 状态或 Gemini 非法工具生成，写出内容前判为失败，可依现有调度策略重试；不会仅因 HTTP 200 记录成功。 |

非流式 SSE 聚合有 64 MiB 总量限制；未结束的候选、未关闭的 Messages 内容块、非法工具 JSON 会失败。未知 Messages 增量类型明确报错；不能据此宣称所有未来扩展事件已经支持。原生流式透传仍保留上游 SSE 字节。

## Chat Completions

| 场景 | 结论 |
|---|---|
| content 为字符串或文本/图片数组 | 已有处理，不能用只接收 string 的解析器。[sub2api 问题 #1189](https://github.com/Wei-Shaw/sub2api/issues/1189) |
| reasoning_content / reasoning / 明文 reasoning_details | 已有别名处理，优先一份明文，避免重复输出；密文 details 不作为正文或明文推理。 |
| 多轮 assistant.tool_calls → tool.tool_call_id | 本次扩大批次处理；保留原 ID，不做无法对应的单侧重命名。[相关 Messages 转换问题 #2337](https://github.com/Wei-Shaw/sub2api/issues/2337) |
| 并行工具、迟发名称、参数分片 | 已有转换处理，最终验证 JSON；原生聚合按 index 排序，不因到达顺序乱配。 |
| include_usage、空 choices 的末尾 usage、真实 0 Token | 已有转换处理，本次补齐原生聚合。累计用量按字段存在更新，不把 0 当未知。 |
| 非流式 JSON 与 Content-Type | 聚合后明确返回 application/json。[sub2api 问题 #1311](https://github.com/Wei-Shaw/sub2api/issues/1311) |
| functions / function_call / role=function 旧接口 | 原生可透传；跨格式明确不支持。sub2api 会为旧历史生成稳定调用 ID，但其孤立结果策略不直接照搬。[参考测试](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/apicompat/chatcompletions_to_responses_legacy_function_call_test.go) |
| response_format JSON、音频、logprobs、预测输出、多个 choices | 不在共同能力内。JSON/音频/n>1 已明确拒绝；logprobs、prediction 等参数的跨协议映射尚未实现，不能保证语义保留。 |
| refusal 字段 | 能读拒绝内容及停止原因；共同表示将拒绝作为文本，尚不完整保留各协议的专用 refusal 结构。 |
| GPT 推理型号的参数差异 | 已按型号使用 max_completion_tokens；temperature/top_p 的模型级能力校验尚未补齐。sub2api 的特定型号移除策略不能当作所有兼容供应商的规则。 |
| DeepSeek / GLM / Grok 等 | 使用 Chat 或供应商提供的 Messages 兼容入口，并非另有一种统一格式。协议能转换不保证模型会选择 apply_patch，或各型号支持同一套参数。 |

对照了 sub2api 的直接 Messages↔Chat 桥接，而不是只看经 Responses 中转的路径。APISHARE 的共同表示本身只转换一次，已有相同的架构收益。[直接桥接源码](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/apicompat/chatcompletions_anthropic_bridge.go)

## Anthropic Messages

| 场景 | 结论 |
|---|---|
| system 字符串/文本块、相邻同角色消息 | 已有处理；转换时合并系统文本和相邻消息。中途系统指令、cache_control 的原生语义不能完整转换。 |
| tool_use / tool_result / is_error | 已有 ID 配对和错误文本提示；本次扩大批次清理范围。跨入 Chat 时结果中的图片放到整批回复之后的 user 图片消息。 |
| content_block_start/delta/stop 生命周期 | 已有转换处理；向 Messages 发送并行工具时逐个发完整块，避免幽灵索引和交错参数。[sub2api 问题 #4193](https://github.com/Wei-Shaw/sub2api/issues/4193) |
| 初始 input={} 后收到 partial_json | 已有转换修正，本次原生聚合同样采用“增量替换种子”，避免 `{}{...}`。[sub2api 问题 #6336](https://github.com/Wei-Shaw/sub2api/issues/6336) |
| message_start 与 message_delta 用量 | 保留字段并按累计值覆盖；不累加每次累计的 output_tokens。本次原生聚合保留缓存分项。 |
| thinking / signature_delta / redacted_thinking | 原生保留。明文 thinking 可以回放到 Chat 的推理字段；跨供应商签名与 redacted 数据不能伪造。[Claude 流协议](https://platform.claude.com/docs/en/build-with-claude/streaming) |
| thinking 的 enabled/adaptive/budget_tokens | 本轮保留独立模式/预算；已知预算型号可转 Gemini 预算，adaptive + 明确 effort 可以转入 Chat/Responses。disabled 覆盖 effort；显示策略与不能等价映射的组合明确拒绝。 |
| tool_choice 与 disable_parallel_tool_use | 已有模式转换，本次补声明检查；向 Gemini 禁止并行没有等价保证，明确拒绝。 |
| max_tokens / budget_tokens | 不自动注入未经模型验证的思考预算；需保持输出预算与思考预算一致。不能无条件复制 sub2api 的 Antigravity 特定预算补偿。 |
| context_management、缓存控制、托管工具、文档/搜索结果块 | 原生保留；跨格式没有完整支持。Claude Code 禁用实验 beta 是现有绕过方式，context_management 尚未实现等价转换。 |
| pause_turn / 新的上下文终止原因 | 原生保留；跨格式 pause_turn 明确失败，不冒充正常完成；model_context_window_exceeded 归为截断。其他未知停止原因仍需要扩展枚举。 |
| 引用与严格结构化输出 | 本次原生聚合保留 citations_delta；跨格式引用结构和工具 strict 的目标能力约束尚未完整适配。 |

sub2api 曾出现签名重试时移除 thinking 却遗留 thinking_strategy/context_management 的二次 400；这说明降级必须检查整套关联配置。APISHARE 当前没有这种自动签名降级，不应直接加一个“删签名重试”补丁。[问题 #599](https://github.com/Wei-Shaw/sub2api/issues/599)、[问题 #851](https://github.com/Wei-Shaw/sub2api/issues/851)

## Gemini

| 场景 | 结论 |
|---|---|
| generateContent / streamGenerateContent | 已支持；流式采用 alt=sse。countTokens、缓存管理、批处理等其他方法尚无路由，不能宣称完整 Gemini CLI 能力。 |
| contents/parts、systemInstruction、图片 | 已有文本及图片输入转换。远程图片跨入 Gemini 需 MIME 或 Base64；fileData 中的文件/音视频不等于图片。 |
| functionDeclarations 的 Schema | 入站转换传统 Schema 的大小写类型；出站使用 parametersJsonSchema，保留引用和联合约束，不无条件削平 Schema。 |
| functionCall / functionResponse 的 ID 与名称 | 已有配对；无 ID 的结果在历史中查找尚未配对的同名调用。本次统一批次归属，保留对象数值精度。 |
| ANY/AUTO/NONE 与 allowedFunctionNames | 已有单工具选择转换；多个允许名称尚不支持跨格式，不静默扩大允许范围。新模式也需要目标能力检查。 |
| topP / stopSequences / candidateCount | 本次修复 topP；跨格式 n>1 明确拒绝，原生聚合保留多个完整候选。 |
| thoughtSignature 与多轮工具 | 原生请求及本次非流式聚合保留签名附着的完整 parts。跨格式的供应商签名缓存/回放尚未实现，不能宣称 Gemini 新型号多轮工具已稳定。[签名要求](https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures) |
| 签名失效的 dummy 替代值 | sub2api 有可配置清洗重试；不直接移植，替换签名与保持原推理状态是两回事。[签名清洗测试](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/gemini_native_signature_cleaner_test.go) |
| 无候选 promptFeedback、安全停止、非法函数 | 本次区分主候选与提示阻断；非法函数生成判失败，安全终止保留拒绝/过滤语义。[问题 #7081](https://github.com/Wei-Shaw/sub2api/issues/7081)、[官方响应定义](https://ai.google.dev/api/generate-content?hl=en) |
| inlineData 图片输出、音频/视频、代码执行、grounding | 原生保留；跨格式输出仍明确拒绝不支持的非文本块。sub2api 有将图片输出转成 Markdown 的方案，APISHARE 尚未实现，不能把 Base64 文本视作等价图片支持。[参考实现测试](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/gemini_chat_completions_compat_service_test.go) |
| SSE 注释与 SDK | 转换路径不自行插入心跳。sub2api 发现部分 Google SDK 拒绝注释；原生上游已有注释的适配仍未实现，透传不等于所有客户端都能消费。[SDK 兼容处理](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/gemini_sse_comment_compat.go) |
| 预算、缓存、安全/模态控制 | 已补已知型号的思考预算/档位映射；不能等价映射的组合拒绝。结构化 JSON、缓存名称和安全/模态控制等仍需原生使用。 |

## 共用边界与后续顺序

1. **下一步应改细分计费。** 协议响应已经保留并换算主要缓存/思考分项，内部计费仍只使用基本输入/输出单价。缓存价格、思考与工具提示的价格归属、日志和额度扣费要一起处理，避免把缓存全部按普通输入收费，或把推理与输出重复收费。[Gemini 定义](https://ai.google.dev/api/generate-content?hl=en)、[sub2api 分项约束](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/antigravity/usage_contract_test.go)
2. **供应商状态需独立适配。** Gemini 签名、Anthropic 签名/暂停、Responses 密文和压缩项的回放需区分上游、账号、模型和对话。现有 Responses 明文缓存不能替代这些状态。会话粘性有帮助，故障转移仍可能使旧状态失效。
3. **继续完善原生特殊输出事件。** JSON→SSE 已包装四种协议的主要生命周期，特殊托管工具和加密/压缩项完整保留于输出项及最终快照；不是每种新托管工具的过程事件都能从最终 JSON 重建。
4. **尚未声明的扩展字段需要能力表。** 主要无映射配置已拒绝，但 Chat logprobs/prediction、思考显示策略等仍可能没有等价转换。后续应按源协议、目标协议、目标模型逐项允许/拒绝，而不是笼统删字段。
5. **辅助接口按各自协议扩展。** Embeddings 仅在 OpenAI 类型上游原生转发，不转换向量、维度、批次顺序或 Base64 编码；模型列表不证明模型可调用。Gemini countTokens、缓存接口，以及图像/音频/Batch/Realtime 不在现有转发路由内。

已开始输出的流发生失败仍停止，不换上游重放；这对带工具执行的客户端尤其重要。sub2api 的等待心跳曾把 Messages ping 插入 OpenAI 流，本项目没有新增这种事件。[问题 #7946](https://github.com/Wei-Shaw/sub2api/issues/7946)

本轮改动保留在工作区；未部署或替换运行中的程序。测试定义覆盖聚合、签名保留、批次、大整数、用量、阻断和 HTTP 200 错误，但尚未运行，不能据静态检查承诺所有型号稳定。
