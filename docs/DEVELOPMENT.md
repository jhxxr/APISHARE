# 本地开发与测试

## 环境要求

- Go 1.26.2+
- 无需 CGO：SQLite 使用纯 Go 驱动 `modernc.org/sqlite`

## 目录结构

```text
.
├── cmd/
│   ├── apishare/          # 网关主程序与 Go 测试
│   │   └── web/           # 编译时内嵌的管理后台和公开页面
│   └── mockupstream/      # 多协议 Mock 上游
├── docs/                  # 协议兼容、计费与配置文档
├── .github/workflows/     # 自动构建与镜像发布
├── Dockerfile
├── go.mod
├── go.sum
├── LICENSE
└── README.md
```

以下命令均在仓库根目录执行；Go 模块文件保留在根目录，主程序入口为 `./cmd/apishare`。

## 构建

```bash
# Linux / macOS
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o apishare ./cmd/apishare

# Windows (PowerShell)
go build -trimpath -ldflags="-s -w" -o apishare.exe ./cmd/apishare
```

## Mock 上游

代码库内置多协议 Mock 测试桩 `cmd/mockupstream`，可一次性模拟四大协议服务端，方便脱机验证转换逻辑与计费：

```bash
# 启动两个独立的假上游示例
go run ./cmd/mockupstream -addr :18081 -name mockA
go run ./cmd/mockupstream -addr :18082 -name mockB
```

## 测试

```bash
# 执行完整的测试套件
go test -v ./...

# 运行真实的 Codex CLI 协议回路测试 (需本地已安装 codex)
export APISHARE_CODEX_BINARY="codex"
go test ./cmd/apishare -run TestCodexCLIResponsesToolLoop -v -count=1

# 原生 WebSocket 工具回路，验证长连接和增量续接
go test ./cmd/apishare -run TestCodexCLIResponsesWebSocketToolLoop -v -count=1
```

## 前端资源

管理后台与公开页面位于 `cmd/apishare/web/`，通过 `go:embed` 编译进单二进制。HTML 引用静态资源时使用内容版本化 URL，修改前端文件后无需单独部署静态服务器。

## 文档索引

- [上游渠道与协议矩阵](UPSTREAM_CHANNELS.md)
- [客户端接入](CLIENTS.md)
- [协议转换引擎](PROTOCOL_CONVERSION.md)
- [会话粘性、计费与配额](BILLING.md)
- [配置参考](CONFIGURATION.md)
- [性能优化经验与基准](PERFORMANCE.md)
- [全格式审计](SUB2API_ALL_FORMATS_AUDIT.md)
- [Responses 审计](SUB2API_RESPONSES_AUDIT.md)
- [计费审计](SUB2API_BILLING_AUDIT.md)
