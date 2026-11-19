# 性能优化经验与基准

基础整理：2026-10-23；本页最后增量更新：2026-11-19。以下结论结合 Go、Caddy、Envoy、SQLite 的一手资料和本仓库实测；外部项目的性能数字不作为 API Share 的收益承诺。

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
| Responses WebSocket 使用长连接和响应续接，适合多轮工具任务 | [S10] OpenAI WebSocket Mode、[S11] Codex Configuration Reference | 已接入 `GET /v1/responses`，覆盖原生 WS 上游及 CLIProxyAPI 账号池；真实 Codex 工具回路已确认连接复用和增量续接。 |

“每条 SSE 只解析一次”和“只复制选中的配置”是结合上述原则及本项目热点提出的具体方案，并非外部资料对本项目收益的保证。

## 已完成的 SSE 与路由优化

- 原生 SSE 的终止检查、前导事件识别、保活状态及计费共享同一个解码对象；原生流聚合也复用解码对象，保留 `UseNumber` 的数字精度。
- 路由候选保存索引，常见规模使用栈内数组；最终返回一份独立配置，保持管理端修改与请求配置之间的隔离。
- 复用 SSE 数据行切片，并清除其中的字符串引用。
- 原生 SSE 透传在下一次阻塞读取上游前刷新，合并已到达的事件；原生 Chat、Anthropic 和 Responses 聚合用追加缓冲维护增量文本，避免每次复制整个前缀，并在提供方终态快照出现时保留其权威值。
- 增加四协议转发及聚合基准，以及多行 SSE、畸形事件提交边界、关闭终止校验、65 个候选上游与配置副本的回归检查。

### 增量文本聚合与原生 SSE Flush（2026-11-19）

- 原生响应聚合中 Chat 文本/推理/拒绝、音频字段、函数名与参数、Anthropic 文本/思考/签名/压缩块与工具 JSON、Responses 文本/推理摘要/拒绝/工具参数均按字段独立追加。Responses 的 done 和 content-part 快照仍能覆盖不完整增量，并继续补全最终快照。
- 原生 SSE 透传将待发字节合并至下一次向上游阻塞读取之前刷新；结束和错误路径也刷新已提交数据。没有使用定时攒帧，因此不必等待下一条尚未到达的事件才交付已到达帧。
- 新增回归：同一上游读取内的多事件合并一次 Flush，且上游等待客户端收到这批数据后才继续；另验证 Responses 增量、done 快照与其后续增量组合、Unicode 和字段追加分配次数随流增长为次线性。

Responses 原生聚合 1,024 个增量的本机基准，同一 Windows/amd64 Go 1.26.10 环境及同一 `-benchtime=200ms -count=1` 命令：

| 场景 | 优化前 | 优化后 | 变化 |
| --- | ---: | ---: | ---: |
| 耗时 | 2.953 ms/op | 1.690 ms/op | 单次基准，受机器即时负载影响 |
| 累计分配 | 4,695,472 B/op | 1,892,639 B/op | 减少 59.7% |
| 分配次数 | 30,916 allocs/op | 29,898 allocs/op | 减少 3.3% |

另外的 `alloc_space` 采样中，重复增量字符串拼接由原先主热点降为非主项；事件 JSON 解码与 map 分配成为主要热点。Flush 合并不改变本机丢弃型 Writer 基准的写入模型，因此这组耗时及分配数字只度量聚合路径，不能用来宣称 Flush 或生产端到端时延的收益。应以真实网络和慢客户端继续测首帧、吞吐与 Flush 次数。

### 请求解析复用、转换流 Flush 与首输出期限（2026-11-19）

- 请求解析把格式专用 RawMessage 解析与转换能力检查共享同一份顶层字段映射；能力检查只解码相关字段，不再为整份请求重新构造任意深度 `map[string]any`。未知的原生扩展字段继续保留在原始请求体中；浮点之外的大整数仍以 `json.Number` 处理。
- 同协议转换流也采用上游读取边界 Flush；普通本地生成的 `protocolStream` 路径维持即时 Flush。测试确认同批多帧合并，且客户端收到第一批数据之前上游可以等待，不会产生人为攒帧延迟。
- 新增 `UPSTREAM_FIRST_OUTPUT_TIMEOUT_SEC`（默认 300 秒，`0` 关闭）。它从每次上游请求开始到第一个语义输出计时；初始化帧/保活不重置计时，输出/推理/工具数据或有效终态会取消该上游尝试的期限。空闲期限仍独立工作，健康的长流不会受首输出期限影响。
- 首输出超时触发的备用切换有独立的一次预算：`retry=0` 仍可试一个备选；如果备选也在首输出期限内卡住，就停止继续沿池切换。达到期限前已产生语义输出的流不会重放。

解析基准使用带 64 KiB 不透明扩展字段的 Chat 请求，在 Windows/amd64、Go 1.26.10、Intel Core Ultra 7 265K 上运行 `-benchtime=300ms -count=3`，耗时取中位数：

| 路径 | 优化前：旧完整解码流程 | 优化后：RawMessage 字段复用 | 变化 |
| --- | ---: | ---: | ---: |
| 耗时 | 488.5 μs/op | 177.7 μs/op | 减少 63.6% |
| 累计分配 | 404,856 B/op | 78,232 B/op | 减少 80.7% |
| 分配次数 | 107 allocs/op | 74 allocs/op | 减少 30.8% |

上游首输出期限和网络切换由阻塞读取、停止进展与 `retry=0` 的多渠道夹具测试覆盖；这些测试不是生产线路压测。Flush 合并效果以调用次数和首批交付边界测试验证，不用内存 `ResponseRecorder` 推断生产端的系统调用或 P99。

验证：`go test ./... -count=1` 通过。

## SSE 与路由微基准

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

1. **部署环境的 WebSocket 对比。** 接入和本地回路验证已完成；继续用实际原生上游、同一任务及相同账号，对比 HTTP 与 WS 的首事件延迟、请求字节数和总耗时。
2. **原生流热路径的剩余 JSON 解码与 map 分配。** 增量文本追加缓冲已完成。对原生 Responses 与其他聚合/透传事件，需先按真实 profile 细化可跳过的解码，再保持 usage、首 token、终止校验、工具识别和重试边界不变。
3. **数据库连接等待。** 在真实鉴权、日志、仪表盘和计费混合负载下测量连接等待，再决定是否拆分读写连接。增加连接数本身不能消除 SQLite 的单写者限制。
4. **部署环境下的延迟和内存。** 覆盖并发长流、慢下游、取消、重试和外层代理，记录首 token、P95/P99、CPU、存活堆、goroutine 与连接复用率。使用代表性的生产 CPU profile 再评估 PGO。

## Codex WebSocket 实现与验证

截至 2026-10-23，官方文档确认 Responses API 提供 `/v1/responses` WebSocket 模式，Codex 自定义 provider 有 `supports_websockets` 配置。长连接内可用 `previous_response_id` 续接并只提交新增输入；实际收益需要用多轮工具任务测量，不能从本文件的 SSE 微基准推算。

### 已接通的链路

| 层 | 当前行为 |
| --- | --- |
| Codex 客户端 | 在原生 WS 模型对应的 provider 中声明 `supports_websockets = true`，见 [接入配置](CLIENTS.md#responses-websocket)。 |
| API Share 外层 | `GET /v1/responses` 进入 `responses_websocket.go`；保留现有 POST、compact 与 Realtime 入口。 |
| 内置 CLIProxyAPI v8.0.38 | 模块内 `internal/api/server_routes.go` 已注册 `GET /v1/responses`，由 `ResponsesWebsocket` 处理。 |
| CLIProxyAPI Codex 上游 | `CodexAutoExecutor` 要求下游为 WS、所选凭据启用 `websockets`。管理员账号设置 API 支持修改该标志，重新授权与迁移保留它；未启用时 SDK 仍可能使用 HTTP。 |

### 实现边界

- 首条创建事件决定模型和上游。原生 WS JSON 透传；模型别名修改使用 RawMessage，保留大整数、工具参数及不透明字段。复用已有 HTTP(S) / SOCKS 出站代理配置，客户端 Key 和握手状态不传入上游。
- 每轮重新读取 Key、共享 / 独立配额和权限，检查 QPS 与并发；终止时释放并发槽。限流器配置更新保留旧请求的活跃计数，避免编辑 QPS 后绕过并发。
- 只在尚未发送输入的握手阶段尝试其他原生候选。发送后固定上游，直接保留续接 ID；网关不缓存或重建完整对话，不重放已接受的输入。
- 连接内按响应 ID 去重终止事件，再逐轮记录 usage 和扣费。缺少 usage 时不估算增量输入，零用量预热不生成推理账单；拒绝的创建请求记录失败日志。
- 客户端断开或写入失败后，默认继续读取上游最多 1.5 秒，尽力保留迟到的最终 usage；不再接受新的创建请求。带有已知响应 ID 的 `error` 先保留，等待随后的 `response.failed` 权威用量；只有错误而无终态时，经过 1.2 秒短窗口释放该轮。
- 单个处理循环管理请求、事件和账单；双向读取队列有界。独立 goroutine 处理协议 ping/pong；存活到期先发送 `1001` 关闭帧，再取消读取。连接数、待完成请求、事件大小及已记录响应 ID 数均有上限。
- 当前支持创建、追加、取消、中断和按 `stream_id` 关联交错事件；不开放 `response.steer` 自动续轮。HTTP 协议转换继续使用原入口；仅支持 HTTP 的兼容商不能通过此入口获得原生上游 WS。

### 本地验证

- Windows 上安装的 Codex CLI `0.162.1`，在临时 `CODEX_HOME` 中连接本地确定性模型夹具，执行“创建文件 → 运行失败测试 → 修复 → 测试通过”的真实工具回路。
- 该回路的 **6 次 `response.create` 共用 1 条 WS 连接，其中 5 次携带增量续接 ID**。这验证了传输与续接实际启用，未做真实模型服务的延迟 A/B 对比。
- 内置 CLIProxyAPI v8.0.38 连接本地 Codex WS 提供商，连续两轮只建立 1 条上游连接，确认 SDK 的 WS 执行器及连接复用生效。
- 回归覆盖模型映射、数字精度、交错与排队、重复终止事件、动态权限、Key 轮换、限流器更新、独立额度、预热、未知 usage、握手前重试、输入发送后断线、代理、双向 ping/pong 与连接到期关闭。

上述回路使用本地响应夹具；不将工具进程执行时间或本地网络耗时解释为真实模型加速比例。用户的现有 Codex 配置与账号开关由用户按需启用。

### 开源实现对照

核对 sub2api 提交 `5c185c5c5d4f5e409e2139806e0964ae944e03e8` 的 `backend/internal/service/openai_ws_v2/passthrough_relay.go` 与 `openai_ws_v2_passthrough_adapter.go`，以及本项目固定依赖 CLIProxyAPI v8.0.38 的 WS handler / executor。

| 重点问题 | 本项目处理与验证 |
| --- | --- |
| 长连接后续轮次绕过权限或额度 | 每个创建事件重新读取权限、余额、QPS 和并发；Key 停用、轮换与配置变化都有回归。 |
| 客户端断开导致尾部用量丢失 | 有界 drain 窗口；覆盖正常关闭与 TCP 中断后的迟到 usage，不估造窗口外的用量。 |
| `error` 与 `response.failed` 变成两个账单，或先结算错误丢失最终用量 | 已知轮次的裸错误暂存；最终失败用量替换错误兜底，单独错误可超时结算。辅助事件不会提前结束等待。 |
| 事件 ID 被误当成响应 ID | 顶层 `id` 仅用于终止快照兼容；普通事件和错误使用明确的响应关联字段。兼容扁平终态但保持原生 JSON 不变。 |
| 上游 `1000` / EOF 被当成推理成功，或所有关闭码都被改成 `1011` | 活跃轮次缺少终态记失败；保留真实 `1008` / `1013` 等关闭码。已明确返回的客户端请求错误不计作上游故障。 |
| 取消控制命令影响其他连接 | 有响应 ID 的取消 / 中断只能引用当前连接已知的响应；未配套逐轮票据的自动续轮事件被拒绝。 |
| 池内 WS 并未实际启用 | 用本地提供商验证真实 SDK 的 WS executor，确认两轮复用上游连接；账号开关兼容布尔和旧版字符串值。 |

参考源码见 [S12]、[S13]。这些对照不等于真实厂商延迟压测，实际效果仍需按部署环境测量。

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
- [S12] sub2api 固定版本 WS Relay：`https://github.com/Wei-Shaw/sub2api/blob/5c185c5c5d4f5e409e2139806e0964ae944e03e8/backend/internal/service/openai_ws_v2/passthrough_relay.go`
- [S13] sub2api 固定版本 Passthrough Adapter：`https://github.com/Wei-Shaw/sub2api/blob/5c185c5c5d4f5e409e2139806e0964ae944e03e8/backend/internal/service/openai_ws_v2_passthrough_adapter.go`
