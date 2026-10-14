# API Share

API Share 是一个轻量的 API 分发网关，统一接入 OpenAI Chat、OpenAI Responses、Anthropic Claude、Gemini 及 OpenAI 兼容服务。客户端通过网关签发的 API Key 调用模型，由网关完成鉴权、计费、限流、上游调度和故障转移。

网关支持会话粘性，将同一对话稳定路由到同一上游，以提高 prompt cache 命中率；支持多上游按权重分流，以及不同接口格式之间的转换，例如用 OpenAI SDK 调用 Claude 模型、用 Claude Code 调用 Gemini 模型。

> 定位说明：本项目只对接**官方 API 上游**（OpenAI / Anthropic / Gemini / xAI Grok / DeepSeek / GLM 等任何 OpenAI 兼容端点），不包含任何"网页订阅转 API"、TLS 指纹伪装或代理轮换功能。

## 支持的格式

### 客户端接入（入站）

| 格式 | 端点 | 认证头 | 典型客户端 |
|---|---|---|---|
| OpenAI Chat Completions | `POST /v1/chat/completions` | `Authorization: Bearer sk-xxx` | OpenAI SDK、ChatBox、LobeChat |
| OpenAI Responses | `POST /v1/responses` | 同上 | 新版 OpenAI SDK / Codex 类工具 |
| OpenAI Embeddings | `POST /v1/embeddings` | 同上 | 各类 RAG 客户端 |
| Anthropic Messages | `POST /v1/messages` | `Authorization` 或 `x-api-key` | Claude Code、Anthropic SDK |
| Gemini | `POST /v1beta/models/{model}:generateContent` 与 `:streamGenerateContent` | `x-goog-api-key` 或 `Authorization` 或 `?key=` | Gemini CLI、google-genai SDK |
| 模型列表 / 详情 | `GET /v1/models`、`GET /v1/models/{model}` | OpenAI Bearer 或 Claude `x-api-key` | OpenAI / Anthropic SDK |

对话生成端点支持流式与非流式；Embeddings 与模型目录返回 JSON。

### 上游请求格式（出站）

| 后台选项 | 管理 API 设置 | 生成端点 | Base URL 示例 |
|---|---|---|---|
| OpenAI Chat Completions | `type: "openai"` + `openai_protocol: "chat"` | `/v1/chat/completions` | `https://api.openai.com`、`https://api.x.ai` |
| OpenAI Responses | `type: "openai"` + `openai_protocol: "responses"` | `/v1/responses` | `https://api.openai.com` 或支持 Responses 的兼容服务 |
| Anthropic Messages | `type: "anthropic"` | `/v1/messages` | `https://api.anthropic.com` |
| Gemini generateContent | `type: "gemini"` | `:generateContent` / `:streamGenerateContent` | `https://generativelanguage.googleapis.com` |

OpenAI 兼容供应商包括 xAI Grok、DeepSeek、GLM、Qwen、本地 vLLM/Ollama 等；按服务实际支持的接口选择 Chat Completions 或 Responses。

### 跨格式转换

**上游接口与客户端格式独立配置**。在「上游服务 → 基本信息」选择上游实际支持的接口，客户端通过自己的端点请求同一个模型，网关转换请求，并将响应转回客户端格式。例如：

```text
Anthropic SDK / Claude Code → /v1/messages
  → 网关转换 → OpenAI 上游 /v1/chat/completions
  → 网关转换 → Anthropic message / SSE 事件
```

后台将 OpenAI 分成两个独立选项，列表显示与筛选也分别区分。两者在管理 API 中均使用 `type: "openai"`，由 `openai_protocol` 固定出站格式：

| 后台选项 / API 设置 | 出站行为 |
|---|---|
| OpenAI Chat Completions / `chat` | 所有对话请求统一发送到 `/v1/chat/completions`；仅支持 Chat 的第三方上游选此项；后台新连接默认选此项 |
| OpenAI Responses / `responses` | 所有对话请求统一发送到 `/v1/responses`；只支持 Responses 的上游选此项 |
| OpenAI · 旧配置（跟随客户端） / `auto` 或空值 | 沿用旧行为：OpenAI Chat / Responses 请求使用对应端点，Anthropic / Gemini 请求转换为 Chat；仅编辑旧连接时显示此选项，升级或修改其他字段不会改变其格式 |

支持四种客户端格式与四种上游接口（Chat、Responses、Anthropic、Gemini）的基础对话与函数工具转换。高级能力和已知边界见 [Codex Responses 完整排查清单](docs/SUB2API_RESPONSES_AUDIT.md)：

- system、多轮文本、max_tokens、temperature、top_p、函数工具定义、工具选择、工具调用与工具结果；支持多个工具和分段工具参数，模型映射仍然生效。
- Codex 的 Responses 客户端工具支持 `custom` 自由文本工具、`namespace` 命名空间、`input.additional_tools` 与 `execution: "client"` 的 `tool_search`。转入 Chat / Anthropic / Gemini 时把自由文本包装成只有 `input` 字符串参数的函数；回程恢复 `custom_tool_call`、原始命名空间、工具搜索调用与 `call_id`。多工具历史合并为一个 assistant 调用批次，保留对应工具结果；搜索返回的工具加入下一轮定义。支持命名工具选择与 `allowed_tools` 子集。重名或展平后撞名会明确报错。grammar 转为工具描述提示，不能保持原生协议的强制语法约束。
- 自由文本工具的描述明确区分函数的 JSON 参数包装和 `input` 内的原始补丁／代码，保留客户端原说明。模型误将顶层 custom 工具 `apply_patch` 写成已有命名空间下的 `functions__apply_patch`，或遗漏唯一 custom 工具的命名空间时，会恢复原始工具身份；已明确声明的同名函数始终优先，存在歧义时不猜测工具归属。
- Responses 历史按实际调用批次配对结果，将并行调用的结果放到对应 assistant 批次之后。取消或重连后没有结果的调用、孤立结果，以及参数被截断的历史 function 调用与其结果会被清理；同一批次重复结果使用最后一份。供应商在后续批次复用调用 ID 时仍分别配对，同一批次内重复调用 ID 会报错。该整理只用于协议转换，原生请求保持完整透传。
- 兼容 Claude Code 在 Anthropic `messages[]` 中插入的纯文本 `role: "system"`：跨格式转换时按出现顺序合并到目标接口的系统指令，保留其他对话、图片和工具调用，且不让中途插入的指令改变会话指纹。转换后无法保留这些指令的原始生效位置；原生 Anthropic 路径仍透传完整请求。消息级 `clear_at`（除 `never`）、`output_config` 和工具增删块等特性不支持跨格式转换，会返回明确错误。
- 图片支持 OpenAI URL / data URL、Anthropic URL / base64、Gemini inlineData / fileData。转入 Gemini 的远程图片需要 MIME 类型；没有 MIME 时返回明确错误，可改用 base64。Responses 工具输出中的结构化图片、JSON 字符串包装的图片内容及整段图片 data URL 可转换：Chat / Gemini 将图片移到整批工具结果之后的 user 图片消息，按 call_id 标注归属，原工具结果保留文本和图片位置提示。孤立或被取消的工具结果不会带入图片，后续批次复用 call_id 时各自配对。图片不转为 Base64 普通文本，普通 JSON 文本保持原样；目标模型仍需要支持视觉输入。原生 Responses 与 Anthropic 转换保留结构化图片。
- SSE 逐增量转换文本、思考文本与工具参数，保留工具调用 ID、停止原因和真实 usage，包含上游明确返回的 0 Token。Gemini 的函数调用要求完整 JSON，因此只缓冲工具参数；上游忽略 stream 并返回 JSON 时，将完整结果包装成客户端 SSE。
- 转换路径在首次实际文本、推理或工具输出前不提交 SSE；空 role / 前导事件后报错或断流时，仍可按现有重试预算切换上游。已有输出后报错会发送失败事件并停止，避免重复执行工具。非流式客户端遇到 SSE 上游时先收齐有效结束事件，再聚合为 JSON；缓冲上限为 64 MiB，未结束或非法参数不会当成成功响应。
- 流式上游请求默认使用 `Accept-Encoding: identity`，避免上游压缩器未逐事件刷新、使推理阶段的 SSE 积压到正文开始时才下发；上游自定义请求头可覆盖该默认值。网关自身的静态资源 gzip 与 SSE 无关。
- Responses 上游仅提供文本／推理 done 事件、output_item.done 或最终 output 快照时，转换器会补读尚未输出的内容，避免空回答；已逐增量输出的块不会重复追加。转换后的输出项分别生成 `msg_`、`rs_`、`fc_`、`ctc_`、`tsc_` ID，所有增量、done 事件和最终快照使用同一 ID，工具结果配对的 `call_id` 保持原值。历史 function 参数兼容 JSON 字符串与对象，工具结果支持文本数组。
- Responses 生成完整的输出项、增量、完成事件；Anthropic 生成带 `event:` 名称的 Messages 事件。上游错误、非法工具参数与缺少结束事件的断流会返回客户端错误并记为失败，不补发成功结束事件。
- 自由文本工具的 JSON 参数会先收齐再解包，下发 `response.custom_tool_call_input.delta/done` 与完整的 `output_item.done`；不会把 JSON 包装或截断输入交给客户端执行。普通函数仍逐增量转发参数，支持上游分片或迟发工具名。
- 向 Anthropic 输出并行工具时，统一收齐、校验参数后按顺序发送完整内容块，正文和思考切换时关闭上一块，避免交错参数或错误块索引。普通 Chat / Responses 函数仍逐增量转发；工具初始 input 与后续参数 delta 不重复拼接。
- 完成事件可补齐已发送增量的缺失后缀；兼容 `response.done`、文本/推理/refusal 和 content part 的完成内容，矛盾快照判失败。支持 Chat 的 `reasoning` 与明文 `reasoning_details` 别名、Anthropic 明文 thinking 回放到 Chat；不将密文或推理过程伪装成最终答案。
- 函数参数 Schema 缺失根 type 或根 type 为 null 时补对象类型，保留 oneOf、defs 与 ref；转入 Responses 时未声明 strict 的函数使用 false。JSON 对象参数中的大整数 ID 保持原值。
- 明文 `agent_message` 和符合 Codex 委派信封的独立任务输入可以转换；加密任务内容仍需要原生支持。模型当前尚未加载的未知工具不会伪装为已加载能力。
- 四种原生对话上游忽略 stream=false 并返回 SSE 时，聚合为 JSON；Responses 保留原生输出项、ID、加密字段及服务端工具结果，Messages 保留签名与引用，Gemini 保留签名附着的 parts、图片和 grounding，Chat 保留末尾用量和按索引配对的工具。未结束的内容块/候选或未知 Messages 增量不作为成功；原生非流式 HTTP 200 的显式错误同样判失败。原生流式 SSE 字节仍保持原样。`/v1/responses/compact` 仅选原生 Responses 上游，不将远程压缩转为 Chat 摘要。
- 思考强度按模型映射后的实际目标适配：优先使用已缓存的上游模型目录 `effort.supported_levels`，没有声明时按已知模型规则兜底。保留受支持的档位，缺失档位映射到下一档，超过上限则使用最高档；例如 DeepSeek 的 `xhigh → max`、`medium → high`、`minimal → low`。支持 Chat 的 `reasoning_effort`、Responses 的 `reasoning.effort`、Anthropic 的 `output_config.effort`，原生与转换路径、流式与非流式均生效；每次故障转移重新适配，未知模型/自定义档位及 `none` 保留原值。
- 原生路径透传完整请求与响应，仅在需要模型映射或思考强度适配时改写对应字段。跨供应商转换无法保留 prompt cache 控制、思考签名或加密推理状态；历史 Anthropic thinking 与 Gemini thought 块不会作为普通文本重新发给其他供应商。
- Responses reasoning 历史优先读取明文 summary，为空时读取明文 content，在 Chat 上游恢复为 assistant 的 `reasoning_content`。连续的多个 reasoning 项与同一工具批次中的不同推理片段按顺序保留，重复附带同一推理的并行调用不会重复拼接。网关生成的 reasoning 输出按 API Key、客户端模型、上游 ID 和映射模型隔离缓存，每个 item ID 仅缓存自己的明文；明文与不透明项混合时逐项查找，不把已找到的内容写到其他 ID 下。缓存命中后续期 7 天，最多 1024 项／8 MiB，容量满时淘汰最久未使用的内容，重启后清空；不会解密供应商的 `encrypted_content`。缓存未命中时可回用同一用户轮次内之前已有的推理，新的 user 消息会清除该回用范围；回用内容不会伪装成未知 reasoning ID 的原始内容写入缓存。Anthropic / Gemini 上游不回放其他供应商的思考签名。
- 内置服务端工具（包括 `web_search`、`file_search` 和服务端 `tool_search`）、音频/视频/文档块、结构化输出配置、Responses 的 `previous_response_id` / `conversation` 等不支持跨格式转换，返回 400；普通 `text.format.type: "text"`、Chat `response_format.type: "text"`、Gemini `responseMimeType: "text/plain"` 可转换。Gemini 缓存上下文、continuationToken、安全控制、思考配置、topK、输出模态等没有等价映射，转换路径明确拒绝。转换路径请发送完整会话，特殊特性请使用原生接口。Responses 上游不支持 stop 序列；跨入 Gemini 时不支持 reasoning effort，跨入 Anthropic 时不支持 `none`（关闭思考），Gemini 不支持关闭并行工具调用。
- 四种格式的转换历史统一按调用批次配对结果；取消留下的未配对调用和孤立回复不会伪造回放，原生历史保持原样。工具重名及强制选择未声明工具明确失败。缓存/推理计费分项、独立 thinking 预算和原生 JSON-only 上游的流式包装仍需专项适配，详见[四种协议排查](docs/SUB2API_ALL_FORMATS_AUDIT.md)。
- 路由优先级（`pickUpstream`）：原生格式+显式白名单 ＞ 其他格式+显式白名单 ＞ 原生格式+通配 ＞ 其他格式+通配。Chat 与 Responses 分别判断原生匹配；想精确控制就给每个上游配模型白名单。
- 会话粘性对转换路径同样生效
- 转换能力不匹配时跳过该候选，继续寻找兼容或原生上游；本地转换失败不消耗网络重试次数，不请求上游、不计费。所有候选都无法转换时才返回 400。

设计参考：[New API 的 OpenAI 适配器](https://github.com/QuantumNous/new-api/blob/main/relay/channel/openai/adaptor.go)通过转换器接收 Claude / Gemini 请求；[Sub2API 的 Messages 分发](https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/service/openai_gateway_messages.go)按上游协议选择 Anthropic 直通、Chat 转换或 Responses 转换。本项目采用自己的规范化表示与转换实现。

Sub2API 兼容问题吸收记录（2026-10-14，参考源码 `9c2cca9`；Issue 是问题线索，不代表所有版本都仍有该问题）：

| 问题记录 | 本项目处理 |
|---|---|
| [#5659：apply_patch 返回 JSON 包装](https://github.com/Wei-Shaw/sub2api/issues/5659)、[#7241：namespace 中的 custom 被丢弃](https://github.com/Wei-Shaw/sub2api/issues/7241) | 自由文本先包装为函数、回程收齐并解包；支持 namespace 中的 custom，保留工具身份 |
| [#6146：custom 输出项 ID 前缀错误](https://github.com/Wei-Shaw/sub2api/issues/6146) | 按输出项类型生成 ID，并保持事件引用与最终快照一致；不混用 item ID 和 call_id |
| [#6584：搜索发现的工具没有加入下一轮](https://github.com/Wei-Shaw/sub2api/issues/6584)、[#5867：搜索结果没有 output](https://github.com/Wei-Shaw/sub2api/issues/5867) | 提取 tool_search_output.tools 加入工具定义，并保留发现结果供模型阅读 |
| [#5520：工具历史丢失 reasoning_content](https://github.com/Wei-Shaw/sub2api/issues/5520) | 回放明文及按 Key／上游／模型隔离的缓存推理；同一用户轮次可回用已有推理；不伪造加密状态 |
| [#2913：arguments 对象、output 数组及缺失 SSE 事件](https://github.com/Wei-Shaw/sub2api/issues/2913)、[#3253：空文本增量](https://github.com/Wei-Shaw/sub2api/issues/3253) | 接受对象参数与文本数组，生成完整输出事件，跳过空文本增量 |
| [#7222：空起始 chunk 阻断故障转移](https://github.com/Wei-Shaw/sub2api/issues/7222)、[#5800：非流式缓冲断流不重试](https://github.com/Wei-Shaw/sub2api/issues/5800) | 未提交输出时保留有界重试；SSE 聚合缺少终止事件判失败；提交之后禁止透明重放 |
| [#7285：HTTP 200 但忽略工具定义](https://github.com/Wei-Shaw/sub2api/issues/7285) | 协议由上游配置明确选择；仅测试连通性不能证明工具兼容，Chat-only 上游应明确选 Chat |
| [#4956：工具图片 Base64 进入文本上下文](https://github.com/Wei-Shaw/sub2api/issues/4956)、[#6240：原生路径把图片数组字符串化](https://github.com/Wei-Shaw/sub2api/issues/6240) | 转换时图片跟随完整工具批次并保留调用归属；原生路径不改写工具输出结构；普通 JSON 文本不重序列化 |
| [#3603：SSE 压缩导致推理阶段积压](https://github.com/Wei-Shaw/sub2api/issues/3603) | 按报告后续更正采用流式 identity 编码，不把延迟简单归因于前导事件缓冲 |
| [#4114：Read 参数等待 done 事件，缺事件时丢失](https://github.com/Wei-Shaw/sub2api/issues/4114) | 不给 Read 等工具增加特殊参数过滤；Anthropic 工具统一校验并收尾，其他函数按目标协议发送增量 |
| [#2098：WS 编辑重发破坏延续链](https://github.com/Wei-Shaw/sub2api/issues/2098)、[#6632：OAuth 加密推理丢失](https://github.com/Wei-Shaw/sub2api/issues/6632) | 当前没有 OAuth／WebSocket 状态桥；转换要求完整历史，不自动推断 previous_response_id，不改写原生会话链 |

服务端搜索／生图等工具不会伪装为普通客户端函数，此类请求需要原生支持，转换路径明确报错。模型是否选择 apply_patch 属于模型行为，协议兼容不能保证其一定调用该工具。[#2556](https://github.com/Wei-Shaw/sub2api/issues/2556) 的空 completion 静默拒绝检测尚未引入：仅推理输出、输出额度耗尽和供应商明确返回的 0 Token 不能简单判为失败；当前仍按实际结束原因与 usage 处理空回答。

要求 thoughtSignature 的新 Gemini 模型尚未实现跨格式工具签名回放，不能承诺 Responses 转 Gemini 的多轮闭环；原生 Gemini 保持签名。缓存读写 token、推理细分和服务端工具的细分计价也尚未完整承载，当前没有自动套用供应商缓存价格。完整处理状态与后续方案见排查清单。

#### Codex 接入 Chat 上游

后台将只支持 Chat Completions 的供应商设为 **OpenAI Chat Completions**（`openai_protocol: "chat"`），Codex 客户端继续使用 Responses。不要为 Chat-only 服务选择旧版「跟随客户端」，否则请求会发到上游的 `/v1/responses`。

以下为 Codex 的 `config.toml` 示例。`web_search = "disabled"` 必须放在顶层：Codex 默认启用搜索，而普通 Chat 上游无法执行服务端 `web_search`。原生 Responses 上游支持搜索时可按其能力启用。

```toml
model = "deepseek-v4-flash"
model_provider = "apishare"
web_search = "disabled"

[model_providers.apishare]
name = "API Share"
base_url = "https://your-gateway.example/v1"
env_key = "APISHARE_API_KEY"
wire_api = "responses"
requires_openai_auth = false
```

在运行 Codex 的环境中设置 `APISHARE_API_KEY` 为网关签发的 Key。模型需同时在该 Key 的授权范围和上游白名单内。HTTP JSON 与 SSE 转换已覆盖；WebSocket Responses、`/responses/compact` 和服务器保存的会话状态不属于此转换实现。长会话仍需遵循客户端与上游的上下文上限。

若模型称 `apply_patch` 不可用，还需检查客户端是否实际声明该工具：第三方模型缺少 Codex 模型目录元数据时，网关无法凭空增加一个客户端未注册的工具。DeepSeek 的[官方接入方案](https://api-docs.deepseek.com/quick_start/agent_integrations/codex/)提供独立模型目录，包括 `apply_patch_tool_type: "freeform"`、`shell_type: "shell_command"` 与 `use_responses_lite: false`。应按实际模型与客户端能力配置，保留网关模型名和地址；不要将另一供应商的模型上下文、能力或指令整套套用。

自动测试覆盖 Codex 工具定义／并行历史／命名空间恢复、工具搜索发现、分片 SSE 与断流、推理回放隔离及路由故障转移。可设置 `APISHARE_CODEX_BINARY` 为已安装 Codex 的原生可执行文件路径，再运行 `go test . -run TestCodexCLIResponsesToolLoop -count=1 -v`：该可选测试使用隔离目录、本地确定性模拟上游及测试 Key，验证真实 CLI 创建代码和测试、运行失败、修复代码、再次测试通过、结果回传和最终回答；不调用真实模型供应商。

## 核心特性

- **会话粘性（保证 cache 命中）**：同一对话固定路由到同一上游。识别优先级：`X-Session-Id`/`X-Conversation-Id` 头 ＞ 对话前缀指纹 `hash(端点+模型+system+首条user消息)`。粘性表存 SQLite、滑动 TTL（默认 30 分钟，命中自动续期），重启不丢。粘性上游故障时自动转移并重绑，恢复后不回切
- **加权调度 + 故障转移**：上游 401/403/408/429/5xx/529 或网络错误时透明重试下一个上游；连续失败进入指数退避冷却（30s 起步，上限 10 分钟）
- 冷却中的上游不会被普通或粘性请求选中，成功响应会清除连续失败计数。客户端取消、响应写出失败及请求参数错误不触发上游冷却；客户端取消后停止故障转移。
- **计费（自动官方定价）**：启动时与每日自动同步 **LiteLLM 官方牌价镜像库**（各厂商无定价 API，这是 LiteLLM/sub2api 一类网关通用的官方定价来源，覆盖 OpenAI / Anthropic / Gemini / Grok / DeepSeek 等全部现行型号，含 Bedrock/Vertex 键名形态归一化）。计费优先级：**手动覆盖 > 官方定价 > 内置兜底表 > default(0)**。用量优先取上游真实 usage（OpenAI 流式自动注入 `include_usage`；Anthropic 解析 `message_start/message_delta`；Gemini 解析 `usageMetadata`；Responses 解析 `response.completed`），缺失时 tiktoken 本地估算
- **限流**：每 Key 独立 QPS 令牌桶 + 并发信号量
- **公开首页 `/`**：展示全站累计 Token、调用趋势、各模型调用量和真实调用 Uptime；输入密钥后查看个人额度及模型独立余量。公开接口不返回额度、密钥、名称、数量、上游地址或凭证。
- **管理后台（默认 `/admin`）**：Key 签发与额度、上游管理（四种请求格式 + 连通性测试）、调用日志（含格式/上游/usage/费用）、价格表与调度参数；未注册路径一律 404
- **上游独立代理**：后台集中管理 HTTP / HTTPS / SOCKS5 / SOCKS5h 代理及认证，每个上游可单独选择；模型调用、流式响应、连通性测试和模型目录获取均通过所选代理。
- **自定义请求头**：每个上游可独立设置固定请求头、客户端请求头透传、正则匹配与变量替换；原生和跨格式转发、流式响应、连接测试及模型目录获取均生效。
- **单文件部署**：前端内嵌 Go 二进制，SQLite（WAL）存储，零外部依赖

转发路径使用不可变的内存上游快照，预解析模型白名单、模型映射和请求头正则。后台新增、修改、删除上游后立即失效，已有请求继续使用接收时的配置；快照每 30 秒过期，下一次访问自动重建。密钥鉴权、模型权限和额度仍实时读取，不使用过期的余额缓存。模型目录最多并行获取 4 个通配上游，并按原有上游顺序合并结果。

设计上参考 [New API 的渠道缓存](https://github.com/QuantumNous/new-api/blob/633b4c5b190dc1af99a2814391354d86948e7943/model/channel_cache.go)和 [Sub2API 的共享 HTTP 客户端池](https://github.com/Wei-Shaw/sub2api/blob/ab80abc6d1f929a6864c8804fd077b509d21fda2/backend/internal/pkg/httpclient/pool.go)，采用适合单进程 SQLite 网关的独立实现。

## 快速开始

### 本地运行

需要 Go 1.26.2 或更高版本。

Linux / macOS：

```bash
go build -o apishare .
PORT=8080 API_DB=apishare.db ./apishare
```

Windows PowerShell：

```powershell
go build -o apishare.exe .
$env:PORT = "8080"
$env:API_DB = "apishare.db"
.\apishare.exe
```

首次启动先创建 SQLite 数据库文件，管理员密码和后台路径在首次配置页面保存。

1. 打开 `http://127.0.0.1:8080/`，新部署自动跳转到独立的 `/setup/` 配置页。填写管理员密码、确认密码和后台路径后保存，配置写入数据库并自动进入新后台。无需预先登录；配置完成后，首页恢复为公开服务页面；
2. 「上游」页添加上游：直接选择上游请求格式：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 或 Gemini generateContent。填 Base URL（不含 `/v1`/`/v1beta`，如 `https://api.openai.com`）、API Key、权重、**模型白名单**（强烈建议填写，跨格式路由靠它）；
3. 「API 密钥」页创建访问密钥，绑定可用模型，设置共享额度或各模型独立额度（USD，0=不限）、QPS 和并发限制；未绑定模型的密钥无法调用；
4. 「设置」页确认自动定价已同步（可手动"立即刷新"；GitHub raw 不可达时把定价源 URL 换成镜像，如 `https://cdn.jsdelivr.net/gh/BerriAI/litellm@main/model_prices_and_context_window.json`）。价格表 JSON 只需放**手动覆盖**项和 `default` 兜底价，其余模型自动用官方价。后台可随时用"查询模型单价"核对某模型的价格与来源。

### 部署配置

在后台侧栏打开「部署配置」，可以随时修改管理员密码和后台访问路径：

新数据库在首次保存前处于未配置状态，刷新首页或重启服务仍会进入 `/setup/`。首次配置成功后，该初始化接口关闭，后续修改需在后台登录并验证当前密码。已有数据库的管理员配置会直接沿用。

- 新密码至少 12 个字符，最多 72 字节，需要再次输入确认；仅修改路径时，新密码留空即可。
- 后台路径默认为 `/admin`，可设置为 3–64 位字母、数字、短横线或下划线，以字母或数字开头。页面支持随机生成路径、实时地址预览和复制入口；系统保留路径不能使用。
- 保存需要验证当前管理员密码。密码仅保存 bcrypt 摘要，配置写入数据库并在重启后保留。数据库中已保存的配置优先于初始环境变量。
- 后台路径保存后立即生效，无需重启；旧入口返回 404，请更新书签。当前浏览器继续登录，其他旧会话失效。公开首页、API 接入路径和现有 API 密钥不受影响。
- 已有部署升级后不会强制重复首次配置，可直接在「部署配置」中更新。

管理员密码、实际后台路径、API 密钥和调用记录仅保存在本地 SQLite 数据库中。数据库、编译产物、本地环境变量文件和测试输出均由 `.gitignore` 排除；分享代码时使用默认 `/admin` 或通用示例，不要将实际部署配置写入源码或文档。

### 公开首页

在后台「公告管理」新建公告，填写标题和正文，可先保存草稿或直接发布；支持编辑、置顶、撤下与删除。公告保存在 SQLite，重启后保留。标题最多 120 个字符，正文最多 10,000 个字符，按纯文本显示并保留换行，可在发布前查看预览。

主页首屏左上角显示公告入口，模型目录下方展示公告卡片，可查看全文和展开更多公告。公开接口 `GET /api/public/announcements` 仅返回最近 5 条已发布公告，置顶优先、同级按发布时间倒序；草稿不公开。没有已发布公告时隐藏入口和区域。公告随主页每 30 秒更新，也可以手动刷新；查看全文时暂停自动更新。后台管理接口沿用管理员鉴权和自定义后台路径。

首页、后台和首次配置页的脚本与样式地址自动附带内容版本号，升级后浏览器和 CDN 会请求新地址，避免新页面加载旧脚本、旧样式。HTML、公开数据接口及不带有效版本号的资源使用 `Cache-Control: no-store`；内容版本匹配的公开资源与固定版本的第三方库可以长期缓存。反向代理或 CDN 应遵循源站缓存头，不要强制缓存 HTML、`/api/public/` 或 `/api/key/` 接口。

访问服务根地址 `/`，无需登录即可查看全站累计 Token、累计调用、用量趋势、各模型调用量与 Uptime。统计汇总全部调用记录，包含停用、删除密钥的历史调用；后台不再需要“首页展示”开关。公开接口不包含任何额度或密钥身份信息。

在概览第一张「我的额度」卡片输入 API 密钥，验证后查看该密钥的额度余量。可切换密钥或退出；只读查询会话有效期为 7 天，刷新页面与服务重启后保留，停用或删除密钥会立即撤销会话。完整密钥仅用于首次验证，不放入 URL 或浏览器存储；会话使用 HttpOnly / SameSite Cookie，HTTPS 下设置 Secure，数据库仅保存查询令牌摘要。查询会话不能调用模型或管理后台。

- 个人总余量对当前绑定模型的共享额度和独立额度分别计算 `max(额度 - 对应已使用, 0)` 后求和；共享额度只合计一次。存在不限额池时显示“不限额度”，同时保留有限额度部分的余量；未绑定模型时明确提示。
- 登录前模型列表仅展示全站状态与用量。验证后，对应模型显示「我的独立额度」、剩余进度与耗尽提示；共享模型标记「使用我的共享额度」，未授权模型标记「当前密钥未开通」。额度耗尽仍能查询。
- 模型目录来自启用且有分配权重的上游白名单；通配上游在后台发现模型并缓存，发现模型不计为成功调用。所有访客看到相同目录，个人查询不会过滤模型列表。
- **Uptime 固定统计最近 24 小时的全站真实请求成功率**，按总成功请求 / 总请求加权计算。默认每小时一格，可切换为每分钟一格；两种粒度覆盖同一个滚动 24 小时窗口，切换不改变成功率。分钟轨迹支持横向滚动，历史详情可选择任意一小时查看分钟记录。无调用时段显示灰色，且不计入分母。
- 当前模型状态取最近 24 小时内的最后一次全站调用结果；无近期调用显示灰色。故障转移后的最终请求结果计为一次调用，断开的流式响应计为失败。
- 主页保留奶油色背景、陶土色流线动画与全屏 Token 首屏，中央数字逐位滚动到真实全站总量。模型调用量图表按累计调用次数排序并保留全部模型。每 30 秒更新，可暂停；标签页隐藏或查看历史时暂停请求，减少动态效果偏好会关闭数字滚动与入场动画。没有调用时显示真实的 0。
- 公开概览在服务端按小时／分钟分别缓存 5 秒，并合并同时到来的统计查询；调用统计最多有约 5 秒的缓存延迟。上游新增、修改、删除或模型发现成功会立即清除概览缓存。缓存只包含匿名全站统计，不包含个人额度；浏览器及 CDN 仍遵循 `Cache-Control: no-store`。
- 3D 站台暂时封存，代码与资源保留在仓库中，主页入口已移除，`/metro` 及对应资源返回 404。

### 客户端接入示例

#### 配置上游代理

1. 在后台「代理配置」添加代理名称和地址，例如 `http://127.0.0.1:7890`、`https://proxy.example.com:8443` 或 `socks5://127.0.0.1:1080`。地址必须包含端口；IPv6 使用 `socks5://[::1]:1080`。
2. 如代理需要认证，分别填写用户名与密码。列表不会返回密码；编辑时密码留空保留原值，勾选「清除已保存的密码」可删除密码，清空用户名则移除认证。
3. 打开「上游服务」添加或编辑上游，在「连接与认证 → 请求代理」选择代理并保存。一个代理可以供多个供应商共用，配置保存在 SQLite，重启后保留。

已选择的代理不受系统 `NO_PROXY` 绕过规则影响；SOCKS5 / SOCKS5h 均由代理解析目标域名。未选择代理时沿用原有 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境设置，未设置时直连。代理配置修改对后续请求生效，进行中的请求继续使用原连接。停用或失效的代理不会自动降级为直连，而是触发既有上游故障转移；删除代理前需要解除所有上游关联。可使用上游列表的「测试连接」或编辑器的「获取当前 Key 模型」验证实际代理链路。

#### 自定义请求头

在「上游服务 → 添加 / 编辑 → 请求头覆盖」填写 JSON，或点击「填入模板」「透传模板」。支持复制、格式化与实时 JSON 校验；留空或填写 `{}` 使用默认请求头，保存后立即生效，重启后保留。

```json
{
  "*": true,
  "re:^X-Trace-.*$": true,
  "X-Foo": "{client_header:X-Foo}",
  "Authorization": "Bearer {api_key}"
}
```

- 字符串设置固定值，可包含 `{api_key}`（当前上游密钥）和 `{client_header:NAME}`（客户端请求头值，名称不区分大小写）。同名请求头被覆盖；客户端变量不存在时跳过该条规则，保留已有默认值。
- `true` 透传客户端的同名请求头，保留多值；`false` 移除请求头。`*` 匹配全部，`re:` 按不区分大小写的 Go 正则匹配请求头名称，值必须是布尔值。
- 应用顺序为通配规则、正则透传、正则移除、精确名称规则，因此精确规则优先；同一层按名称排序，不依赖 JSON 字段顺序。请求头名称不区分大小写，不允许重复。
- 通配和正则规则过滤 `Authorization`、`x-api-key`、`x-goog-api-key`、Cookie 等凭证，默认使用上游密钥。需要修改认证时使用精确名称，例如 OpenAI 的 `"Authorization": "Bearer {api_key}"`、Anthropic 的 `"x-api-key": "{api_key}"` 或 Gemini 的 `"x-goog-api-key": "{api_key}"`；模板会按当前接口类型填入。
- `Host`、`Content-Length`、`Connection`、`Transfer-Encoding` 等连接控制头由 HTTP 自动管理，不能自定义；客户端 `Connection` 指定的头也不透传。非法名称、换行或控制字符会被拒绝。
- 连接测试和模型获取也应用当前配置，并支持保存前测试。它们没有客户端调用请求头，客户端变量对应规则会跳过；管理员的登录凭证不会参与透传。修改配置会重新获取模型目录缓存。

管理接口的 `header_overrides` 字段为 JSON 文本字符串，与 `model_map` 一样。更新时省略字段保留原值，传空字符串清除。

在同一区域选择 **Codex 模板** 或 **Claude Code 模板**，可以替换 `User-Agent`，同时保留其他请求头。页面显示最新已检测版本、检测时间和生成的请求头，也可点击「检查更新」立即刷新。

版本来自官方 npm 包的 `latest` 稳定版本：[Codex](https://registry.npmjs.org/@openai/codex/latest)、[Claude Code](https://registry.npmjs.org/@anthropic-ai/claude-code/latest)。服务启动时检测，正常每小时检查，失败后每 5 分钟重试；两个客户端分别更新，单个检测失败不会影响另一个。最近成功版本保存到 SQLite，网络失败或重启时继续使用缓存；没有成功获取过版本时不会填写虚构的版本号。

模板使用动态变量，保存后后续请求自动跟随新版，无需重新编辑上游：

```json
{"User-Agent": "{codex_user_agent}"}
```

```json
{"User-Agent": "{claude_code_user_agent}"}
```

分别生成 `codex_cli_rs/<版本>`（Codex CLI 标识的版本部分）与 `claude-cli/<版本> (external, cli)`。格式依据 [Codex 官方客户端源码](https://github.com/openai/codex/blob/main/codex-rs/login/src/auth/default_client.rs)与 [Claude Code 官方维护记录](https://github.com/anthropics/claude-code/issues/72879)。这些变量在转发、流式请求、连接测试和模型获取中都生效；新版被检测到后，相关上游的模型目录缓存也会更新。手动填写固定 `User-Agent` 的上游不会跟随版本变化。

#### 获取当前 Key 的模型

后台「上游服务 → 添加 / 编辑 → 模型访问」点击 **获取当前 Key 模型**，会使用当前表单的接口类型、Base URL 和 Key 实时请求模型目录。编辑已有连接时，Key 留空使用已保存的值；填写新 Key 可在保存前验证。OpenAI 兼容格式和 Claude / Anthropic 原生格式均支持，Claude 分页会自动读完。结果可搜索、勾选并保存为模型白名单；空列表和请求失败会分别提示，不会覆盖已有白名单。

使用网关签发的 Key 时，两种客户端均请求 `/v1/models`：

```bash
# OpenAI：返回 object=list、data[]；单个模型含 id/object/created/owned_by
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer sk-xxxx"

# Claude：返回 data[]、has_more、first_id、last_id
curl "http://127.0.0.1:8080/v1/models?limit=20" \
  -H "x-api-key: sk-xxxx" \
  -H "anthropic-version: 2023-06-01"
```

Claude 列表支持 `limit`（1–1000，默认 20）、`after_id` 和 `before_id`（二选一）。使用 Bearer 认证的 Claude 客户端也可以携带 `anthropic-version` 指定 Claude 返回格式；仅带 `x-api-key` 时默认采用 Claude 格式。两种格式均支持 `GET /v1/models/{model}` 查询目录内的单个模型。返回字段参照 [OpenAI 模型接口](https://developers.openai.com/api/reference/resources/models/methods/list) 和 [Claude 模型接口](https://platform.claude.com/docs/en/api/models/list)。

网关 Key 的模型目录来自启用且权重大于零的上游，并按该 Key 的可用模型配置过滤，仍执行 Key 鉴权与限流检查。显式上游白名单直接纳入目录；通配上游根据其当前上游 Key 拉取并缓存 5 分钟，更换地址、类型或 Key 后重新获取。目录按模型 ID 排序去重，映射别名只有符合上游白名单时才返回；未知发布时间使用 `0` / Unix epoch，展示名使用模型 ID。模型目录表示配置或上游声明的可用性，不保证每个模型的推理请求一定成功；查询目录不计费、不写入推理调用统计，额度用完仍可查询已绑定的模型。

#### 每个网关密钥的可用模型

在「API 密钥」创建表单填写 **可用模型**，或点击已有密钥的 **模型与额度** 按钮修改。可点击 **获取可用模型**，搜索并勾选启用上游提供的模型，也可手动输入，以逗号或换行分隔。留空或点击 **清空模型绑定** 后无法调用任何模型；已有数据库中未绑定模型的旧密钥也需要先绑定模型。

- 白名单按客户端请求的模型 ID 精确匹配，区分大小写，不支持通配符。使用模型映射时，权限检查发生在映射之前；允许某个别名不会同时允许它映射到的上游模型 ID。
- `/v1/models` 只列出该密钥白名单与上游目录的交集，Claude 分页也基于过滤后的列表。无权限或目录中不存在的模型详情返回 404。
- Chat、Responses、Messages、Embeddings 和 Gemini 的普通及流式调用均执行权限检查。未授权模型返回 403，不转发、不计费；会话粘性和故障转移不能绕过权限。
- 保存后对新请求立即生效。白名单不会使上游未提供的模型变得可调用，正在处理的请求沿用接收时的配置。

管理 API 的 `POST /api/keys`、`PATCH /api/keys/{id}` 使用 `models` 字符串字段（实际路径在后台入口下，例如 `/admin/api/keys`）。创建时省略字段表示未绑定；更新时省略保留原值，传 `""` 清空绑定。`GET /api/keys/models` 返回 `{ "models": ["model-a", "model-b"] }`，供管理员选择可用模型。

#### 共享额度与模型独立额度

- 每个已绑定模型默认 **使用共享额度**，多个模型共同消耗同一份 `quota_usd`。也可以逐个切换为 **独立额度**，或点击 **全部独立设置** 后分别填入金额。
- 独立额度仅消耗对应模型的额度，不减少共享额度，也不影响其他模型的独立额度。独立额度用完后不会回退到共享额度；共享额度用完也不会阻止仍有独立额度的模型调用。耗尽的调用返回 402，不转发、不扣费。
- 金额单位为 USD，共享及独立额度均以 0 表示不限。额度按客户端模型 ID 计算，模型别名与映射后的上游 ID 分开配置。
- 修改额度、切换额度方式或重新绑定模型不会清零已用金额。正在处理的请求按接收时选定的额度方式结算；过去的消费不会在共享和独立额度之间转移。升级时已有消费保留为共享已用金额。
- 列表分别显示共享额度使用、各模型独立额度使用与累计费用；首页合计只统计已绑定模型实际使用的额度池，共享池仅合计一次，全独立模式不计入闲置的共享池。

管理 API 使用 `model_quotas` 指定独立额度，没有出现在该对象中的已绑定模型使用共享额度。例如：

```json
{
  "name": "项目 A",
  "models": "model-a,model-b,model-c",
  "quota_usd": 100,
  "model_quotas": {
    "model-c": { "quota_usd": 20 }
  }
}
```

上例中 model-a 与 model-b 共用 100 USD，model-c 单独使用 20 USD。更新时省略 `model_quotas` 保留原配置，传 `{}` 切回全部共享；传入对象替换整份独立额度配置。取消模型绑定时同步取消该模型的独立额度配置，历史已用金额仍保留。响应中的 `used_usd` 为累计费用，`shared_used_usd` 为共享池已用金额，`model_quotas[模型].used_usd` 为该模型独立池已用金额；已用金额由服务端维护，客户端不能修改。

#### 调用模型

```bash
# OpenAI 格式（也可指向 Grok/DeepSeek 等模型，网关自动路由）
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxx" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# Claude Code 直接指向网关（ANTHROPIC_BASE_URL=http://127.0.0.1:8080）
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: sk-xxxx" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-20250514","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}'

# Gemini 格式
curl "http://127.0.0.1:8080/v1beta/models/gemini-2.5-flash:generateContent" \
  -H "x-goog-api-key: sk-xxxx" -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}'
```

想锁定会话粘性时带 `-H "X-Session-Id: <会话id>"`；不带网关也会按对话前缀自动识别。

### Docker

Docker 镜像由 GitHub Actions 在 GitHub 上自动打包，并发布到 GitHub Container Registry（GHCR），支持 `linux/amd64` 和 `linux/arm64`。镜像地址为 `ghcr.io/<owner>/<repo>`，其中用户名或组织名、仓库名均使用小写。发布成功后，可直接拉取镜像运行，无需在本地编译：

```bash
docker run -d --name apishare -p 8080:8080 -v apishare-data:/data \
  ghcr.io/<owner>/<repo>:latest
```

`apishare-data` 保存数据库和部署配置。启动后打开 `http://127.0.0.1:8080/`，首次部署会跳转到配置页，默认后台路径为 `/admin`。

### GitHub Actions 自动发布

工作流在 [.github/workflows/docker.yml](.github/workflows/docker.yml)，构建与发布均在 GitHub Actions 中完成：

- 推送到 `main` 分支 → 发布 `:main` 和 `:sha-<提交短哈希>`；`main` 为默认分支时，同时更新 `:latest`。
- 推送版本标签（如 `v1.2.3`）→ 发布 `:1.2.3`、`:1.2` 和 `:sha-<提交短哈希>`；正式版本同时更新 `:latest`。
- 支持在仓库 **Actions → Publish Docker image → Run workflow** 手动触发。

```bash
git remote add origin git@github.com:<you>/<repo>.git
git push -u origin main
git tag v0.1.0
git push origin v0.1.0   # 触发版本发布
```

工作流使用 GitHub 自动提供的 `GITHUB_TOKEN`，已声明 `packages: write` 权限，无需另填镜像仓库密码。发布结果可在仓库 Actions 页面查看，镜像可在 Packages 页面查看。若希望用户无需登录即可拉取，首次发布后将该 Package 的可见性设为 Public。认证与可见性规则参见 [GitHub GHCR 文档](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | `8080` | 监听端口 |
| `API_DB` | `apishare.db` | SQLite 文件路径（容器内为 `/data/apishare.db`） |
| `ADMIN_PATH` | `admin` | 首次配置页的默认后台路径，后续以数据库保存的路径为准 |
| `UPSTREAM_MAX_IDLE_CONNS` | `256` | 上游连接池的最大空闲连接总数，直接连接与每个代理连接池分别使用 |
| `UPSTREAM_MAX_IDLE_CONNS_PER_HOST` | `64` | 每个上游主机保留的最大空闲连接数 |
| `UPSTREAM_DIAL_TIMEOUT_SEC` | `15` | TCP 建连及 TLS 握手超时，单位秒 |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `180` | 等待上游响应头的超时，单位秒；0 关闭 |
| `UPSTREAM_IDLE_TIMEOUT_SEC` | `300` | 等待上游响应体数据的超时，单位秒；0 关闭 |
| `TIKTOKEN_CACHE_DIR` | 系统临时目录下的 `data-gym-cache` | 可选的分词词表缓存目录；也兼容已有 `DATA_GYM_CACHE_DIR` 配置 |

管理员密码通过首次配置页创建，不再从 `ADMIN_PASSWORD` 生成；已有数据库中保存的密码继续有效。

连接参数在启动时读取，对直接连接和上游独立代理均生效。响应体超时只作用于正在等待上游数据的读取，不限制持续输出的流式响应总时长，也不会因客户端消费已缓冲数据较慢而触发。尚未向客户端写出内容时可故障转移，已经开始的流不会重放。空闲连接数范围为 1–65536；建连超时范围为 1–86400 秒，响应头／响应体超时范围为 0–86400 秒；无效配置使用默认值并记录配置项名称。

## 本地联调工具

`cmd/mockupstream` 是一个假上游，同时实现四种服务端格式，回复里带自己的名字，用来验证转换、粘性与计费：

```bash
go run ./cmd/mockupstream -addr :18081 -name mockA
go run ./cmd/mockupstream -addr :18082 -name mockB
# 管理后台把两个 mock 加为 openai 上游（白名单 mock-model），再各加 anthropic/gemini 类型上游指向同端口即可
```

## 注意事项

- 仅在上游缺少真实 usage 时估算对应 Token。启动时后台预热常用分词词表，按编码共享初始化与缓存；下载超时为 5 秒，失败后至少间隔 1 分钟才重试。词表未就绪或离线时立即使用字符启发式估算，不阻塞转发；上游明确返回的 0 Token 同样按真实 usage 处理。普通对话的输入估算包含完整多轮文本、工具调用／结果与工具定义，与只取会话前缀的粘性指纹分开；图片和特殊原生状态仍只能近似估算。
- Gemini 只支持 `generateContent` / `streamGenerateContent`（流式统一按 SSE `alt=sse` 处理，官方 SDK 均使用该模式）；`countTokens` 等其他方法不转发。
- 跨格式流式响应中，若上游在首个增量前没有报告 usage，Anthropic 的 `message_start.usage.input_tokens` 为 0；最终真实用量在 `message_delta.usage` 中。原生路径沿用上游事件。
- 已下架型号（如 grok-4）在现行官方价格库中无标价，按 default(0) 记账不扣费；需要计费就在价格表里手动覆盖。
- 上游健康状态（连续失败/冷却）在内存中，重启后清零。
- 建议在前端挂 nginx/caddy 做 HTTPS；`/healthz` 可用于探活。

## 许可证

项目代码采用 [MIT License](LICENSE)。内嵌的 Space Grotesk 和 Orbitron 字体分别遵循其 [SIL Open Font License](web/public/fonts/OFL.txt) 和 [SIL Open Font License](web/public/fonts/orbitron-OFL.txt)，字体目录中保留了原始授权文件。
