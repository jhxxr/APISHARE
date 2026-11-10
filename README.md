<p align="center">
  <img src="cmd/apishare/web/public/brand/apishare-icon-v2.png" alt="API Share Logo" width="128" height="128" />
</p>

<h1 align="center">API Share</h1>

<p align="center">
  <strong>轻量级 AI 分发网关 · 全协议双向转换 · 会话粘性 · 单二进制零外部依赖</strong>
  <br />
  <em>Universal AI routing gateway with bi-directional protocol conversion and prompt-cache stickiness</em>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-GPL--3.0--or--later-blue.svg?style=flat-square" alt="License"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go Version"></a>
  <a href="https://github.com/jhxxr/APISHARE/pkgs/container/apishare"><img src="https://img.shields.io/badge/Docker-GHCR-2496ED?style=flat-square&logo=docker" alt="Docker Support"></a>
  <a href="https://sqlite.org/"><img src="https://img.shields.io/badge/Storage-Pure_Go_SQLite_(WAL)-003B57?style=flat-square&logo=sqlite" alt="SQLite"></a>
  <img src="https://img.shields.io/badge/Platform-Linux%20|%20macOS%20|%20Windows-lightgrey?style=flat-square" alt="Platform">
  <img src="https://img.shields.io/badge/PRs-Welcome-brightgreen.svg?style=flat-square" alt="PRs Welcome">
</p>

<p align="center">
  <a href="#-核心特性">核心特性</a> •
  <a href="#-支持的协议">支持的协议</a> •
  <a href="#-快速开始">快速开始</a> •
  <a href="#-客户端接入">客户端接入</a> •
  <a href="#-文档">文档</a> •
  <a href="#-本地开发">本地开发</a> •
  <a href="#-许可证">许可证</a>
</p>

---

## 📖 项目简介

API Share 是一个用 Go 编写的 AI 网关。它统一接收 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 和 Google Gemini 四种协议，再把请求转发到对应的上游服务，并在转发过程中完成鉴权、计费、限流、会话粘性路由、故障转移与跨协议转换。

客户端只需要一个网关签发的 API Key，就可以用 Claude Code 驱动 DeepSeek、用 Codex CLI 驱动 Gemini，或者让任意 OpenAI 兼容客户端接入不同厂商的模型。

> [!NOTE]
> 支持官方 API 上游（OpenAI、Anthropic、Gemini、xAI、DeepSeek、GLM、Qwen 或私有部署的 vLLM / Ollama 等标准端点），并通过内置 CLIProxyAPI SDK 接入订阅账号 OAuth 凭据，无需额外安装核心或使用其 WebUI。OpenCode 免费模型流量会模拟官方客户端所用 Bun 运行时的 HTTP/1.1 请求格式与 TLS 握手特征，以通过官方免费入口的客户端校验；`/zen/go`、付费模型及其他上游不受影响。不包含网页 Cookie 抓取。

---

## ✨ 核心特性

- **Smart Router 2.0 智能分流** — 支持虚拟聚合模型（如 `auto` / 个人 `my-auto`），六维结构探针、棘轮会话升档、Prompt Cache 亲和、模态过滤、82% 甜点区重排、多源极限发现、报错自学习与无损上下文压缩。
- **全协议双向转换** — OpenAI Chat / Responses、Anthropic Messages、Gemini 四种格式任意入站与出站组合。
- **会话粘性** — 通过请求头或对话指纹把同一会话绑定到同一上游，提升 Prompt Cache 命中率。
- **优先级与加权路由** — 先按 `priority` 取最高可用层（数值越小越优先，主力 0、备用 1），同层内再按权重分流；上游返回 `401 / 403 / 408 / 429 / 5xx / 529` 或网络异常时自动切换；连续故障触发指数退避冷却。
- **个人 BYOK 自带渠道** — 普通用户可在个人面板绑定私有渠道（免扣站点额度），配置个人虚拟模型，并与站点渠道实现智能双备。
- **计费与配额** — 每日同步 LiteLLM 价格库，支持每 Key 的 QPS、并发、共享额度与模型专属额度。
- **独立代理池** — 每个上游可单独绑定 HTTP / HTTPS / SOCKS5 / SOCKS5h 代理（均支持用户名密码），OpenCode 免费流量同样经所选代理出站；内置订阅账号池也可从同一代理池选择核心出站代理。代理列表可逐个测试连通性与延迟。
- **单二进制部署** — 纯 Go SQLite（WAL）与 `go:embed` 静态前端，无需 Redis、MySQL 或 Nginx。
- **内置订阅账号池** — CLIProxyAPI SDK 随程序编译、默认启动，在现有后台完成 OAuth 登录、凭据导入与账号管理，核心版本随 API Share 发版更新。[使用与升级](docs/CLIPROXY.md)
- **管理与监控** — 初始化向导、热重命名后台路径、公开状态页、自定义站点标题与图标、调用日志与速度指标；仪表盘提供实时 RPM、消费/请求趋势、缓存命中分析与密钥 / 模型 / 上游排行，并自动识别请求客户端，记录版本与 User-Agent，支持客户端筛选和长期用量统计。
- **公开模型排行** — 首页「查看模型排行」进入 `/public/rankings/`，展示已接入全部模型（含无调用样本）的六维能力参考分，支持领域、在线后验表现和首字延迟排序，以及名称搜索和提供方筛选。能力画像与智能路由共用，明确标注校准 / 推断来源；个人 BYOK 数据不参与公共排行。

---

## 🏗️ 支持的协议

### 入站

| 协议 | 端点 | 认证方式 |
|---|---|---|
| 智能路由虚拟模型 | `auto`（站点默认）或自定义虚拟模型（如 `my-auto`） | 任意支持的入站端点均可直接传入虚拟模型调用 |
| OpenAI Chat Completions | `POST /v1/chat/completions` | `Authorization: Bearer` |
| OpenAI Responses | `POST /v1/responses`、`/v1/responses/compact` | `Authorization: Bearer` |
| OpenAI Responses WebSocket | `GET /v1/responses`（WebSocket Upgrade） | `Authorization: Bearer` |
| OpenAI Embeddings / Audio | `POST /v1/embeddings`、`/v1/audio/*` | `Authorization: Bearer` |
| OpenAI Realtime | `GET /v1/realtime`（WebSocket）、`POST /v1/realtime/calls`（WebRTC） | Bearer 或子协议 |
| Anthropic Messages | `POST /v1/messages` | `x-api-key` 或 Bearer |
| Google Gemini | `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent` | `x-goog-api-key`、Bearer 或 `?key=` |
| 模型目录 | `GET /v1/models`、`GET /v1/models/{model}` | Bearer 或 `x-api-key` |

### 出站

| 上游类型 | 管理 API `type` | 说明 |
|---|---|---|
| OpenAI Chat Completions | `openai` + `openai_protocol: "chat"` | OpenAI 官方及各类兼容商 |
| OpenAI Responses | `openai` + `openai_protocol: "responses"` | 支持 Responses 协议的上游 |
| Anthropic Messages | `anthropic` | Claude 官方端点 |
| Google Gemini | `gemini` | AI Studio 官方端点 |
| 决策模型上游 | `openrouter`、`cloudflare`、`systemone`、`jev` | 为智能分流提供低延迟（50~300ms）、低成本的任务难度判定 |
| Cline 免费模型 | `cline` | 账号登录、令牌刷新与免费模型发现 |
| CLIProxyAPI 订阅账号池 | `cliproxy` | 内置 OAuth 账号池，四种模型协议按客户端原生格式转交核心 |
| OpenCode 免费模型 | `opencode-free` | 自动发现免费模型、按模型选择原生协议，并对官方免费入口模拟 Bun TLS/HTTP 指纹 |

渠道配置、Cline / OpenCode 登录流程与路由优先级见 **[上游渠道文档](docs/UPSTREAM_CHANNELS.md)**。

---

## 🧠 智能分流 (Smart Router 2.0)

API Share 提供内置的 **Smart Router 2.0 智能分流与自适应编排引擎**。客户端只需将请求模型设置为虚拟模型（如 `auto` 或个人 `my-auto`），网关即可毫秒级感知任务复杂度，智能分发至最具性价比的模型候选。

- **六维全量结构探针** — 分类器不仅分析当前用户文本，还全面感知会话中的 **Assistant 历史代码密度**、**工具循环调用深度（`ToolLoopDepth`）**、**编译/运行时报错堆栈**、**多模态图片/附件**、**短指令追问（如“继续 / 改一下”）** 以及 **用户挫败/重试意图（如“还是报错 / 不对”）**，计算结构保底档位（`StructuralFloor`），根治多轮编程中短指令追问误落弱模型的痛点。
- **棘轮会话状态机（Session Ratchet）与 Prompt Cache 亲和** — 针对 Coding Agent 长对话场景，同档请求优先保持原模型以最大化上游 Prompt KV Cache 命中率；而当后续提问复杂度上升或遇到挫败报错时，会话状态机立即单向升档（棘轮向上突破），兼顾缓存经济性与智商上限。
- **模态感知与 82% 甜点区重排** — 携带图片时自动跳过纯文本推理模型；预留输出与思考 Token 余量，若某候选模型已进入 $>82\%$ 窗口容量衰减区，自动提升同档充裕候选优先级。
- **多源极限发现与 400 报错自学习** — 自动整合 LiteLLM 官方 3900+ 模型库同步、上游 `/v1/models` 原生元数据提取、以及上游 `400 / 413 / 422` 报错实时自学习，毫秒级自适应更新真实上下文窗口与最大输出上限。
- **二维测试时算力（TTC）自适应缩放** — 跨厂商（OpenAI、Claude 3.7/4.x、Gemini 2.5/3、DeepSeek、Qwen）自适应转换 Thinking 思考参数；在重试报错或小模型承担复杂任务时自动提升思考强度补偿参数量差距。
- **Middle-Out 无损上下文压缩** — 当长对话逼近模型上下文上限时，优先折叠陈旧大工具日志（保留头尾），并保证 `tool_use`/`tool_result` 结对完整性，告别 400 Context Exceeded。
- **个人 BYOK 混合路由** — 普通用户可在个人中心绑定个人私有 API Key（调用免扣站点额度），并与站点授权模型组成双备容灾。

---

## 🚀 快速开始

### Docker 部署（推荐）

镜像发布在 GitHub Container Registry，支持 `linux/amd64` 与 `linux/arm64`：

```bash
docker run -d \
  --name apishare \
  --restart unless-stopped \
  -p 8080:8080 \
  -v apishare-data:/data \
  ghcr.io/jhxxr/apishare:latest
```

或使用 `docker-compose.yml`：

```yaml
services:
  apishare:
    image: ghcr.io/jhxxr/apishare:latest
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

### 本地编译

环境要求：Go 1.26.2+，无需 CGO。

```bash
git clone https://github.com/jhxxr/APISHARE.git && cd APISHARE
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o apishare ./cmd/apishare
PORT=8080 API_DB=apishare.db ./apishare
```

Windows PowerShell：

```powershell
go build -trimpath -ldflags="-s -w" -o apishare.exe ./cmd/apishare
$env:PORT = "8080"; $env:API_DB = "apishare.db"; .\apishare.exe
```

### 首次初始化

1. 打开 `http://127.0.0.1:8080/`，按向导设置管理员密码与后台路径（默认 `/admin`）。
2. 进入「上游服务」添加上游：选择接口类型，填写 Base URL、API Key、优先级、权重与模型白名单。
3. 进入「API 密钥」创建访问令牌，绑定可用模型并设置额度。
4. 在「设置」中检查 LiteLLM 价格库同步状态，按需配置自定义价格。

用一条请求验证：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-your-apishare-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

---

## 🔌 客户端接入

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_API_KEY="sk-your-apishare-key"
claude
```

### Codex CLI

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

```bash
export APISHARE_API_KEY="sk-your-apishare-key"
codex
```

### OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-your-apishare-key",
    base_url="http://127.0.0.1:8080/v1",
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True,
)
for chunk in response:
    print(chunk.choices[0].delta.content or "", end="")
```

Claude Code 结构化输出、Codex 配置说明、Gemini 与 cURL 示例、音频和 Realtime 接入见 **[客户端接入指南](docs/CLIENTS.md)**。

---

## ⚙️ 常用配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8080` | 服务监听端口 |
| `API_DB` | `apishare.db` | SQLite 数据库路径 |
| `ADMIN_PATH` | `admin` | 空数据库首次启动时的默认后台路径 |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `180` | 等待上游响应头的超时（秒） |
| `UPSTREAM_IDLE_TIMEOUT_SEC` | `300` | 上游数据空闲超时（秒，0 为不限） |

完整环境变量、代理池、动态请求头、模型映射与日志保留见 **[配置参考](docs/CONFIGURATION.md)**。

---

## 📚 文档

| 文档 | 内容 |
|---|---|
| [上游渠道与协议矩阵](docs/UPSTREAM_CHANNELS.md) | 入站 / 出站协议、路由优先级、Cline 与 OpenCode 渠道 |
| [内置 CLIProxyAPI](docs/CLIPROXY.md) | 订阅账号、持久化数据、SDK 版本与随发行版升级 |
| [客户端接入指南](docs/CLIENTS.md) | Codex、Claude Code、OpenAI SDK、Gemini、音频与 Realtime |
| [协议转换引擎](docs/PROTOCOL_CONVERSION.md) | 工具调用、推理内容、流式保活与已知边界 |
| [会话粘性、计费与配额](docs/BILLING.md) | Prompt Cache、价格同步、Usage 采集、日志清理 |
| [配置参考](docs/CONFIGURATION.md) | 环境变量、代理、请求头、模型映射 |
| [本地开发与测试](docs/DEVELOPMENT.md) | 目录结构、构建、Mock 上游、测试 |
| [全格式审计](docs/SUB2API_ALL_FORMATS_AUDIT.md) | 跨协议行为与 sub2api 问题对照 |
| [Responses 审计](docs/SUB2API_RESPONSES_AUDIT.md) | Responses / Codex 兼容性明细 |
| [计费审计](docs/SUB2API_BILLING_AUDIT.md) | Token 分类与计费口径 |

---

## 🛠️ 本地开发

```bash
# 启动两个 Mock 上游
go run ./cmd/mockupstream -addr :18081 -name mockA
go run ./cmd/mockupstream -addr :18082 -name mockB

# 运行测试
go test ./...

# Codex CLI 协议回路测试（需已安装 codex）
export APISHARE_CODEX_BINARY="codex"
go test ./cmd/apishare -run TestCodexCLIResponsesToolLoop -v -count=1
```

目录结构、前端资源与更多测试说明见 **[开发文档](docs/DEVELOPMENT.md)**。

---

## 🗺️ 路线图

- [x] OpenAI Chat / Responses、Anthropic Messages、Gemini 全协议覆盖
- [x] Smart Router 2.0 智能分流引擎与个人 BYOK 混合编排
- [x] 会话粘性与 Prompt Cache 优化
- [x] LiteLLM 价格库每日同步
- [x] 独立代理池与动态请求头
- [x] 共享额度与模型专属配额
- [x] 音频与 Realtime（WebSocket / WebRTC）
- [ ] 分布式集群与 SQLite 异地备份

---

## 🤝 参与贡献

欢迎提交 Issue 与 Pull Request。

1. Fork 仓库并创建特性分支：`git checkout -b feature/amazing-feature`
2. 修改代码并通过测试：`go test ./...`
3. 提交：`git commit -m 'feat: add amazing feature'`
4. 推送并发起 Pull Request

---

## 📄 许可证

Copyright (C) 2026 API Share contributors.

本项目基于 [GNU General Public License v3.0 or later](LICENSE) 开源。

`internal/bunhttp` 移植自 [opencode2api](https://github.com/jasonxu114514/opencode2api)（GPL-3.0-or-later，Copyright (C) Jason Xu），修改说明见 [internal/bunhttp/NOTICE](internal/bunhttp/NOTICE)。`cmd/apishare/third_party/opencode` 中的 OpenCode 工具定义仍分别遵循其 MIT 许可。

前端内嵌的 Space Grotesk 与 Orbitron 字体遵循各自的 [SIL Open Font License](cmd/apishare/web/public/fonts/OFL.txt)。

感谢 [LiteLLM](https://github.com/BerriAI/litellm) 提供的模型价格规范，以及 [New API](https://github.com/QuantumNous/new-api)、[Sub2API](https://github.com/Wei-Shaw/sub2api) 在协议兼容设计上的探索。

内置 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) SDK 遵循 [MIT 许可](cmd/apishare/third_party/cliproxy/LICENSE)，后台上游的「账号池运行设置」提供许可原文，Docker 镜像同时携带该文件。

<div align="center">
  <sub>Built with ❤️ by the API Share contributors.</sub>
</div>
