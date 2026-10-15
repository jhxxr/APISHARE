<p align="center">
  <img src="cmd/apishare/web/public/brand/apishare-icon-v2.png" alt="API Share Logo" width="128" height="128" />
</p>

<h1 align="center">API Share</h1>

<p align="center">
  <strong>轻量级 · 全协议双向互转 · 会话粘性 · 零外部依赖的现代化 AI 分发网关</strong>
  <br />
  <em>Universal AI Routing Gateway with Bi-directional Protocol Conversion, Prompt Cache Stickiness & Zero External Dependencies</em>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg?style=flat-square" alt="License"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go Version"></a>
  <a href="https://github.com"><img src="https://img.shields.io/badge/Docker-GHCR-2496ED?style=flat-square&logo=docker" alt="Docker Support"></a>
  <a href="https://sqlite.org/"><img src="https://img.shields.io/badge/Storage-Pure_Go_SQLite_(WAL)-003B57?style=flat-square&logo=sqlite" alt="SQLite"></a>
  <img src="https://img.shields.io/badge/Platform-Linux%20|%20macOS%20|%20Windows-lightgrey?style=flat-square" alt="Platform">
  <img src="https://img.shields.io/badge/PRs-Welcome-brightgreen.svg?style=flat-square" alt="PRs Welcome">
</p>

<p align="center">
  <a href="#-核心特性">✨ 核心特性</a> •
  <a href="#️-架构与协议矩阵">🏗️ 架构与协议</a> •
  <a href="#-快速开始">🚀 快速开始</a> •
  <a href="#-常用客户端接入">🔌 客户端接入</a> •
  <a href="#-跨协议转换引擎">🔄 协议转换引擎</a> •
  <a href="#️-进阶功能与配置">⚙️ 进阶配置</a> •
  <a href="#-环境变量参考">📜 环境变量</a> •
  <a href="#-开源协议与致谢">📄 许可证</a>
</p>

---

## 📖 项目简介

**API Share** 是一个用 Go 语言编写的高性能、轻量级 AI 分发网关。它统一接入 **OpenAI Chat Completions**、**OpenAI Responses (Codex)**、**Anthropic Claude**、**Google Gemini** 以及各类 **OpenAI 兼容协议** 上游。客户端通过网关签发的统一 API Key 发起调用，由网关在毫秒级内完成请求鉴权、智能计费、限流控频、会话粘性路由、故障转移与全协议双向动态转换。

> [!NOTE]
> **官方规范与合规定位**：本项目只对接**官方 API 上游**（OpenAI、Anthropic、Gemini、xAI Grok、DeepSeek、GLM、Qwen 或私有部署的 vLLM / Ollama 等标准端点），不包含任何“网页订阅逆向转 API”、TLS 浏览器指纹伪装或高危轮换代理逻辑，专注为生产级开发与自动化提供纯粹、稳定、合规的转发底座。

---

## ✨ 核心特性

- ⚡ **零外部依赖 · 极速单二进制**
  纯 Go 编写（内置现代化纯 Go SQLite WAL 驱动 `modernc.org/sqlite` 与 `go:embed` 静态前端），无须部署 Redis、MySQL 或 Nginx 即可独立稳定运行，开箱即用。
- 🔄 **全协议任意双向互转 (Universal Protocol Converter)**
  打破厂商生态壁垒：支持 OpenAI Chat、OpenAI Responses、Anthropic Messages、Google Gemini 四大格式的任意入站与出站互相转换。例如：**使用 Claude Code 驱动 DeepSeek / Gemini**，或**使用 OpenAI Codex 驱动常规 Chat Completions 模型**。
- 🎯 **智能会话粘性 (Prompt Cache 命中优化)**
  根据会话特征或请求头将同一轮对话精准绑定到同一物理上游，大幅提升 Claude、Gemini 及 OpenAI 的 Prompt Cache 命中率，降低最高 80%+ 的推理延迟与成本。
- ⚖️ **加权调度与弹性熔断 (Fault Tolerance & Failover)**
  多上游按权重分流，在上游遭遇 `401 / 403 / 408 / 429 / 5xx / 529` 或网络波动时毫秒级透明重试下一个可用上游；连续故障触发指数退避冷却（30s ~ 10m），成功后自动解除。
- 💰 **官方牌价自动对齐与多级配额**
  启动及每日定时自动同步 **LiteLLM 官方定价镜像库**，告别繁重的人工维护；支持每 Key 独立 QPS 令牌桶与并发控制，独创 **“共享额度池” 与 “模型独立专属额度”** 混合配置。
- 🛡️ **上游独立代理池与动态 Header 伪装**
  每个上游可单独绑定 HTTP / HTTPS / SOCKS5 / SOCKS5h 代理；支持基于正则匹配的请求头改写与客户端请求头透传；内置自动定时同步官方 Codex CLI 与 Claude Code 最新稳定版 User-Agent 变量。
- 📊 **现代化公开展现与后台控制台**
  - **公开首页 `/`**：滚动 24 小时真实请求 Uptime（分钟/小时粒度）、全站 Token 滚轮动效、模型调用趋势，并提供基于 HttpOnly Cookie 的免登录安全额度查验。
  - **管理控制台**：首启进入 `/setup/` 初始化，支持一键热重命名安全后台路径（如 `/secret-portal`），全可视化管理密钥、模型白名单、代理、公告与详细审计日志。
  - **调用速度指标**：每条调用记录显示**首字延迟**（请求发出到首个输出增量）、**总耗时**（覆盖流式输出的完整转发时间）与**输出 TPS**（输出 Token ÷ 实际生成时长），与耗时列区分开，便于直接对比上游响应速度。
  - **自定义模型测试**：在上游列表点击“测试模型”，选择或输入模型，默认发送 `hi`，也可填写自己的多行测试消息；支持恢复默认消息、调整最大输出 Token，查看回复、思考内容、耗时和用量。测试直接使用该上游保存的协议、模型映射、代理及请求头，不经过其他上游；每次测试为独立请求，可能产生上游费用。

---

## 🏗️ 架构与协议矩阵

### 整体架构流转

```mermaid
flowchart TD
    subgraph Clients["💻 客户端接入 (Inbound)"]
        C1["OpenAI SDK / Chatbox<br><code>/v1/chat/completions</code>"]
        C2["OpenAI Codex CLI / Responses<br><code>/v1/responses</code>"]
        C3["Claude Code / Anthropic SDK<br><code>/v1/messages</code>"]
        C4["Google Gemini CLI / SDK<br><code>/v1beta/models/...</code>"]
    end

    subgraph Gateway["⚡ API Share Gateway (Single Binary)"]
        direction TB
        Auth["🔑 密钥鉴权 & 限流 (QPS / 并发信号量)"]
        Sticky["🎯 会话粘性提取 (Session-Id / 对话指纹)"]
        Router["⚖️ 路由决策 (格式匹配 > 权重 > 熔断过滤)"]
        Engine["🔄 协议双向转换引擎 (Chat ⇋ Responses ⇋ Claude ⇋ Gemini)"]
        ProxyHead["🛡️ 独立代理池 (SOCKS5/HTTP) & 动态 Header 注入"]
        Billing["💰 用量统计与计费扣除 (LiteLLM 官方价格库)"]

        Auth --> Sticky --> Router --> Engine --> ProxyHead --> Billing
    end

    subgraph Upstreams["🌐 目标上游服务 (Outbound)"]
        U1["OpenAI 官方 API"]
        U2["Anthropic Claude"]
        U3["Google Gemini"]
        U4["OpenAI 兼容商<br>(DeepSeek / Grok / GLM / Qwen / vLLM)"]
    end

    Clients ==> Gateway
    ProxyHead ==> Upstreams
```

### 1. 客户端接入矩阵 (入站 Inbound)

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

### 2. 上游请求协议矩阵 (出站 Outbound)

| 后台选项 | 管理 API 参数 | 请求生成端点 | 典型适用上游 |
|---|---|---|---|
| **OpenAI Chat Completions** | `type: "openai"`<br>`openai_protocol: "chat"` | `/v1/chat/completions` | OpenAI 官方、xAI Grok、DeepSeek、GLM、通义千问、vLLM、Ollama |
| **OpenAI Responses** | `type: "openai"`<br>`openai_protocol: "responses"` | `/v1/responses` | OpenAI 官方 Responses 端点或支持 Responses 协议的兼容商 |
| **Anthropic Messages** | `type: "anthropic"` | `/v1/messages` | Anthropic 官方 Claude 端点 |
| **Gemini generateContent** | `type: "gemini"` | `:generateContent`<br>`:streamGenerateContent` | Google AI Studio 官方端点 |

> [!TIP]
> **路由匹配优先级规则（`pickUpstream`）**：
> 原生格式 + 显式模型白名单 ＞ 转换格式 + 显式模型白名单 ＞ 原生格式 + 通配符 ＞ 转换格式 + 通配符。如果需要精准控制特定模型路由，推荐在后台为上游配置准确的模型白名单。

---

## 🚀 快速开始

### 方式一：Docker 一键部署 (推荐)

项目预构建的多架构镜像发布在 GitHub Container Registry (GHCR)，支持 `linux/amd64` 与 `linux/arm64`：

```bash
docker run -d \
  --name apishare \
  --restart unless-stopped \
  -p 8080:8080 \
  -v apishare-data:/data \
  ghcr.io/<owner>/<repo>:latest
```

或使用 `docker-compose.yml` 组织：

```yaml
version: "3.8"
services:
  apishare:
    image: ghcr.io/<owner>/<repo>:latest
    container_name: apishare
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
    environment:
      - PORT=8080
      - API_DB=/data/apishare.db
```

### 方式二：本地编译运行

环境要求：**Go 1.26.2+**。由于采用纯 Go 实现的 SQLite，无需任何 CGO 编译依赖环境。

```bash
# 1. 克隆代码库
git clone https://github.com/<owner>/<repo>.git && cd apishare

# 2. 编译为单二进制文件
# Linux / macOS
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o apishare ./cmd/apishare
PORT=8080 API_DB=apishare.db ./apishare

# Windows (PowerShell)
go build -trimpath -ldflags="-s -w" -o apishare.exe ./cmd/apishare
$env:PORT = "8080"; $env:API_DB = "apishare.db"; .\apishare.exe
```

### 首次初始化步骤 (首次引导)

1. **设置管理员密码与后台路径**：浏览器打开 `http://127.0.0.1:8080/`，系统检测到空数据库将自动进入独立的 `/setup/` 初始化向导。设置高强度管理员密码（至少 12 位）及自定义管理后台路径（默认 `/admin`，支持随时热更名），保存后配置即刻写入 SQLite。
2. **添加上游服务**：进入后台「上游服务」页面，选择上游实际支持的接口类型（OpenAI Chat、OpenAI Responses、Anthropic 或 Gemini），填入 Base URL（如 `https://api.deepseek.com`）、上游 API Key、权重与**模型白名单**。
3. **签发访问密钥 (API Key)**：进入「API 密钥」页面创建访问令牌，绑定允许调用的模型列表，分配共享额度或各模型独立专属额度（USD，0 为无限额）。
4. **价格与定价检查**：进入「设置」页面检查 LiteLLM 官方价格库同步状态（支持手动立即刷新），亦可根据需要手动配置单模型自定义价格覆盖。

---

## 🔌 常用客户端接入

### 1. OpenAI Codex CLI 接入

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

### 2. Anthropic Claude Code 接入

将 Claude Code 直接指向 API Share 网关端点，即使后端挂载的是 OpenAI 或 Gemini 上游，也能顺畅运行：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_API_KEY="sk-your-apishare-key"
claude
```

### 3. OpenAI SDK / 兼容客户端

#### Python

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

#### cURL 测试指令

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

---

### 4. 音频与 Realtime 接入

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

## 🔄 跨协议转换引擎

API Share 实现了业界领先的高保真度协议转换层，具备针对复杂工程级场景的深度健壮性保证：

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

### 深度保真特性

1. **Codex Responses 自由文本与工具转换**
   - 完美适配 Codex 客户端特有的 `custom` 自由文本工具（如 `apply_patch`）、`namespace` 命名空间以及客户端模式的 `tool_search`。
   - 跨格式转发时自动将自由文本安全打包为标准函数入参，并在回程流式下发时完整恢复 `custom_tool_call`、原始命名空间、工具搜索结果与对应 `call_id`。
2. **多工具调用与历史严格配对**
   - 并行工具调用历史按实际批次自动配对，严格清理取消、重连后无结果的残缺调用；重复的调用 ID 分组配对隔离，防止上下文混乱。
3. **Claude Code 复杂系统指令兼容**
   - 自动识别并合并 Claude Code 在 Anthropic `messages[]` 内部动态插入的纯文本 `role: "system"`，按序聚合到目标接口系统指令，同时避免扰乱会话指纹。
4. **全模态视觉图片规范化互转**
   - 统一转译 OpenAI URL/data URL、Anthropic URL/Base64、Gemini inlineData/fileData；针对 Responses 工具执行返回的结构化截图及数据，自动规范重构为目标模型的图片消息块。
5. **SSE 流式断流保护与首包回滚**
   - 上游流式请求默认携带 `Accept-Encoding: identity`，消除代理压缩器导致的推理 SSE 积压延迟；
   - 在首次向客户端输出实质性文本、推理思考或工具增量前，若遭遇上游断流或错误，**不消耗客户端重试预算，静默无缝切换到备用上游**；一旦产生实质输出后遇错则返回标准失败事件，严格避免工具重复执行。
6. **深度思考 / Reasoning 隔离与档位自适应**
   - 支持 Chat `reasoning_effort`、Responses `reasoning.effort` 与 Claude `output_config.effort` 的双向自适应映射（如 DeepSeek `xhigh -> max`, `medium -> high`）。
   - 思维链明文缓存机制：针对返回的推理内容按 `Key + 客户端模型 + 上游 ID + 映射模型` 独立空间缓存（LRU 1024 项 / 8 MiB，TTL 7天），多轮对话精准回放，杜绝密文伪装与跨租户泄露。

---

## ⚙️ 进阶功能与配置

### 1. 🎯 会话粘性与 Prompt Cache 优化
多轮对话路由到不同实例会导致上游的 KV Cache（Prompt Cache）彻底失效。API Share 采用双轨识别机制：
1. **显式请求头**：客户端传入 `X-Session-Id` 或 `X-Conversation-Id`；
2. **对话前缀指纹**：自动提取 `hash(端点 + 模型 + system指令 + 首条user消息)` 动态计算指纹。

粘性关系存入 SQLite，具备**滑动过期机制**（默认 30 分钟，有调用自动顺延），服务重启不丢失。当粘性节点故障时，网关透明故障转移到新上游并自动刷新绑定，节点恢复后不会发生不必要的抖动回切。

### 2. 💰 智能计费与多级配额策略
- **计费优先级**：`后台手动覆盖价格 > LiteLLM 官方牌价库 > 网关内置兜底表 > default (0)`。
- **准确 Usage 采集**：原生捕获 OpenAI `include_usage`、Anthropic `message_delta`、Gemini `usageMetadata` 与 Responses `response.completed`；若遇到个别不返回用量的上游，网关自动调用内置 `tiktoken-go` 进行本地分词计算。
- **分项价格与费用明细**：普通输入、缓存读取/创建、明确的 1 小时缓存创建、正文和推理分别计算。手动价格可选 `cache_read`、`cache_write`、`cache_write_1h`、`reasoning`，缺省继承输入/输出单价，明确填 0 表示免费；自动同步保留价格库中的分项。新日志保存当次价格和费用快照，扣费与日志在同一事务提交。长上下文分档、托管工具、存储及分模态价格仍需专项适配，详见[计费边界](docs/SUB2API_BILLING_AUDIT.md)。
- **速度指标**：日志的速度列记录 `first_token_ms`（首字延迟，仅流式实际下发增量时才有值）、`duration_ms`（总耗时，覆盖完整流式转发）与 `latency_ms`（上游响应头耗时，保留原有含义）。输出 TPS 由输出 Token 与实际生成时长（总耗时减首字）推导；非流式请求没有首字，按总耗时估算。历史日志缺少新列，总耗时回退显示原有的响应头耗时，首字与 TPS 显示为“—”，不会伪造速度数据。
- **共享池 vs 独立配额**：
  在后台可针对密钥设置基础共享额度（如 100 USD），同时对特定昂贵模型（如 `o3-mini`、`claude-3-7-sonnet`）设置专属独立额度（如 20 USD）。独立额度仅消耗自身配额，互不挤占，用尽后返回 `402 Payment Required`。

### 3. 🌐 独立上游代理池 (Proxy Pool)
每个上游可独立配置代理网络，支持 `HTTP`、`HTTPS`、`SOCKS5`、`SOCKS5h`（代理解析域名）及鉴权。连接测试、模型拉取、常规请求与流式传输均严格遵守所选代理通道，彻底解除服务器网络受限问题。

### 4. 🎭 动态请求头与客户端版本同步
在后台每个上游均可配置请求头覆盖规则（支持正则透传、变量注入）：

```json
{
  "*": true,
  "re:^X-Trace-.*$": true,
  "X-Custom-Env": "production",
  "User-Agent": "{claude_code_user_agent}",
  "Authorization": "Bearer {api_key}"
}
```

网关后台内置自动化守护器，定期从 npm 官方仓库同步获取 `@openai/codex` 和 `@anthropic-ai/claude-code` 的最新稳定版本，通过 `{codex_user_agent}` 与 `{claude_code_user_agent}` 占位符自动注入规范 User-Agent，防止因客户端版本过旧被上游拒绝。

### 5. 模型标签与模型映射
在上游编辑器的「模型访问」中输入模型名，按回车、逗号或粘贴多行即可生成可移除的标签，重复名称自动去重；也可以获取当前 Key 的模型后勾选。白名单留空允许全部模型。

模型映射支持可视化逐行编辑、JSON 切换和批量添加。例如 `{"chat-fast":"deepseek-chat"}` 表示客户端请求 `chat-fast` 时，该上游收到 `deepseek-chat`。映射中的请求名称自动参与模型发现和显式路由，无需再手动加入上游白名单；API Key 的模型权限和配额仍按客户端请求名称检查。每条规则只替换一次，保存后立即生效。批量添加每行填写 `请求模型 = 上游模型`，空名称、重复规则和无效 JSON 会提示错误。

---

## 📜 环境变量参考

所有环境变量均具备合理的生产默认值，按需覆盖即可：

| 环境变量 | 默认值 | 作用说明 |
|---|---|---|
| `PORT` | `8080` | 服务监听端口 |
| `API_DB` | `apishare.db` | SQLite 数据库文件存放路径（容器内为 `/data/apishare.db`） |
| `ADMIN_PATH` | `admin` | 首次配置向导设定的默认后台路径（首启后以数据库中的配置为准） |
| `UPSTREAM_MAX_IDLE_CONNS` | `256` | 连接池最大空闲连接总数 |
| `UPSTREAM_MAX_IDLE_CONNS_PER_HOST` | `64` | 每个目标主机保留的最大空闲连接数 |
| `UPSTREAM_DIAL_TIMEOUT_SEC` | `15` | TCP 建连与 TLS 握手超时时间（秒） |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `180` | 等待上游响应首包头部的最长超时（秒，0 为不限） |
| `UPSTREAM_IDLE_TIMEOUT_SEC` | `300` | 等待上游数据包的空闲超时（秒，不限制流式持续总长，0 为不限） |
| `REALTIME_MAX_DURATION_SEC` | `3600` | Realtime WebSocket 单连接最长存活时间（秒，1～86400）；空闲读取另受 `UPSTREAM_IDLE_TIMEOUT_SEC` 限制 |
| `TIKTOKEN_CACHE_DIR` | 临时目录/`data-gym-cache` | 本地 Token 分词词表缓存路径 |

---

## 🛠️ 本地联调与开发测试

### 目录结构

```text
.
├── cmd/
│   ├── apishare/          # 网关主程序与 Go 测试
│   │   └── web/           # 编译时内嵌的管理后台和公开页面
│   └── mockupstream/      # 多协议 Mock 上游
├── docs/                  # 协议兼容与计费排查文档
├── .github/workflows/     # 自动构建与镜像发布
├── Dockerfile
├── go.mod
├── go.sum
├── LICENSE
└── README.md
```

以下命令均在仓库根目录执行；Go 模块文件保留在根目录，主程序入口为 `./cmd/apishare`。

代码库内置了多协议 Mock 测试桩 `cmd/mockupstream`，可一次性模拟四大协议服务端，方便脱机验证转换逻辑与计费：

```bash
# 启动两个独立的假上游示例
go run ./cmd/mockupstream -addr :18081 -name mockA
go run ./cmd/mockupstream -addr :18082 -name mockB

# 执行完整的测试套件
go test -v ./...

# 运行真实的 Codex CLI 协议回路测试 (需本地已安装 codex)
export APISHARE_CODEX_BINARY="codex"
go test ./cmd/apishare -run TestCodexCLIResponsesToolLoop -v -count=1
```

---

## 🗺️ 路线图 (Roadmap)

- [x] 全协议覆盖（OpenAI Chat、Responses、Claude Messages、Gemini）
- [x] 基于指纹与会话 ID 的滑动窗口 Prompt Cache 会话粘性
- [x] LiteLLM 官方价格库每日自动同步
- [x] SOCKS5 / HTTP 独立代理池与动态 Header 模板
- [x] 现代化公开展现主页（分钟级 24H Uptime 仪表盘、Token 滚轮动画）
- [x] 独立模型专属配额与共享额度混合结算
- [x] 客户端真实 User-Agent 自动检测与无缝同步
- [x] 多模态音频 / 实时语音（Audio HTTP、Realtime WebSocket 双向代理、WebRTC SDP 建连；计费边界见上文）
- [ ] 分布式集群模式与 SQLite 异地自动备份同步

---

## 🤝 参与贡献

热烈欢迎提交 Issue 与 Pull Request！
1. Fork 本仓库并创建特性分支 (`git checkout -b feature/amazing-feature`)
2. 提交代码更改并确保通过所有单元测试 (`go test ./...`)
3. 提交 Commit (`git commit -m 'feat: add amazing feature'`)
4. 推送分支 (`git push origin feature/amazing-feature`)
5. 新建 Pull Request

---

## 📄 开源协议与致谢

- 本项目基于 **[MIT License](LICENSE)** 开源。
- 前端内嵌的 Space Grotesk 和 Orbitron 字体分别遵循其各自的 [SIL Open Font License](cmd/apishare/web/public/fonts/OFL.txt)。
- **技术致谢**：感谢 [LiteLLM](https://github.com/BerriAI/litellm) 提供的全网模型官方价格规范；感谢 [New API](https://github.com/QuantumNous/new-api) 与 [Sub2API](https://github.com/Wei-Shaw/sub2api) 在多格式协议兼容设计上给予的灵感与先驱探索。

<div align="center">
  <sub>Built with ❤️ by the API Share Open Source Community.</sub>
</div>
