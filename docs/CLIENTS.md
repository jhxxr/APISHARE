# 客户端接入指南

API Share 同时提供 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 与 Google Gemini 入站端点。客户端使用网关签发的 API Key 即可接入，由网关完成协议转换与上游路由。

调用日志会记录客户端、版本与 User-Agent，仪表盘提供按客户端统计的请求数、成功率、Token 和费用；详见 [客户端识别、统计与 User-Agent 样本](CLIENT_USER_AGENTS.md)。

## OpenAI Codex CLI

后台将仅支持 Chat Completions 的上游（如 DeepSeek、GLM、本地 vLLM）设置为 `OpenAI Chat Completions`（`openai_protocol: "chat"`）。Codex 客户端直接以原生 `responses` 格式连接网关，由网关在内存中实时转换工具调用与流式增量。

修改 Codex 的 `config.toml` 配置：

```toml
model = "deepseek-v4-flash"
model_provider = "apishare"
web_search = "disabled"   # 普通 Chat 上游不支持服务端搜索，必须显式禁用

[model_providers.apishare]
name = "API Share"
base_url = "https://your-gateway.example/v1"
env_key = "APISHARE_API_KEY"
wire_api = "responses"
requires_openai_auth = false
```

配置环境变量后启动：

```bash
export APISHARE_API_KEY="sk-your-apishare-key"
codex
```

### Responses WebSocket 状态

Codex 支持 Responses WebSocket，自定义 provider 的能力字段为 `supports_websockets`。当前 API Share 外层仅提供 `POST /v1/responses`，尚未接入其 WebSocket Upgrade；以上配置继续使用 HTTP。内置 CLIProxyAPI 虽然提供 Responses WS，仍需要外层代理、逐轮鉴权、限流与计费配套接通，之后才能对对应 provider 声明 `supports_websockets = true`。核查结果与接入边界见 [性能优化文档](PERFORMANCE.md)。

## Anthropic Claude Code

将 Claude Code 直接指向 API Share 网关端点，即使后端挂载的是 OpenAI 或 Gemini 上游，也能顺畅运行：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_API_KEY="sk-your-apishare-key"
claude
```

Claude Code 会独立发起 Haiku 后台请求；将 Haiku 别名映射到其他模型时，也需要上游支持该请求使用的功能。对于 `output_config.format`（兼容旧版 `output_format`）中的 JSON Schema，网关会转换为目标 Chat 的 `response_format.json_schema`、Responses 的 `text.format` 或 Gemini 的 `generationConfig.responseJsonSchema`，不再一律拒绝。

### 结构化输出转换

在上游编辑器的「结构化输出转换」中选择：

| 选项 | 管理 API 的 `structured_output` | 行为 |
| --- | --- | --- |
| 保留结构约束（默认） | `""` 或 `"json_schema"` | 将完整 Schema 转换为上游对应参数；上游必须支持该功能及所用 Schema。 |
| JSON 兼容模式（校验后返回） | `"json_object"` | 仅用于支持 JSON 模式的 Chat / Responses 上游。发送 JSON 模式及 Schema 提示，验证结果后再返回。要求 Schema 根类型明确为 `object`。 |
| 不支持跨格式结构化输出 | `"disabled"` | 在本地跳过该转换候选，继续寻找可用上游，不消耗网络重试次数。 |

DeepSeek 官方 Chat 文档提供 `json_object`，Responses 文档提供 `json_schema`；WorkBuddy 等第三方入口需以实际支持情况为准。对于只支持 JSON 模式的 DeepSeek Chat 入口，选择「JSON 兼容模式」；若已经验证入口支持 Schema，则保留默认设置。不同模型能力不同，可拆分为不同上游配置。连接测试仅检查连接和模型列表，不代表已验证结构化输出能力。

跨格式的结构化响应会在完整接收并通过 JSON Schema 校验后交付；流式 Messages 客户端等待期间继续收到心跳，普通流式任务仍实时转发。正常结束但不符合 Schema 的结果不会当作成功返回，可按既有重试设置切换其他候选；工具调用、明确拒绝和达到 token 上限仍保留各自的协议语义。原生同协议请求保留原有透传行为。这项设置不改变上游端点，也不会静默忽略 Schema；本地校验不会读取 Schema 指向的外部文件或 URL。

参考：[Claude 结构化输出](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)、[DeepSeek JSON 模式](https://api-docs.deepseek.com/guides/json_mode/)、[DeepSeek Responses](https://api-docs.deepseek.com/api/create-response/)。

## OpenAI SDK / 兼容客户端

### Python

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-your-apishare-key",
    base_url="http://127.0.0.1:8080/v1"
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True
)
for chunk in response:
    print(chunk.choices[0].delta.content or "", end="")
```

### cURL 测试指令

```bash
# OpenAI Chat 格式
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-apishare-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# Claude Messages 格式 (支持 X-Session-Id 锁定粘性)
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: sk-your-apishare-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "X-Session-Id: session-user-12345" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-3-7-sonnet-20250219","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}'

# Google Gemini 格式
curl "http://127.0.0.1:8080/v1beta/models/gemini-2.5-flash:generateContent" \
  -H "x-goog-api-key: sk-your-apishare-key" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}'
```

## 音频与 Realtime

在后台添加 `type: "openai"` 上游，将音频 / Realtime 模型加入上游和客户端 Key 的模型白名单即可，无需新增协议类型。`openai_protocol` 的 Chat / Responses 选项不改变这些独立端点；上游必须原生支持对应接口，音频不会转换为 Claude / Gemini 文本请求。模型别名、独立 HTTP(S) / SOCKS 代理、动态请求头和建连前故障切换均可使用。

服务端 WebSocket 连接示例（替换地址、模型及 Key）：

```javascript
import WebSocket from "ws";

const socket = new WebSocket("wss://gateway.example/v1/realtime?model=voice-alias", {
  headers: { Authorization: `Bearer ${process.env.APISHARE_API_KEY}` },
});
socket.on("open", () => socket.send(JSON.stringify({
  type: "session.update", session: { type: "realtime", instructions: "请用中文回答。" },
})));
socket.on("message", data => console.log(data.toString()));
```

浏览器原生 `WebSocket` 可使用 `["realtime", "openai-insecure-api-key." + apiShareKey]` 子协议鉴权；网关仅向客户端协商 `realtime`，使用上游自己的 Key 建连。可选 `OpenAI-Beta: realtime=v1` 请求头用于 beta 协议。每条 WebSocket 在整个连接期间占用一个 Key 并发槽，连接内不能切换模型；已建立的会话不会重试到其他上游。网关按不同 `response.done.response.id` 的真实 usage 逐轮记账，自动去重；每轮结束以及显式创建响应、提交音频或更新会话时重新检查 Key 权限与额度，额度耗尽使用关闭码 `1008` 结束会话。没有上游 usage 的事件不会按文本猜测音频 Token。

WebRTC 推荐统一建连方式：

```javascript
const form = new FormData();
form.set("sdp", offer.sdp); // RTCPeerConnection 创建的本地 SDP
form.set("session", JSON.stringify({ type: "realtime", model: "voice-alias" }));
const response = await fetch("https://gateway.example/v1/realtime/calls", {
  method: "POST", headers: { Authorization: `Bearer ${apiShareKey}` }, body: form,
});
if (!response.ok) throw new Error(await response.text());
await pc.setRemoteDescription({ type: "answer", sdp: await response.text() });
```

也支持 `Content-Type: application/sdp` 配合 `?model=voice-alias` 的建连请求。SDP、上传音频和其他 multipart 字段保持原样，仅按上游模型映射改写模型字段；上传总请求体上限为 32 MiB。语音合成的二进制响应及转录 SSE 增量直接转发。

**计费边界：** WebRTC 音频和数据通道直连上游，网关只记录 SDP 建连，不掌握后续 usage，因此不能对通话逐轮扣费、限制通话并发或在额度耗尽后结束直连通话。需要完整网关计费与会话配额控制时使用 WebSocket。当前不提供临时上游密钥签发、`call_id` sideband 和通话控制 API；也不转发上游的 `Location`。HTTP 音频接口仅按上游返回的 Token usage 记账；MP3 / WAV、纯文本和仅报告时长的结果没有 Token usage 时，记录请求但不按字符数或时长猜测费用。

自动价格同步支持音频单价；手动价格 JSON 可增加 `in_audio`、`out_audio`、`cache_read_audio`（USD / 1M tokens），分别用于音频输入、输出和缓存读取，后台日志会展示对应的用量与费用。未配置专用音频单价时回退到普通输入、输出、缓存读取单价。

实现参考 [sub2api 的 WebSocket passthrough relay（固定版本）](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/openai_ws_v2/passthrough_relay.go) 中的双向转发与终止事件用量处理；音频接口依照 [OpenAI WebSocket 文档](https://developers.openai.com/api/docs/guides/realtime-websocket) 和 [WebRTC 文档](https://developers.openai.com/api/docs/guides/realtime-webrtc) 实现。
