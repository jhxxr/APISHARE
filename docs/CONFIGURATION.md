# 配置参考

## 环境变量

所有环境变量均具备合理的生产默认值，按需覆盖即可。

| 环境变量 | 默认值 | 作用说明 |
|---|---|---|
| `PORT` | `8080` | 服务监听端口 |
| `API_DB` | `apishare.db` | SQLite 数据库文件存放路径（容器内为 `/data/apishare.db`） |
| `ADMIN_PATH` | `admin` | 首次配置向导设定的默认后台路径（首启后以数据库中的配置为准） |
| `LOG_TRUST_FORWARDED_IP` | `true` | 请求日志使用代理头兼容模式，优先提取公网 IP；设为 `false` 时仅按 `TRUSTED_PROXIES` 解析 |
| `LOG_FORWARDED_IP_HEADERS` | 空 | 请求日志额外读取的 IP 请求头名称，按优先级用逗号分隔 |
| `TRUSTED_PROXIES` | 空 | 可信入站代理 IP／CIDR，支持逗号或空白分隔；为空时不信任任何代理，影响严格模式及登录限流的来源 IP |
| `UPSTREAM_MAX_IDLE_CONNS` | `256` | 连接池最大空闲连接总数 |
| `UPSTREAM_MAX_IDLE_CONNS_PER_HOST` | `64` | 每个目标主机保留的最大空闲连接数 |
| `UPSTREAM_DIAL_TIMEOUT_SEC` | `15` | TCP 建连与 TLS 握手超时时间（秒） |
| `UPSTREAM_HEADER_TIMEOUT_SEC` | `180` | 等待上游响应首包头部的最长超时（秒，0 为不限） |
| `UPSTREAM_IDLE_TIMEOUT_SEC` | `300` | HTTP / Realtime 等待上游数据包的空闲超时（秒，不限制流式持续总长，0 为不限）；Responses WS 使用独立 ping/pong 保活 |
| `STREAM_KEEPALIVE_INTERVAL_SEC` | `10` | Messages 流式请求向客户端发送保活事件的间隔（秒，0 为关闭，最大 300）；不影响上游空闲超时，其他协议和非流式请求不注入保活 |
| `REALTIME_MAX_DURATION_SEC` | `3600` | Realtime WebSocket 单连接最长存活时间（秒，1～86400）；空闲读取另受 `UPSTREAM_IDLE_TIMEOUT_SEC` 限制 |
| `RESPONSES_WS_MAX_DURATION_SEC` | `3300` | Responses WebSocket 单连接最长存活时间（秒，1～86400）；到期发送 `1001` 关闭帧，由客户端重连 |
| `RESPONSES_WS_PING_INTERVAL_SEC` | `30` | Responses WebSocket 双向协议 ping 间隔（秒，1～300）；每次等待 pong 最长 10 秒 |
| `RESPONSES_WS_MAX_CONNECTIONS_PER_KEY` | `16` | 每个 Key 的 Responses WS 连接数上限（1～1024），独立于推理并发；每连接最多 64 个待完成请求 |
| `RESPONSES_WS_DRAIN_TIMEOUT_MS` | `1500` | 客户端断开或写入失败后，继续读取上游末尾 usage 的短窗口（毫秒，0～30000，0 关闭）；期间不再接收新轮次 |
| `TIKTOKEN_CACHE_DIR` | 临时目录/`data-gym-cache` | 本地 Token 分词词表缓存路径 |

## 请求 IP 与反向代理

请求日志默认启用兼容模式，适用于 Docker、Nginx 和 CDN 转发。依次检查 `LOG_FORWARDED_IP_HEADERS` 中的自定义头、`CF-Connecting-IP`、`True-Client-IP`、`X-Real-IP`、`X-Forwarded-For`。跳过非法值、未指定地址和多播地址，优先选择公网地址；私网、回环及链路本地地址仅作为后备。因此即使 `X-Real-IP` 中误填了 Docker 网桥地址，只要 `X-Forwarded-For` 中有公网 IP，日志仍会记录公网 IP。没有公网候选时保留第一个有效后备地址；没有有效转发头时使用 Gin 解析的连接来源地址。IPv4、IPv6、IPv4 映射地址和带端口地址会统一规范化。

兼容模式的日志 IP 来自请求头自报信息，反向代理应覆盖或正确维护这些头。登录限流仍通过 Gin 的可信代理链解析来源，不使用日志兼容模式。需要严格验证来源时设置 `LOG_TRUST_FORWARDED_IP=false`，并配置实际代理的 IP 或 CIDR：

```yaml
environment:
  - LOG_TRUST_FORWARDED_IP=false
  - TRUSTED_PROXIES=172.18.0.2/32,127.0.0.1/32
```

这里的地址仅为示例，应替换为应用实际收到连接的反向代理地址。严格模式从 `X-Forwarded-For` 右侧验证代理链，遇到第一个不可信节点即停止；不属于可信代理的直连请求不能通过伪造转发头改变来源 IP。自定义头仅在日志兼容模式中使用，例如 `LOG_FORWARDED_IP_HEADERS=X-Client-IP,X-Edge-IP`。

反向代理必须传递来源 IP。Nginx 常见配置如下；若 Nginx 本身位于 CDN 之后，还需正确配置其真实 IP 解析：

```nginx
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
```

以上环境变量在启动时读取，修改后需重启容器。配置错误的可信代理列表会整体禁用代理信任；无法解析的兼容模式开关会回退到严格模式，并在启动日志中提示。历史请求已经丢失的公网 IP 无法通过本次修改补回，新请求会使用新的解析方式。

## 独立上游代理池

每个上游可独立配置代理网络，支持 `HTTP`、`HTTPS`、`SOCKS5`、`SOCKS5h`（代理解析域名）及鉴权。连接测试、模型拉取、常规请求与流式传输均严格遵守所选代理通道。

「代理配置」列表中的「测试」会从网关服务器经该代理访问公共探测地址（`generate_204` 等），显示握手与响应耗时；任一探测地址成功即视为连通，失败时在列表中给出原因。停用的代理也可以测试，便于启用前先确认出口。该结果只反映网关到公共网络的连通性，不代表具体上游或账号可用，且不会影响正在处理的请求。

`type: "cliproxy"` 的账号池渠道连接的是本机回环地址，因此不使用渠道自身的「请求代理」。内置核心访问厂商网络（登录、刷新、模型请求）的出口在「订阅账号 → 核心设置 → 出站代理」中从同一代理池选择，详见 [内置 CLIProxyAPI](CLIPROXY.md#出站代理)。

## 动态请求头

在后台每个上游均可配置请求头覆盖规则，支持正则透传与变量注入：

```json
{
  "*": true,
  "re:^X-Trace-.*$": true,
  "X-Custom-Env": "production",
  "User-Agent": "{claude_code_user_agent}",
  "Authorization": "Bearer {api_key}"
}
```

网关后台内置自动化守护器，定期从 npm 官方仓库同步获取 `@openai/codex`、`@anthropic-ai/claude-code` 和 `@opencode/cli` 的最新稳定版本，通过 `{codex_user_agent}`、`{claude_code_user_agent}` 与 `{opencode_user_agent}` 占位符自动注入对应格式的 User-Agent。OpenCode 专用渠道默认自动使用其版本，无需额外配置模板。

转发 Anthropic Messages 请求时，网关默认合并客户端和渠道生成的 `anthropic-beta`，支持逗号分隔及多行请求头，并自动去重。当最终请求体使用 `thinking.display: "updates"` 时，还会在请求头覆盖规则执行后补齐必需的 `thinking-display-updates-2026-08-18`，适用于 Anthropic、OpenCode 和 CLIProxy 账号池渠道。

书生 TokenPlan（`discovery-api.intern-ai.org.cn`）的 Anthropic 兼容接口即使带上 beta 也会拒绝 `thinking.display: "updates"`。网关仅对该域名的 Messages 请求移除这个可选字段及对应的 beta 标记，清理 system、消息及工具结果中的空文本块和空历史消息，保留思考模式、预算、有效文本、图片、工具调用与签名。空的最后一条用户消息仍由上游校验，不会自动重放较早的用户消息；其他域名和协议保持原样。

## 模型白名单与模型映射

在上游编辑器的「模型访问」中输入模型名，按回车、逗号或粘贴多行即可生成可移除的标签，重复名称自动去重；也可以获取当前 Key 的模型后勾选。白名单留空允许全部模型。

模型映射支持可视化逐行编辑、JSON 切换和批量添加。例如 `{"chat-fast":"deepseek-chat"}` 表示客户端请求 `chat-fast` 时，该上游收到 `deepseek-chat`。映射中的请求名称自动参与模型发现和显式路由，无需再手动加入上游白名单；API Key 的模型权限和配额仍按客户端请求名称检查。每条规则只替换一次，保存后立即生效。批量添加每行填写 `请求模型 = 上游模型`，空名称、重复规则和无效 JSON 会提示错误。

## 管理后台路径

首次初始化时设置，之后可在后台热重命名（如 `/secret-portal`）。`ADMIN_PATH` 只影响空数据库首次启动时的默认路径，首启后以数据库中的配置为准。

## 网站标题与图标

在「工作空间设置」的「网站外观」中可以修改公开首页、登录页、后台和首次配置页显示的网站标题与图标。标题留空即恢复默认名称；图标支持 PNG、JPEG、GIF、WebP 与 ICO，最大 512 KB、最多 1600 万像素，并同时用作浏览器标签页图标和页面 Logo。上传时会验证图片可解码，校验失败不会覆盖原图。上传内容会保存在 SQLite 中，无需修改静态文件；恢复默认图标不会影响其他设置。

## 调用记录保留

见[计费与会话文档](BILLING.md#调用记录自动清理)。

## XAuth 统一身份认证与用户专属控制台

在「工作空间设置」中启用 XAuth OIDC 认证并配置 `Issuer`、`Client ID`、`Client Secret`（回调地址为 `/api/auth/callback`）后，公开首页将展示 Auth 登录入口。通过 XAuth 应用访问控制授权的用户首次登录时，系统会自动创建专属 `sk-...` 密钥并发放预设的默认赠送额度、可用模型（支持共享或独立模型额度）、速率限制与并发上限；用户可在 `/user/` 个人专属控制台中查看或轮换自己的密钥、查看剩余额度、每日用量趋势与个人请求明细。管理员可在「API 密钥」列表直观查看每个密钥绑定的模型、单独调整任意用户密钥的配额与模型，或使用「批量添加模型」（已有模型自动跳过并保留原配额，缺失模型自动追加）；若管理员删除某 XAuth 用户的密钥，该用户再次登录时新生成的密钥可用额度将置为 `0`。
