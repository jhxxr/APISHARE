# 上游渠道与协议矩阵

本文档说明 API Share 支持哪些入站协议、如何配置上游渠道，以及 Cline 免费模型渠道的细节。

## 客户端接入矩阵（入站）

| 协议格式 | 接入端点 | 推荐认证请求头 | 典型适配客户端 |
|---|---|---|---|
| **OpenAI Chat Completions** | `POST /v1/chat/completions` | `Authorization: Bearer sk-xxx` | OpenAI SDK、Chatbox、LobeChat、NextChat |
| **OpenAI Responses** | `POST /v1/responses` | `Authorization: Bearer sk-xxx` | 新版 OpenAI SDK、OpenAI Codex CLI |
| **OpenAI Embeddings** | `POST /v1/embeddings` | `Authorization: Bearer sk-xxx` | 各类 RAG 知识库检索客户端 |
| **OpenAI Audio** | `POST /v1/audio/speech`、`/v1/audio/transcriptions`、`/v1/audio/translations` | `Authorization: Bearer sk-xxx` | TTS、语音识别、音频翻译 |
| **OpenAI Realtime WebSocket** | `GET /v1/realtime?model={model}`（WebSocket Upgrade） | Bearer 或浏览器凭证子协议 | 双向实时音频、文本与工具事件 |
| **OpenAI Realtime WebRTC** | `POST /v1/realtime/calls`；兼容 `POST /v1/realtime?model={model}` | `Authorization: Bearer sk-xxx` | 浏览器 SDP 建连 |
| **Anthropic Messages** | `POST /v1/messages` | `x-api-key` 或 `Authorization` | Claude Code CLI、Anthropic SDK |
| **Google Gemini** | `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent` | `x-goog-api-key`、`Authorization` 或 `?key=` | Gemini CLI、google-genai SDK |
| **模型目录 / 详情** | `GET /v1/models`<br>`GET /v1/models/{model}` | OpenAI Bearer 或 Claude `x-api-key` | OpenAI SDK、Claude 客户端 |

## 上游请求协议矩阵（出站）

| 后台选项 | 管理 API 参数 | 请求生成端点 | 典型适用上游 |
|---|---|---|---|
| **OpenAI Chat Completions** | `type: "openai"`<br>`openai_protocol: "chat"` | `/v1/chat/completions` | OpenAI 官方、xAI Grok、DeepSeek、GLM、通义千问、vLLM、Ollama |
| **OpenAI Responses** | `type: "openai"`<br>`openai_protocol: "responses"` | `/v1/responses` | OpenAI 官方 Responses 端点或支持 Responses 协议的兼容商 |
| **Cline** | `type: "cline"`<br>`openai_protocol: "chat"` | 固定 Chat Completions，支持账号令牌刷新及免费模型发现 | Cline 账号 / 官方 API |
| **Anthropic Messages** | `type: "anthropic"` | `/v1/messages` | Anthropic 官方 Claude 端点 |
| **Gemini generateContent** | `type: "gemini"` | `:generateContent`<br>`:streamGenerateContent` | Google AI Studio 官方端点 |

## 路由匹配优先级

`pickUpstream` 按以下顺序选择上游：

原生格式 + 显式模型白名单 ＞ 转换格式 + 显式模型白名单 ＞ 原生格式 + 通配符 ＞ 转换格式 + 通配符。

如果需要精准控制特定模型路由，推荐在后台为上游配置准确的模型白名单。

## Cline 免费模型渠道

在「添加上游」选择 **Cline 免费模型**，保留默认地址 `https://api.cline.bot/api/v1`。选择代理后，点击 **登录 Cline 账号**，打开授权页并核对验证码，在浏览器登录自己的 Cline 账号；完成后网关自动将账号凭据填回当前编辑器，无需本地安装 CLI，也无需手动读取 Token。随后点击「获取模型」，勾选需要的模型并保存连接。支持粘贴单个账号 JSON（`{"refreshToken":"..."}` 或 `{"refresh_token":"..."}`）；可从已登录的 Cline 客户端凭据或 cline2api 导出账号中取得。需要代理时，在同一连接中选择代理，认证刷新、模型发现、正式转发和模型测试均使用该代理。

- **账号与 API Key 区别**：默认把凭据作为 RefreshToken，刷新后发送 `Authorization: Bearer workos:<accessToken>`。普通官方 API Key 需填成 `api-key:你的密钥`；已有短期访问令牌可填 `workos:你的访问令牌`，但短期令牌无法自动续期。Cline [官方免费模型说明](https://github.com/cline/cline/blob/main/docs/getting-started/free-models.mdx) 指出促销免费额度仅用于扩展和 CLI，普通 API Key 不享有该额度；本渠道账号协议参考 [cline2api](https://github.com/luawei1/cline2api)，实际可用性、免费额度和区域限制由 Cline 决定。
- **浏览器登录**：通过 WorkOS 设备授权和 Cline `/auth/register` 完成登录；由网关轮询授权，浏览器无需提供回调地址，服务器远程部署也可使用。授权状态仅当前管理员会话可读取，链接及凭据响应禁止缓存；可取消、重新发起登录，切换连接类型、地址、代理或手动修改凭据会停止等待，关闭编辑器也会取消。登录成功只填入当前编辑器，点击保存后才绑定到上游；登录凭据仍按下述规则保存在 SQLite。
- **手动授权与回填**：授权方式选择「手动授权并回填」，点击登录生成链接，可复制到其他浏览器打开。登录授权后，把完整的 `http://127.0.0.1:48801/auth?code=...` 回调地址或其中的授权码粘贴回来，点击「完成回填」。浏览器提示本地页面无法访问时，复制地址栏即可，不需要安装 CLI 或启动本地回调服务。网关通过官方 `/auth/token` 换取凭据，使用发起授权时选择的代理；授权码通常一次有效，失效后需重新授权。完整回调地址只在网关内解析，不会访问其中的网址。已取得的 RefreshToken 仍可直接填入账号凭据栏。
- **自动续期**：调用 `/auth/refresh`，在官方认证入口明确拒绝凭据时尝试 WorkOS 刷新；并发请求和复制的相同账号连接共享刷新锁。轮换后的 RefreshToken 和 AccessToken 保存在 SQLite 的 `cline_tokens` 表，服务重启及“获取模型”后不会丢失轮换结果；401 在输出正文前最多刷新重试一次。
- **模型发现**：账号模式读取 `/ai/cline/recommended-models` 的 `free` 数组，不使用写死的免费模型清单，沿用 5 分钟缓存。官方 API Key 模式读取 `/models` 的全部目录。获取模型代表凭据可刷新、目录可访问，具体模型能否调用需使用「测试模型」确认；模型白名单和别名映射沿用现有功能，可手动指定付费或订阅模型，但可能消耗账号额度。
- **客户端标识**：账号模式对齐参考项目的默认请求头，包括 `User-Agent: Cline/3.0.47`、`X-CLIENT-TYPE: cline-cli`、`X-CLIENT-VERSION`、`X-PLATFORM`、`X-CORE-VERSION`、`HTTP-Referer` 和 `X-Title`。宽泛透传不会覆盖这些标识，仍可用固定请求头显式修改；该兼容版本随程序内置，不自动跟随 npm。
- **请求与响应**：固定使用 Chat Completions，保留工具声明、工具历史和客户端参数，发送一致的 `session_id` / `X-Task-ID`。省略 `reasoning_effort: none`，将 `max_completion_tokens` 转为 `max_tokens`，低于 16 的正数输出预算提高到 16。JSON 和 SSE 中的 `{data: ...}` 完成响应会解包，跨格式请求继续由现有协议转换器处理。
- **会话隔离**：优先使用请求体 `session_id` / `metadata.session_id` / `conversation_id`，再取 `X-Task-ID` / `X-Session-Id` 或转换前的会话标识，缺失时取第一条用户消息指纹，并按网关 Key 和上游隔离。开头相同的独立会话请提供显式会话 ID。

参考实现：[认证与刷新](https://github.com/luawei1/cline2api/blob/main/auth.go)、[请求构造与响应处理](https://github.com/luawei1/cline2api/blob/main/proxy.go)、[动态免费模型目录](https://github.com/luawei1/cline2api/blob/main/models_sync.go)、[默认客户端请求头](https://github.com/luawei1/cline2api/blob/main/admin.go)。
