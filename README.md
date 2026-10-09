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

以上所有端点都支持流式与非流式。

### 上游类型（出站）

| 类型 | 适用供应商 | Base URL 示例 |
|---|---|---|
| `openai` | OpenAI、**xAI Grok**（api.x.ai 即 OpenAI 兼容）、DeepSeek、GLM、Qwen、本地 vLLM/Ollama 等 | `https://api.openai.com`、`https://api.x.ai` |
| `anthropic` | Anthropic 官方 API | `https://api.anthropic.com` |
| `gemini` | Google AI 官方 API | `https://generativelanguage.googleapis.com` |

### 跨格式转换

客户端格式与上游类型不一致时自动转换（内部走规范化表示）：

- 支持：system、多轮对话、max_tokens、temperature、流式（逐增量转换，非伪流式）、真实 usage 计费（上游报多少算多少）
- **不支持：工具调用（tools）等高级特性的跨格式转换**——带 tools 的请求若被路由到需要转换的上游，会返回 400 并提示为该模型配置原生格式上游；同格式直连（如 Claude Code → Anthropic 上游）功能完全无损
- 路由优先级（`pickUpstream`）：原生格式+显式白名单 ＞ 其他格式+显式白名单 ＞ 原生格式+通配 ＞ 其他格式+通配。想精确控制就给每个上游配模型白名单
- 会话粘性对转换路径同样生效

## 核心特性

- **会话粘性（保证 cache 命中）**：同一对话固定路由到同一上游。识别优先级：`X-Session-Id`/`X-Conversation-Id` 头 ＞ 对话前缀指纹 `hash(端点+模型+system+首条user消息)`。粘性表存 SQLite、滑动 TTL（默认 30 分钟，命中自动续期），重启不丢。粘性上游故障时自动转移并重绑，恢复后不回切
- **加权调度 + 故障转移**：上游 401/403/408/429/5xx/529 或网络错误时透明重试下一个上游；连续失败进入指数退避冷却（30s 起步，上限 10 分钟）
- **计费（自动官方定价）**：启动时与每日自动同步 **LiteLLM 官方牌价镜像库**（各厂商无定价 API，这是 LiteLLM/sub2api 一类网关通用的官方定价来源，覆盖 OpenAI / Anthropic / Gemini / Grok / DeepSeek 等全部现行型号，含 Bedrock/Vertex 键名形态归一化）。计费优先级：**手动覆盖 > 官方定价 > 内置兜底表 > default(0)**。用量优先取上游真实 usage（OpenAI 流式自动注入 `include_usage`；Anthropic 解析 `message_start/message_delta`；Gemini 解析 `usageMetadata`；Responses 解析 `response.completed`），缺失时 tiktoken 本地估算
- **限流**：每 Key 独立 QPS 令牌桶 + 并发信号量
- **公开首页 `/`**：首屏展示当前模型与累计 Token，下滑查看公开余量、用量趋势、模型分布和真实调用 Uptime。后台按密钥勾选参与合计，公开接口不返回密钥、名称、数量、上游地址或凭证。
- **管理后台（默认 `/admin`）**：Key 签发与额度、上游管理（三种类型 + 连通性测试）、调用日志（含格式/上游/usage/费用）、价格表与调度参数；未注册路径一律 404
- **上游独立代理**：后台集中管理 HTTP / HTTPS / SOCKS5 / SOCKS5h 代理及认证，每个上游可单独选择；模型调用、流式响应、连通性测试和模型目录获取均通过所选代理。
- **单文件部署**：前端内嵌 Go 二进制，SQLite（WAL）存储，零外部依赖

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
2. 「上游」页添加上游：选类型、填 Base URL（不含 `/v1`/`/v1beta`，如 `https://api.openai.com`）、API Key、权重、**模型白名单**（强烈建议填写，跨格式路由靠它）；
3. 「API 密钥」页创建访问密钥，设置额度（USD，0=不限）、QPS 和并发限制；
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

在后台「API 密钥」列表勾选 **首页展示**，访问服务根地址 `/` 即可查看合计。已有密钥升级后默认不公开；勾选多个时汇总这些密钥的调用量、Token 用量与额度，未勾选的不参与统计。停用密钥的历史记录仍计入所选合计，删除或取消勾选后移除。

- 余量沿用后台的 USD 额度，逐密钥计算 `max(总额度 - 已使用, 0)` 再求和。任何所选密钥不限额时显示“不限额度”；Token 用量分别展示累计输入和输出。
- 模型目录来自启用且有分配权重的上游白名单；通配上游在后台发现模型并缓存，发现模型不计为成功调用。
- **Uptime 固定统计最近 24 小时的真实请求成功率**，按总成功请求 / 总请求加权计算。默认每小时一格，可切换为每分钟一格；两种粒度覆盖同一个滚动 24 小时窗口，切换不改变成功率。分钟轨迹支持横向滚动，历史详情可选择这 24 小时内任意一小时查看分钟记录。无调用时段显示灰色，且不计入分母；请求日志保留秒级时间，这不是定时探测得出的在线时长。
- 当前模型状态取最近 24 小时内的最后一次所选调用结果；无近期调用显示灰色。故障转移后的最终请求结果计为一次调用，断开的流式响应计为失败。
- 首页使用奶油色背景与陶土色流线动画，打开即为全屏 Token 用量。没有顶部导航、宣传区或立体环；中央数字逐位向上滚动并停在真实累计值，每次进入页面播放一次，自动刷新不重播。无公开用量时显示“—”，不生成虚假数字。下滑查看用量、趋势、模型与历史；底部可暂停动画。首屏离开视野或标签页隐藏时停止背景，减少动态效果偏好会关闭流线运动、入场和数字滚动。页面每 30 秒更新，可暂停，标签页隐藏或查看历史时暂停请求。

### 客户端接入示例

#### 配置上游代理

1. 在后台「代理配置」添加代理名称和地址，例如 `http://127.0.0.1:7890`、`https://proxy.example.com:8443` 或 `socks5://127.0.0.1:1080`。地址必须包含端口；IPv6 使用 `socks5://[::1]:1080`。
2. 如代理需要认证，分别填写用户名与密码。列表不会返回密码；编辑时密码留空保留原值，勾选「清除已保存的密码」可删除密码，清空用户名则移除认证。
3. 打开「上游服务」添加或编辑上游，在「连接与认证 → 请求代理」选择代理并保存。一个代理可以供多个供应商共用，配置保存在 SQLite，重启后保留。

已选择的代理不受系统 `NO_PROXY` 绕过规则影响；SOCKS5 / SOCKS5h 均由代理解析目标域名。未选择代理时沿用原有 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境设置，未设置时直连。代理配置修改对后续请求生效，进行中的请求继续使用原连接。停用或失效的代理不会自动降级为直连，而是触发既有上游故障转移；删除代理前需要解除所有上游关联。可使用上游列表的「测试连接」或编辑器的「获取当前 Key 模型」验证实际代理链路。

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

网关 Key 当前共享启用且权重大于零的上游目录，仍执行 Key 鉴权、额度与限流检查；没有单独配置每个网关 Key 的模型权限。显式白名单直接纳入目录；通配上游根据其当前上游 Key 拉取并缓存 5 分钟，更换地址、类型或 Key 后重新获取。目录按模型 ID 排序去重，映射别名只有符合白名单时才返回；未知发布时间使用 `0` / Unix epoch，展示名使用模型 ID。模型目录表示配置或上游声明的可用性，不保证每个模型的推理请求一定成功；查询目录不计费、不写入推理调用统计。

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

管理员密码通过首次配置页创建，不再从 `ADMIN_PASSWORD` 生成；已有数据库中保存的密码继续有效。

## 本地联调工具

`cmd/mockupstream` 是一个假上游，同时实现四种服务端格式，回复里带自己的名字，用来验证转换、粘性与计费：

```bash
go run ./cmd/mockupstream -addr :18081 -name mockA
go run ./cmd/mockupstream -addr :18082 -name mockB
# 管理后台把两个 mock 加为 openai 上游（白名单 mock-model），再各加 anthropic/gemini 类型上游指向同端口即可
```

## 注意事项

- tiktoken 首次使用会联网下载词表（缓存在系统临时目录），离线自动退化为字符启发式估算；上游返回真实 usage 时以 usage 为准。
- Gemini 只支持 `generateContent` / `streamGenerateContent`（流式统一按 SSE `alt=sse` 处理，官方 SDK 均使用该模式）；`countTokens` 等其他方法不转发。
- 跨格式转换的流式响应中，Anthropic 客户端拿到的 `message_start.usage.input_tokens` 为 0，真实输入用量在末尾 `message_delta.usage` 中（原生直连无此问题）。
- 已下架型号（如 grok-4）在现行官方价格库中无标价，按 default(0) 记账不扣费；需要计费就在价格表里手动覆盖。
- 上游健康状态（连续失败/冷却）在内存中，重启后清零。
- 建议在前端挂 nginx/caddy 做 HTTPS；`/healthz` 可用于探活。

## 许可证

项目代码采用 [MIT License](LICENSE)。内嵌的 Space Grotesk 和 Orbitron 字体分别遵循其 [SIL Open Font License](web/public/fonts/OFL.txt) 和 [SIL Open Font License](web/public/fonts/orbitron-OFL.txt)，字体目录中保留了原始授权文件。
