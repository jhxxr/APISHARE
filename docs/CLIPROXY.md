# 内置 CLIProxyAPI

CLIProxyAPI 的 Go SDK 默认编入 API Share，随网关启动。用户无需下载核心、安装额外程序或部署第二个容器，也不使用 CLIProxyAPI 的 WebUI。

## 使用

1. 打开后台的「订阅账号」。
2. 选择提供商登录，或导入 CLIProxyAPI 格式的账号 JSON 凭据。
3. 远程部署时，如授权后浏览器停在 localhost 回调地址，将完整地址复制到后台的「完整回调地址」中提交。
4. 点击「接入上游服务」，创建一个 `type: "cliproxy"` 的账号池渠道。重复点击不会创建重复渠道。
5. 在「API 密钥」中授权需要的模型，并按需设置渠道优先级、权重、模型白名单和计费价格。

### 出站代理

「订阅账号 → 核心设置 → 出站代理」可从后台已有的代理池中选择一个代理，作为内置核心访问厂商网络的出口。支持 HTTP、HTTPS、SOCKS5、SOCKS5h 及用户名密码认证；OAuth 登录、凭据刷新和模型请求都会经过该代理。

- 选择结果保存在网关数据库中，并写入 `cliproxy-data/config.yaml` 的 `requests.proxy-url`。核心运行中修改会通过其管理接口热加载，不中断正在处理的请求。
- 「默认连接」会清空该设置，核心按进程环境（`HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`）出网。
- 账号池渠道自身的「请求代理」不参与该出口：该渠道只连接本机回环地址，真正出网的是核心。
- 代理密码不会出现在后台状态、代理列表或日志中。

网关保留鉴权、配额、限流、统计和渠道故障转移；SDK 负责 OAuth 凭据、刷新、池内账号调度及厂商适配。四种模型协议按客户端格式交给 SDK，避免重复跨协议转换。转发的会话标识按网关 API Key 隔离；核心启用池内会话粘性。

后台可以启停核心；主动停止后，下次重启网关也保持停止。普通 API 上游不依赖此核心。核心默认只监听本机回环地址，管理密钥不会发到浏览器；仅将明确支持的账号操作接入现有管理员认证，不提供通用管理 API 透传。

## 数据与部署

默认数据目录为 SQLite 文件所在目录下的 `cliproxy-data/`。Docker 使用原有 `/data` 持久卷，无需增加端口或卷：

- `state.json`：内部连接密钥、监听端口和启动偏好。
- `config.yaml`：核心配置。
- `auths/`：由 SDK 保存、刷新账号凭据。

升级时保留整个数据目录。新程序会加载同一份账号和配置，不覆盖已有配置。

可选环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `CLIPROXY_DATA` | 数据库所在目录的 `cliproxy-data` | 内置核心持久化目录 |
| `CLIPROXY_PORT` | `18317` | 首次初始化时的内部回环监听端口；后续使用状态文件中已保存的端口 |

## 随 API Share 发版升级

版本由根目录 `go.mod` / `go.sum` 锁定，后台展示实际编译进去的模块版本。程序运行时没有核心下载、独立升级或自动更新入口。

维护者升级依赖：

```powershell
go get github.com/router-for-me/CLIProxyAPI/v8@v8.x.y
go mod tidy
go test ./...
go build -trimpath -ldflags="-s -w" -o apishare.exe ./cmd/apishare
```

将 `v8.x.y` 替换成拟发布的具体版本，验证 OAuth 流程、原生工具调用、流式取消和 usage 口径，再按 API Share 原有流程发布二进制或 Docker 镜像。依赖版本和适配改动随同一提交评审。用户只更新 API Share。

SDK 公共接口、管理 API 或大版本变化仍需要适配与回归，不能保证任意上游版本直接替换。回退整个 API Share 版本前应备份数据目录，并检查上游是否迁移过配置或凭据格式。

接入层位于 `internal/cliproxy`，后台路由位于 `cmd/apishare/cliproxy.go`。上游源码不作本地修改，避免长期维护 fork。

## 验证与许可

`go test ./...` 包含实际内置 SDK 的启动、客户端鉴权、管理接口、停止和重启测试，以及网关管理权限、敏感字段过滤、会话隔离、原生 Responses 转发和用量采集测试。测试不需要真实用户账号。

CLIProxyAPI 使用 MIT 许可。其版权与许可原文保存在 `cmd/apishare/third_party/cliproxy/LICENSE`，Docker 镜像同时携带该文件。上游项目与文档：

- https://github.com/router-for-me/CLIProxyAPI
- https://help.router-for.me/management/apiv8
