# 上游渠道与协议矩阵

本文档说明 API Share 支持哪些入站协议、如何配置上游渠道，以及 Cline 与 OpenCode 免费模型渠道的细节。

## 客户端接入矩阵（入站）

| 协议格式 | 接入端点 | 推荐认证请求头 | 典型适配客户端 |
|---|---|---|---|
| **OpenAI Chat Completions** | `POST /v1/chat/completions` | `Authorization: Bearer sk-xxx` | OpenAI SDK、Chatbox、LobeChat、NextChat |
| **OpenAI Responses** | `POST /v1/responses` | `Authorization: Bearer sk-xxx` | 新版 OpenAI SDK、OpenAI Codex CLI |
| **OpenAI Responses WebSocket** | `GET /v1/responses`（WebSocket Upgrade） | `Authorization: Bearer sk-xxx` | Codex 长连接、多轮增量续接；需原生 Responses WS 上游或 CLIProxyAPI 账号池 |
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
| **CLIProxyAPI 账号池** | `type: "cliproxy"` | 跟随客户端原生模型协议 | 内置 SDK 管理的 OAuth 账号池；渠道代理不用于回环连接，核心出站代理在「订阅账号 → 核心设置」配置，见 [使用说明](CLIPROXY.md#出站代理) |
| **OpenCode 免费模型** | `type: "opencode-free"`<br>`openai_protocol: "auto" / "chat" / "responses" / "anthropic"` | 自动发现免费模型，按模型选择 Chat、Responses 或 Messages 端点 | OpenCode Zen 免费模型，匿名或自有 Zen Key |
| **Anthropic Messages** | `type: "anthropic"` | `/v1/messages` | Anthropic 官方 Claude 端点 |
| **Gemini generateContent** | `type: "gemini"` | `:generateContent`<br>`:streamGenerateContent` | Google AI Studio 官方端点 |

## 路由匹配优先级

`pickUpstream` 按以下顺序选择上游：

1. **用户优先级**：只从当前可用候选中 `priority` 最小的一层选择。数值越小越优先（0 最高），建议主力 0、备用 1、应急 2；低优先级渠道仅在更高层全部不可用时承接流量。
2. **格式与白名单层级**（同一优先级内）：原生格式 + 显式模型白名单 ＞ 转换格式 + 显式模型白名单 ＞ 原生格式 + 通配符 ＞ 转换格式 + 通配符。
3. **权重**（同一优先级、同一格式层内）：按 `weight` 加权随机；权重 0 不参与调度。
4. **粘性会话**：同一会话在 affinity TTL 内保持原上游，不受上述重新调度影响，以保护上游 Prompt Cache。

如果需要精准控制特定模型路由，推荐在后台为上游配置准确的模型白名单；如果需要主备关系，把主力渠道设为较低的 `priority`。

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

## OpenCode 免费模型渠道

在「添加上游」选择 **OpenCode 免费模型**，保留默认地址 `https://opencode.ai/zen`，选择需要的代理后，点击「获取模型」，勾选模型并保存即可。无需安装 CLI，也无需选择 Zen / Go 套餐。API Key 可留空，网关默认使用匿名凭据 `public`；如需使用自己的 Zen Key，可以在同一连接中填写。编辑已有免费连接时留空保留凭据，填写 `public` 可切回匿名。

- **免费目录**：从官方目录识别免费模型，只展示免费条目；填写实际 Key 后仍只提供免费模型。正式调用和模型测试都会先检查映射后的模型 ID，手动白名单和别名不会绕过免费限制。目录短暂不可用时沿用缓存，已有目录中的 `-free` 条目可由端点文档补充识别。
- **模型名称**：Zen 免费版的 ID 为 `space-bunny-free`；`space-bunny` 是 Go 目录中的另一个模型，不会自动视为免费。客户端可继续使用自己的名称，在模型映射中配置 `{"space-bunny":"space-bunny-free"}`。请以「获取模型」返回的最新目录为准。
- **协议与代理**：默认自动识别模型原生协议，也可手动指定 Chat、Responses 或 Messages；免费资格仍由官方目录检查。目录刷新、模型获取、正式请求和模型测试使用所选代理；模型列表访问成功不代表某个推理请求一定被上游放行。
- **TLS / HTTP 指纹**：仅对官方 `https://opencode.ai` 的免费模型流量生效。`/zen` 使用 OpenCode 1.19.3 客户端格式，`/inference` 对齐 2.1.1 格式（版本头优先使用后台已检测的稳定版本，缺失时回退到 2.1.1）；两类请求都通过 Bun 兼容传输发送：TLS ClientHello 对齐 Bun/BoringSSL（JA4 `t13d1713h1`，仅 ALPN `http/1.1`，X25519MLKEM768 优先，无 GREASE），HTTP/1.1 头部按 Bun fetch 的顺序、拼写和默认值写出。`Content-Length` 在公共头块中自动计算，v2 的 `Accept: */*` 由运行时补齐；声明 `gzip, deflate, br, zstd` 后由网关解压。官方 `/zen/go`、付费模型、目录与文档请求仍走普通传输。
- **新版请求细节**：官方 `/inference` 的 Chat / Responses 路径分别为 `/inference/openai/v1/chat/completions`、`/inference/openai/v1/responses`，Messages 为 `/inference/anthropic/v1/messages?beta=true`。自定义网关保留配置的路径。v2 Chat 未指定输出上限时补充 `max_completion_tokens: 32000`，客户端显式指定的上限与工具 `strict` 设置优先；缺省工具标记补为 `strict: false`。请求体按客户端顺序编码，不转义 `<`、`>`、`&`，并保留大整数精度；shell 工具描述随网关宿主平台选择 bash / powershell。
- **出站代理**：直连模式沿用部署的 `HTTP(S)_PROXY` / `NO_PROXY`；选择代理后，HTTP、HTTPS、SOCKS5 与 SOCKS5h 均支持用户名密码认证，TLS 指纹流量也经同一代理出口，不会绕过代理直连。
- **工具兼容**：默认 `/zen` 入口保留客户端工具的参数定义，必要时把真实客户端工具临时命名为 `read`，同步改写工具选择和历史调用，并在 JSON / SSE 响应中还原名称。没有客户端工具时声明兼容工具，保留调用方原有的工具选择设置；若上游仍返回工具调用，在转发给客户端之前拦截，不会执行或透传这个客户端不存在的工具。官方网关 `/inference` 请求带有不兼容的客户端工具时自动使用兼容的 `/zen` 入口，保留原有工具能力；自建代理路径不会自动改写。网关不执行客户端工具。
- **会话与限流**：复用 OpenCode 客户端头、官方降序格式会话标识和会话持久化；保持对话亲和性。优先读取 OpenCode 会话头、Codex 的 `session_id` / `conversation_id` 头与请求体，以及 Claude Code `metadata.user_id` 中的 `_session_<uuid>`；客户端自带的合法官方会话原样保留。网关自行生成的会话和路由亲和都按 API Key 与对话起点隔离，开头相同的不同用户不会共享会话、上游 Key 或代理出口。`/zen` 的 `x-opencode-request` 使用按用户轮次固定的 `msg_` + 12 位 hex + 14 位 Base62，`/inference` 不发送该头；旧 `/zen` 持久化的不合法会话标识会在读取时原子修复。匿名请求按代理出口共享 429 冷却；相同实际 Key 在同一服务和套餐下共享冷却。遵守上游 `Retry-After`，诊断不更新生产冷却。
- **出口封禁**：上游 403 明确提示 IP 滥用或当前网络不可用时，不再切换 Zen / inference 重放；同一配置代理出口的官方免费模型连接共享至少 15 分钟冷却，较长的 `Retry-After` 优先。调度自动尝试其他可用出口或渠道；全部不可用时返回带 `Retry-After` 的 503 和出口封禁说明。其他出口、Go、付费模型与自建网关不受此冷却影响，后台「恢复」可清除出口冷却，模型测试不写入该状态。普通 403 客户端校验错误仍可在工具兼容时尝试另一官方入口一次，400 / 413 / 422 等请求错误直接返回。
- **升级提示**：免费流量改为 HTTP/1.1 后连接数会高于旧版；升级后网关会重新计算一次会话标识，进行中的对话可能切换上游 Key 或代理，上游 Prompt Cache 需要重新建立。
- **错误详情**：`403 FreeTierError` 表示上游拒绝当前免费请求，仍受其客户端校验、账号与区域策略影响。正式转发保留脱敏后的上游错误详情，并显示实际模型和请求路径；没有其他可用渠道时也不会只剩一个没有原因的 `403`。
- **旧配置兼容**：已有 `type: "opencode"` 的 Zen / Go 连接保留原套餐、Key、模型和转发方式，编辑器显示为「OpenCode · 旧配置」。新建入口使用 `opencode-free`；旧连接可改选「OpenCode 免费模型」，重新获取免费模型并保存。切换时默认使用匿名凭据，若需要自己的 Zen Key，请显式填写；Go 地址不能用于新的免费渠道。

免费模型、额度和访问策略由 OpenCode 决定。内置工具定义与 MIT 许可见 `cmd/apishare/third_party/opencode/`；Bun 兼容传输的 GPL-3.0-or-later 来源与修改说明见 `internal/bunhttp/NOTICE`。本次对齐 opencode2api 截至 2026-10-23 的 `57b906b`，相关变化包括 [2.1.1 路由](https://github.com/jasonxu114514/opencode2api/commit/f9ef5693ab58d4c2c03f63329399732f8b0e9007)、[Chat 请求体](https://github.com/jasonxu114514/opencode2api/commit/5c5100c47076f675712b50d36fd5aa27833d3216)、[HTTP 头顺序](https://github.com/jasonxu114514/opencode2api/commit/4e2edbf1e678ab6e770c056a9bf5c4b028fac5d3) 和 [出口封禁及重试](https://github.com/jasonxu114514/opencode2api/commit/0bb23c7a7f0b7cbe08c475e7148c56bc477abcf9)。另见 [OpenCode 官方模型目录](https://opencode.ai/v2/docs/console/models/) 与 [会话编码修复](https://github.com/jasonxu114514/opencode2api/pull/70)。
