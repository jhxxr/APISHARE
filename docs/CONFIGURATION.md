# 配置参考

## 环境变量

所有环境变量均具备合理的生产默认值，按需覆盖即可。

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
| `STREAM_KEEPALIVE_INTERVAL_SEC` | `10` | Messages 流式请求向客户端发送保活事件的间隔（秒，0 为关闭，最大 300）；不影响上游空闲超时，其他协议和非流式请求不注入保活 |
| `REALTIME_MAX_DURATION_SEC` | `3600` | Realtime WebSocket 单连接最长存活时间（秒，1～86400）；空闲读取另受 `UPSTREAM_IDLE_TIMEOUT_SEC` 限制 |
| `TIKTOKEN_CACHE_DIR` | 临时目录/`data-gym-cache` | 本地 Token 分词词表缓存路径 |

## 独立上游代理池

每个上游可独立配置代理网络，支持 `HTTP`、`HTTPS`、`SOCKS5`、`SOCKS5h`（代理解析域名）及鉴权。连接测试、模型拉取、常规请求与流式传输均严格遵守所选代理通道。

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

## 模型白名单与模型映射

在上游编辑器的「模型访问」中输入模型名，按回车、逗号或粘贴多行即可生成可移除的标签，重复名称自动去重；也可以获取当前 Key 的模型后勾选。白名单留空允许全部模型。

模型映射支持可视化逐行编辑、JSON 切换和批量添加。例如 `{"chat-fast":"deepseek-chat"}` 表示客户端请求 `chat-fast` 时，该上游收到 `deepseek-chat`。映射中的请求名称自动参与模型发现和显式路由，无需再手动加入上游白名单；API Key 的模型权限和配额仍按客户端请求名称检查。每条规则只替换一次，保存后立即生效。批量添加每行填写 `请求模型 = 上游模型`，空名称、重复规则和无效 JSON 会提示错误。

## 管理后台路径

首次初始化时设置，之后可在后台热重命名（如 `/secret-portal`）。`ADMIN_PATH` 只影响空数据库首次启动时的默认路径，首启后以数据库中的配置为准。

## 网站标题与图标

在「工作空间设置」的「网站外观」中可以修改公开首页、登录页、后台和首次配置页显示的网站标题与图标。标题留空即恢复默认名称；图标支持 PNG、JPEG、GIF、WebP 与 ICO，最大 512 KB、最多 1600 万像素，并同时用作浏览器标签页图标和页面 Logo。上传时会验证图片可解码，校验失败不会覆盖原图。上传内容会保存在 SQLite 中，无需修改静态文件；恢复默认图标不会影响其他设置。

## 调用记录保留

见[计费与会话文档](BILLING.md#调用记录自动清理)。
