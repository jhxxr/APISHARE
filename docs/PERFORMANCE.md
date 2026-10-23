# 性能优化经验与基准

整理日期：2026-10-23。以下结论结合 Go、Caddy、Envoy、SQLite 的一手资料和本仓库实测；外部项目的性能数字不作为 API Share 的收益承诺。

## 网上经验与本项目的落点

| 经验 | 来源 | 对 API Share 的应用 |
| --- | --- | --- |
| 先区分 CPU、累计分配、存活堆和锁等待；优化后重复测量 | [S1] Go Diagnostics、[S2] GC Guide | 使用 `-benchmem` 与 `pprof -alloc_space` 找到重复 JSON 解析和配置复制。累计分配不能当成进程常驻内存。 |
| 减少分配可降低 GC 开销；`GOGC` / `GOMEMLIMIT` 调节的是 CPU 与内存的权衡 | [S2] GC Guide | 先减少每条 SSE 事件的重复工作，再根据部署负载调整 GC 参数。本轮没有改变 GC 配置。 |
| 复用 `http.Client` 和 `Transport`，连接池容量与并发上限是不同概念 | [S3] Go `net/http` | 当前已有共享 Transport、代理 Transport 缓存及每主机空闲连接配置；后续结合连接复用率、握手次数和上游限制调整。 |
| SSE 应及时刷新；慢客户端需要背压和有界缓冲 | [S4] Caddy Reverse Proxy、[S5] Envoy Flow Control | 保留逐帧写入与 Flush、正文大小限制和取消传播。避免使用无界队列提前读完整个上游流；部署压测也要覆盖外层代理。 |
| WAL 支持读写并发，但仍只有一个写者；连接池过小会让请求等待 | [S6] SQLite WAL、[S7] Go Database Connections | 当前单连接同时承担读取和写入。下一步先测 `DB.Stats()` 的 `WaitCount` / `WaitDuration`，再评估独立读连接池；计费和配额事务需要继续保持原子性。 |
| PGO 依赖具有代表性的 CPU profile | [S8] Go PGO | 等实际网关流量的热点稳定后，再评估 PGO。单个无网络微基准不足以代表生产负载。 |
| Go 1.26.2 的 `encoding/json/v2` 仍属于实验接口 | [S9] Go 1.26.2 `encoding/json/v2` | 本轮继续使用现有标准库接口，复用解码结果；更换 JSON 实现需要单独验证数字精度、未知字段、错误处理和协议兼容性。 |
| Responses WebSocket 使用长连接和响应续接，适合多轮工具任务 | [S10] OpenAI WebSocket Mode、[S11] Codex Configuration Reference | 已确认外层网关缺少 `GET /v1/responses`；内置 CLIProxyAPI 有对应入口，但当前 HTTP 转发无法触发其 Codex WS 执行器。接入方案见下文，尚未实现。 |

“每条 SSE 只解析一次”和“只复制选中的配置”是结合上述原则及本项目热点提出的具体方案，并非外部资料对本项目收益的保证。

## 本轮已完成

- 原生 SSE 的终止检查、前导事件识别、保活状态及计费共享同一个解码对象；原生流聚合也复用解码对象，保留 `UseNumber` 的数字精度。
- 路由候选保存索引，常见规模使用栈内数组；最终返回一份独立配置，保持管理端修改与请求配置之间的隔离。
- 复用 SSE 数据行切片，并清除其中的字符串引用。
- 增加四协议转发及聚合基准，以及多行 SSE、畸形事件提交边界、关闭终止校验、65 个候选上游与配置副本的回归检查。

验证：`go test ./... -count=1` 通过。

## 实测结果

环境：Windows/amd64，Go 1.26.2，Intel Core Ultra 7 265K，默认 `GOMAXPROCS=20`。

同一机器、相同输入，`-benchtime=300ms -count=3`，耗时取三次中位数。每条流含 1,024 个文本增量，每个增量为 5 字节；路由场景包含 32 个上游，每个上游配置 128 个模型。转发基准写入丢弃型 ResponseWriter，不含真实网络或下游阻塞。

| 场景 | 优化前 | 优化后 | 耗时下降 |
| --- | ---: | ---: | ---: |
| 路由选择 | 4.986 μs | 0.796 μs | 84.0% |
| Chat 原生转发 | 2.411 ms | 1.188 ms | 50.7% |
| Responses 原生转发 | 2.547 ms | 1.283 ms | 49.6% |
| Anthropic 原生转发 | 2.917 ms | 1.605 ms | 45.0% |
| Gemini 原生转发 | 3.565 ms | 1.968 ms | 44.8% |
| Chat 原生聚合 | 6.021 ms | 3.503 ms | 41.8% |
| Responses 原生聚合 | 4.994 ms | 3.333 ms | 33.3% |
| Anthropic 原生聚合 | 4.782 ms | 3.208 ms | 32.9% |
| Gemini 原生聚合 | 5.910 ms | 2.827 ms | 52.2% |

路由每次累计分配从 16,080 B 降至 240 B，分配次数从 6 次降至 1 次。四协议原生转发每条流的累计分配减少约 41%–46%，聚合减少约 26%–52%。这些结果描述本机基准中的处理开销，不代表整体请求延迟或生产吞吐等比例变化。

本机原始结果位于 `output/performance-before.txt`、`output/performance-after.txt` 和 `output/performance-comparison.json`；`output/` 为 Git 忽略的本地测量产物。

## 下一步优先级

1. **Codex Responses WebSocket 接入。** 当前外层入口缺失；优先验证原生 Responses 上游和 CLIProxyAPI 账号池的端到端长连接，分别测多轮工具任务的首事件延迟、请求字节数和总耗时。
2. **原生 Responses 聚合的字符串拼接。** 优化后的 `alloc_space` 采样中，`collectNativeResponsesSSE` 内逐增量拼接文本所在行约占总分配的 59%。优先评估按输出项、正文位置维护追加缓冲，覆盖交错工具参数、reasoning、done 快照和终态补全，并用不同流长度确认增长趋势。
3. **数据库连接等待。** 在真实鉴权、日志、仪表盘和计费混合负载下测量连接等待，再决定是否拆分读写连接。增加连接数本身不能消除 SQLite 的单写者限制。
4. **部署环境下的延迟和内存。** 覆盖并发长流、慢下游、取消、重试和外层代理，记录首 token、P95/P99、CPU、存活堆、goroutine 与连接复用率。使用代表性的生产 CPU profile 再评估 PGO。

## Codex WebSocket 专项核查

截至 2026-10-23，官方文档确认 Responses API 提供 `/v1/responses` WebSocket 模式，Codex 自定义 provider 有 `supports_websockets` 配置。长连接内可用 `previous_response_id` 续接并只提交新增输入；实际收益需要用多轮工具任务测量，不能从本文件的 SSE 微基准推算。

### 当前断点

| 层 | 核查结果 |
| --- | --- |
| Codex 客户端 | 官方配置有 `model_providers.<id>.supports_websockets`；它声明 provider 能力，不会替网关增加路由。 |
| API Share 外层 | `cmd/apishare/main.go` 只注册 `POST /v1/responses` 和 `POST /v1/responses/compact`。`cmd/apishare/realtime.go` 的 WebSocket 路由为 `/v1/realtime`。 |
| 内置 CLIProxyAPI v8.0.38 | 模块内 `internal/api/server_routes.go` 已注册 `GET /v1/responses`，由 `ResponsesWebsocket` 处理。 |
| CLIProxyAPI Codex 上游 | `internal/runtime/executor/codex_websockets_executor.go` 的 `CodexAutoExecutor` 要求下游为 WebSocket，且所选凭据启用 `websockets`；否则走 HTTP。因此仅有依赖库、仅修改客户端开关或仅启用账号开关都不能接通当前外层链路。 |

### 接入时需要一起完成的行为

- 为原生 Responses 上游和 CLIProxyAPI 账号池增加已鉴权的 `GET /v1/responses` 代理，复用代理出站配置，并隔离客户端 Key 与上游 Key。
- 从首条创建事件确定模型和上游；后续每轮重新检查密钥状态、模型权限、QPS、并发与配额，空闲连接不应永久占用推理并发槽。
- 同一续接链保持上游及账号状态；仅在明确允许的边界重连或回退。遇到缺失的上一轮响应时，按协议通知客户端恢复完整上下文，不能把相同 `previous_response_id` 盲目转交其他账号。
- 按每个响应的终止事件及 usage 独立记账，并处理重复事件。CLIProxyAPI 当前 HTTP 请求的 `task.traceID` 也不能直接提升为整个多轮连接的唯一计费键。
- 验证保活、取消、交错事件、预热请求、断线、额度耗尽和模型映射；普通 Chat / Anthropic / Gemini 渠道继续通过现有 HTTP 转换路径提供服务。

该专项本次完成的是官方协议与源码核查，没有修改网关路由或用户的 Codex 配置，也没有产生真实上游调用。接通并验证端点后，才适合在对应 Codex provider 上声明 `supports_websockets = true`。

## 复现

在仓库根目录执行：

```powershell
go test ./...
go test ./cmd/apishare -run '^$' -bench 'Benchmark(NativeRelayStream|NativeCollectStream|RoutingSelection)$' -benchmem -benchtime=300ms -count=3

New-Item -ItemType Directory -Force output | Out-Null
go test ./cmd/apishare -run '^$' -bench '^BenchmarkNativeCollectStream/openai-responses$' -benchtime=1s -memprofile output/native-collect.mem.prof -o output/native-performance.test.exe
go tool pprof -top -alloc_space output/native-collect.mem.prof
go tool pprof -alloc_space -list 'collectNativeResponsesSSE.func1' output/native-collect.mem.prof
```

## 一手资料

- [S1] Go Diagnostics：`https://go.dev/doc/diagnostics`
- [S2] A Guide to the Go Garbage Collector：`https://go.dev/doc/gc-guide`
- [S3] Go `net/http`：`https://pkg.go.dev/net/http#Transport`
- [S4] Caddy `reverse_proxy` / Streaming：`https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming`
- [S5] Envoy Flow Control：`https://www.envoyproxy.io/docs/envoy/latest/faq/configuration/flow_control`
- [S6] SQLite Write-Ahead Logging：`https://sqlite.org/wal.html`
- [S7] Managing Connections：`https://go.dev/doc/database/manage-connections`
- [S8] Go Profile-guided Optimization：`https://go.dev/doc/pgo`
- [S9] Go 1.26.2 `encoding/json/v2`：`https://pkg.go.dev/encoding/json/v2@go1.26.2`
- [S10] OpenAI Responses WebSocket Mode：`https://developers.openai.com/api/docs/guides/websocket-mode`
- [S11] Codex Configuration Reference：`https://developers.openai.com/codex/config-reference/`
