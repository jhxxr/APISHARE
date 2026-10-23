# 客户端 User-Agent 调研

采集日期：2026-10-23（Asia/Shanghai）。

已接入入站客户端识别、调用日志、日志筛选与导出，以及仪表盘客户端统计。配套的 [JSON 样本库](client-user-agents.json) 保存调研正则、版本捕获位置、辅助请求头、适用范围和固定版本源码链接；它是调研与回归样本，运行时使用 `cmd/apishare/request_client.go` 中的固定规则，不会在请求路径联网查询客户端版本。

**以下是从官方源码或官方发布包提取的格式，不是本项目实际抓到的请求。** `{version}` 等表示变量；JSON 中的示例也是依据源码构造的样本，不代表当前最新版本。没有确认的 UA 不补猜测值。

## 已确认的应用 UA

这些特征可以识别请求声明的客户端，但不能证明实际使用的软件；SDK、插件、代理和用户配置都可能改写请求头。

| 客户端 | UA 格式 | 辅助特征与适用范围 |
| --- | --- | --- |
| Codex | `codex_cli_rs/{version} ({os} {os_version}; {arch}) {terminal}` | 默认 `originator: codex_cli_rs`；同一官方实现还认可 `codex-tui`、`codex_vscode` 等 originator，UA 前缀随之变化。不要只匹配 `codex_cli_rs`。[S01] |
| Claude Code | `claude-cli/{version} (external, {entrypoint})` | 已检查官方 npm 包 `2.1.20`；`x-app: cli`，entrypoint 来自环境，可能附加 `agent-sdk/{sdk_version}`。不要要求固定后缀 `(external, cli)`。[S02] |
| OpenCode | `opencode/{channel}/{version}/{client}` | 当前官方构造函数；模型请求带 `x-opencode-client`、`x-opencode-project`、`x-opencode-session` 等。[S03][S04] |
| OpenCode 1.x | `opencode/{version}` | 已确认 `v1.19.3` 的模型请求使用此格式，而同一版本还存在上面的四段构造函数。兼容后续追加的 SDK/runtime 标识，不要完整字符串相等匹配。[S05][S06] |
| Gemini CLI | `GeminiCLI/{version}/{model} ({platform}; {arch}; {surface})` | 可变成 `GeminiCLI-{clientName}/...`。VS Code surface 使用 `CloudCodeVSCode/{version} (...; proxy_client=geminicli)`，需要一起匹配该标记；用户自定义头可以覆盖 UA。[S07] |
| Cline | `Cline/{version}` | 已确认 Cline/Cline Pass 计费 provider、OpenAI Codex provider、OpenCode Go provider 的专属头。计费路径还带 `X-CLIENT-TYPE`、`X-CLIENT-VERSION`、`X-Title: Cline`。普通自定义 OpenAI/Anthropic provider 不保证走这组头。[S08] |
| Roo Code | `RooCode/{version}` | OpenAI 兼容请求使用 `DEFAULT_HEADERS`，包含 `X-Title: Roo Code` 和 `HTTP-Referer: https://github.com/RooVetGit/Roo-Cline`；检查到的原生 Anthropic 构造路径没有设置这组头。[S09][S10][S11] |
| Kilo Code | `kilo/{channel}/{version}/{client}` | 已确认 CLI 模型请求使用该 UA；Kilo gateway provider 另有 `opencode-kilo-provider` 或 `opencode-kilo-provider/{version}`。部分路径带 `X-Title: Kilo Code`、`X-KILOCODE-EDITORNAME`。[S12][S13][S14][S15][S16] |
| Kimi CLI | `KimiCLI/{version}` | 已确认 `provider.type == "kimi"` 的默认头。检查到的 `openai_legacy`、`openai_responses`、`anthropic` 路径仅传用户自定义头，不能假定都带 Kimi UA。[S17][S18] |
| Qwen Code | `QwenCode/{version} ({platform}; {arch})` | 默认 OpenAI 兼容 provider 设置此 UA；DashScope 路径还带 `X-DashScope-UserAgent`。[S19][S20] |
| LobeHub Desktop | `LobeHub Desktop/{version}` | 官方桌面端存在专用 UA 设置函数。还需要实测确认不同模型转发路径中，该 UA 是否一直传到自建网关。[S22] |

### 已确认的身份歧义

Qwen Code 的 Anthropic provider 在 **非 Anthropic 官方 Base URL** 下，会主动使用 `claude-cli/{qwen_version} (external, cli)` 并设置 `x-app: cli`；直连 Anthropic 才使用 `QwenCode/...`。[S21]

因此，`claude-cli` 与 `x-app: cli` 同时出现，也不足以证明实际应用是 Claude Code。统计可以记为“Claude Code（请求声明）”，同时保留识别依据。没有额外证据时，不应反过来猜测一定是 Qwen Code。其他客户端也可以配置自定义请求头。

同样，`x-opencode-session` 不是 OpenCode 独有：本次检查的 Cline OpenCode Go provider 和 Cherry Studio OpenCode Go/Zen 配置也会设置它。[S08][S25]

## 应用标识主要来自辅助请求头

这些字段可补充 UA，**仅在字段实际出现时使用**。表中限定的 provider 路径不等于“连接 API Share 时默认也发送”。

| 客户端 | 已确认的辅助头 | 限制 |
| --- | --- | --- |
| Cherry Studio | `HTTP-Referer: https://cherry-ai.com`；`X-Title: Cherry Studio` | 规范预置/托管 provider 保留归属头；当前源码对纯自定义 provider 默认不添加这组头。自建 API Share 通常需要特别检查这一点。[S23][S24] |
| Aider | `HTTP-Referer: https://aider.chat`；`X-Title: Aider` | 官方代码设置 LiteLLM 的 `OR_SITE_URL`、`OR_APP_NAME`，用于 OpenRouter 归属。该证据不是普通 OpenAI 请求具有 Aider 专属 UA 的证明。[S26] |
| Open WebUI | `HTTP-Referer: https://openwebui.com/`；`X-Title: Open WebUI` | 已检查实现仅在 URL 包含 `openrouter.ai` 时自动添加。[S27] |
| SillyTavern | `HTTP-Referer: https://sillytavern.app`；`X-Title: SillyTavern` | 在 OpenRouter、AIMLAPI、Featherless 等特定 provider 配置中定义；普通自定义 OpenAI 请求不保证携带。[S28][S29] |
| LobeHub | `HTTP-Referer: https://lobehub.com`；`X-Title: LobeHub` | 已确认 OpenRouter provider 的默认头，不代表所有 LobeHub Web/服务端调用都有专属 UA。[S30] |

归属头匹配时，`X-Title` 应做完整值匹配；`HTTP-Referer` 应解析 URL 后核对 hostname，避免 `cline.bot.attacker.example` 等字符串被误识别。这些头同样可以被改写，只能作为客户端自报特征。

## 通用 SDK 回退分类

没有应用标识时，以下 UA 只记 SDK，应用仍为 `unknown`。不能把 OpenAI SDK 直接统计成 Codex，也不能把 Anthropic SDK 直接统计成 Claude Code。

| SDK | 默认 UA 格式 | 说明 |
| --- | --- | --- |
| OpenAI Python | `OpenAI/Python {version}`、`AsyncOpenAI/Python {version}` 等 | 前半部分实际来自客户端类名；该版本是 SDK 版本。[S31] |
| OpenAI JavaScript/TypeScript | `OpenAI/JS {version}` 等 | 前半部分来自构造函数名；检查到的浏览器路径不会主动设置 SDK UA。[S32] |
| Anthropic Python | `Anthropic/Python {version}`、`AsyncAnthropic/Python {version}` 等 | 前半部分来自客户端类名；该版本是 SDK 版本。[S33] |
| Anthropic JavaScript/TypeScript | `Anthropic/JS {version}` | 只能识别 SDK。[S34] |
| Google Gen AI Python | `google-genai-sdk/{version} gl-python/{python_version}` | 也会设置 `x-goog-api-client`；用户 UA 可以与库标识组合。[S35] |

浏览器 UA、空 UA、只有运行时/HTTP 库标识的 UA，统一归入未知客户端，保留 UA 供后续补样，不据此猜测应用名称。

## 需要实际请求补样的客户端

“待补样”表示本轮尚未获得足够的应用识别证据，不表示该软件一定没有专用 UA。

| 客户端 | 本轮检查结果 | 后续采样重点 |
| --- | --- | --- |
| Chatbox | `createAfetch` 中的 `CHATBOX-PLATFORM`、`CHATBOX-VERSION` 等头只在 `isChatboxAPI(url)` 成立时注入；自建网关不保证收到。[S36] | 桌面、Web、移动端分别请求自定义 OpenAI/Anthropic Base URL。 |
| Continue | 检查到的公共 fetch 合并调用方头和 `requestOptions.headers`，尚未确认贯穿各 provider 的专属 UA。[S37] | VS Code/JetBrains/CLI 的自定义 API 请求。 |
| LibreChat | 检查到的自定义 endpoint 支持配置请求头，尚未确认应用专属默认 UA。[S38] | 自定义 endpoint 的 Chat/Responses/Messages 请求。 |
| OpenClaw | 已检查 OpenRouter stream/provider 路径，尚未确认跨 provider 的专属 UA。[S39] | OpenAI 兼容、Responses、Anthropic 与插件路径分别采样。 |
| Cursor | 待补样；本轮没有确认可作为网关入站规则的官方 UA 格式。 | BYOK/自定义 Base URL 实际到达网关的头。 |
| Windsurf | 待补样；本轮没有确认可作为网关入站规则的官方 UA 格式。 | 自定义 API 路径实际到达网关的头。 |
| NextChat | 待补样；本轮没有核实其各部署方式的默认 UA。 | Web 直连与服务端转发分别采样。 |

## 已接入的记录与统计

1. **在入站处提取一次。** 鉴权通过后、限流前，从 `c.Request.Header` 提取 UA 与身份辅助特征，随后重试和上游头覆盖共用同一份快照。UA 最多记录 2048 字节，并过滤控制字符、保证 UTF-8 有效。
2. **分别保存应用与 SDK。** 日志 API 增加 `client` 对象，字段为 `id`、`name`、`version`、`sdk_id`、`sdk_name`、`sdk_version`、`user_agent`、`source`、`rule_version`。数据库用 `client_id` 索引聚合维度，用 `client_json` 保存快照。SDK 请求的应用仍是 `unknown`。
3. **保留来源和冲突。** `source` 区分 `user_agent`、`auxiliary_headers`、`originator`、`sdk_only`、`unknown`；`conflict` 与 `candidates` 保留标识冲突，UA 优先。`claude-cli` 请求增加 `ambiguous`，不假定实际软件一定是 Claude Code。
4. **按稳定 ID 聚合。** 仪表盘“客户端统计”按请求数排序，展示请求数、成功数、成功率、输入/输出 Token、成功请求费用和最近调用。支持本日、近 7 天、近 30 天和累计；点击客户端可查看同一时间范围内保留的调用明细。
5. **覆盖所有入站和结果。** 四种模型协议、音频、WebRTC 信令与 Realtime 计费轮次均保留身份。限流、非法模型、配额与请求格式等鉴权后提前失败也记日志；一次故障切换只记最终调用，Realtime 继续按现有每个终止事件的口径记录。模型发现、OPTIONS 和未通过鉴权的请求不计入模型调用。
6. **与日志保留共同提交。** 明细清理前按北京日期和客户端汇总进 `call_log_client_daily`，与原模型汇总、明细删除在同一事务内完成。升级时将已有 `call_log_daily` 中的历史数据一次性归入未知客户端；旧明细也保留为未知，`rule_version` 为 0，不推测历史身份。

### 管理 API

以下路径位于已配置后台入口的 `/api` 下，均需要管理员鉴权：

| 路径 | 说明 |
| --- | --- |
| `GET /clients` | 返回可筛选的客户端 ID 与名称，包含 `unknown`。 |
| `GET /client-stats?period=30d` | 返回 `items` 与 `summary`，范围支持 `today`、`7d`、`30d`、`all`，按北京时间自然日计算。 |
| `GET /logs?client_id=codex&client_version=1.2.3` | 按应用和精确版本筛选；也支持 `sdk_id`。原有 `q` 搜索扩展到客户端名称、版本、SDK 名称和 UA。 |
| `GET /logs?client_id=codex&export=csv` | 使用相同筛选导出，并追加客户端、SDK、UA、识别依据与歧义字段。 |

日志详情展示身份快照和 UA；旧日志与未提供 UA 的请求会明确显示未采集或未知。统计包含已归档明细，因此点击统计项后，日志列表的条数可能少于累计统计。

只保存客户端身份快照，不保存完整请求头、认证信息或会话 ID。后续若补采真实样本，保留 UA、必要的应用身份头、应用版本与 provider 路径即可。

## 官方依据

GitHub 链接固定到本轮检查的提交；npm 链接固定到所检查的发布版本。完整来源索引也保存在 JSON 中。

[S01]: https://github.com/openai/codex/blob/30587d78850abf2bfc484ffb0ab0f71497cc9c05/codex-rs/login/src/auth/default_client.rs
[S02]: https://www.npmjs.com/package/@anthropic-ai/claude-code/v/2.1.20
[S03]: https://github.com/anomalyco/opencode/blob/09399e3eec776400ea4f9f44e3b999c34a4e71ba/packages/core/src/app.ts
[S04]: https://github.com/anomalyco/opencode/blob/09399e3eec776400ea4f9f44e3b999c34a4e71ba/packages/core/src/session/model-request.ts
[S05]: https://github.com/anomalyco/opencode/blob/e3c4eb242585dcd77be1a867391b7c25fd47ce52/packages/opencode/src/session/llm/request.ts
[S06]: https://github.com/anomalyco/opencode/blob/e3c4eb242585dcd77be1a867391b7c25fd47ce52/packages/opencode/src/installation/index.ts
[S07]: https://github.com/google-gemini/gemini-cli/blob/a713410aba5d594594e20d5c822e3aab8f822089/packages/core/src/core/contentGenerator.ts
[S08]: https://github.com/cline/cline/blob/1619168bdeb570aa233d0f0a9af1d332d83a2816/sdk/packages/llms/src/providers/request-headers.ts
[S09]: https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/api/providers/constants.ts
[S10]: https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/api/providers/openai.ts
[S11]: https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/api/providers/anthropic.ts
[S12]: https://github.com/Kilo-Org/kilocode/blob/eb11204e8f146fb2b98676029b8d9a9f841c1f6c/packages/opencode/src/installation/index.ts
[S13]: https://github.com/Kilo-Org/kilocode/blob/eb11204e8f146fb2b98676029b8d9a9f841c1f6c/packages/opencode/src/session/llm/request.ts
[S14]: https://github.com/Kilo-Org/kilocode/blob/eb11204e8f146fb2b98676029b8d9a9f841c1f6c/packages/kilo-gateway/src/headers.ts
[S15]: https://github.com/Kilo-Org/kilocode/blob/eb11204e8f146fb2b98676029b8d9a9f841c1f6c/packages/kilo-gateway/src/api/constants.ts
[S16]: https://github.com/Kilo-Org/kilocode/blob/eb11204e8f146fb2b98676029b8d9a9f841c1f6c/packages/opencode/src/provider/provider.ts
[S17]: https://github.com/MoonshotAI/kimi-cli/blob/9ab1286b8fe4e6bcd116949a27ce5e0ac3389c82/src/kimi_cli/constant.py
[S18]: https://github.com/MoonshotAI/kimi-cli/blob/9ab1286b8fe4e6bcd116949a27ce5e0ac3389c82/src/kimi_cli/llm.py
[S19]: https://github.com/QwenLM/qwen-code/blob/7fc12989fd0ee65a4f96ea44776a6216a681906f/packages/core/src/core/openaiContentGenerator/provider/default.ts
[S20]: https://github.com/QwenLM/qwen-code/blob/7fc12989fd0ee65a4f96ea44776a6216a681906f/packages/core/src/core/openaiContentGenerator/provider/dashscope.ts
[S21]: https://github.com/QwenLM/qwen-code/blob/7fc12989fd0ee65a4f96ea44776a6216a681906f/packages/core/src/core/anthropicContentGenerator/anthropicContentGenerator.ts
[S22]: https://github.com/lobehub/lobe-chat/blob/599f13cbef20e6b75ffff6bfc9d0b185971e70aa/apps/desktop/src/main/utils/user-agent.ts
[S23]: https://github.com/CherryHQ/cherry-studio/blob/6e35bf4419f9434e013d396bad8fafc2fb596600/src/main/utils/http.ts
[S24]: https://github.com/CherryHQ/cherry-studio/blob/6e35bf4419f9434e013d396bad8fafc2fb596600/src/main/ai/utils/provider.ts
[S25]: https://github.com/CherryHQ/cherry-studio/blob/6e35bf4419f9434e013d396bad8fafc2fb596600/src/main/ai/provider/config.ts
[S26]: https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/llm.py
[S27]: https://github.com/open-webui/open-webui/blob/91ea4e3421d1d8069f13d4a463fcb5986d94ea60/backend/open_webui/utils/headers.py
[S28]: https://github.com/SillyTavern/SillyTavern/blob/06bde939fb1e9c4c8d8641d810f0a916b5bce127/src/constants.js
[S29]: https://github.com/SillyTavern/SillyTavern/blob/06bde939fb1e9c4c8d8641d810f0a916b5bce127/src/endpoints/backends/chat-completions.js
[S30]: https://github.com/lobehub/lobe-chat/blob/599f13cbef20e6b75ffff6bfc9d0b185971e70aa/packages/model-runtime/src/providers/openrouter/index.ts
[S31]: https://github.com/openai/openai-python/blob/81792ff040a2ca2b70348163049d3d69d5ba95cf/src/openai/_base_client.py
[S32]: https://github.com/openai/openai-node/blob/4c11b87c3e5e4fcddeeffc85b55502b2296f49fd/src/client.ts
[S33]: https://github.com/anthropics/anthropic-sdk-python/blob/9313f13a5a643ceed10fafba6785eab8e04b6e6e/src/anthropic/_base_client.py
[S34]: https://github.com/anthropics/anthropic-sdk-typescript/blob/5badc765f17d373405f300ee8e69e7f490b1f172/src/client.ts
[S35]: https://github.com/googleapis/python-genai/blob/92f72b3a236a5bc56e8e3327ab9563f77dd119c3/google/genai/_api_client.py
[S36]: https://github.com/chatboxai/chatbox/blob/0ac6385ae1ff5bf778777826da3c5edcd55b61b8/src/shared/request/request.ts
[S37]: https://github.com/continuedev/continue/blob/5522c6f44ca0ac3528b37244818fbfa39b5af470/packages/fetch/src/fetch.ts
[S38]: https://github.com/LibreChat-AI/LibreChat/blob/b793ab8bda7dd54ed766d6dd39013dadd85abfcb/packages/api/src/endpoints/custom/initialize.ts
[S39]: https://github.com/openclaw/openclaw/blob/279015b0578ee497695120b2c682a8154fc520ab/extensions/openrouter/stream.ts
