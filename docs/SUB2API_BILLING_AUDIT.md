# 细分用量与扣费排查

本轮把主要缓存、推理和工具提示 Token 接入扣费、额度和日志。标准化响应与扣费使用同一个分桶函数；各桶互不重叠，不能把已包含在总量里的缓存或推理再次加上去。

改动保留在工作区，没有更新运行中的程序或实际数据库。新增 12 个回归测试函数，按用户要求未执行测试或真实模型调用；静态检查、程序编译和前端语法检查通过。数据库迁移、事务回滚和浏览器展示尚未运行验证。

- [用量归一化与费用计算](../cmd/apishare/billing_usage.go)
- [日志迁移和原子扣费](../cmd/apishare/billing_store.go)
- [回归定义](../cmd/apishare/billing_audit_test.go)
- [全格式兼容边界](SUB2API_ALL_FORMATS_AUDIT.md)

## 已处理的供应商口径

| 场景 | 计算规则与依据 |
|---|---|
| 标准 Anthropic Messages | input_tokens 是普通输入；缓存读写另列，计入输入总量但各按自己的单价。5 分钟和 1 小时创建明细归属同一个创建总量，不能重复计费。[官方口径](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) |
| Kimi / GLM 的 Messages 兼容响应 | 返回 prompt_tokens 总量时，按总量拆分缓存，不再无条件把缓存加到 input_tokens 上。缓存读取兼容顶层 cached_tokens 和嵌套 prompt_tokens_details.cached_tokens。流式中后续明确的独立输入桶会清除早先总量别名，聚合 JSON 也清除同一旧别名，避免最终快照把过期总量重新带回来。[sub2api 回归](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/kimi_anthropic_usage_test.go) |
| DeepSeek 缓存命中/未命中桶 | 兼容 prompt_cache_hit_tokens 与 prompt_cache_miss_tokens；Messages 以明确 miss 桶作为普通输入，Chat 以 prompt 总量拆分 hit。没有 prompt 总量、但 hit/miss 完整时可恢复真实输入，不估算。[官方定义](https://api-docs.deepseek.com/api/create-chat-completion/) |
| Gemini | promptTokenCount 已含缓存；工具提示桶另列。candidatesTokenCount 与 thoughtsTokenCount 是两种输出量，推理计入输出总量并单独定价；不能遗漏思考，也不能重复加缓存。[响应定义](https://ai.google.dev/api/generate-content?hl=en) |
| 常规 OpenAI Chat / Responses | 输入总数拆出缓存；输出通常已经包含 reasoning_tokens，正文与推理分桶后总数保持不变。 |
| Grok / xAI 的独立推理计数 | 仅在 total_tokens 与输入、可见输出和推理严格对应时，把独立推理补入输出；没有总数证据不盲加。兼容 Responses 返回 prompt_tokens/completion_tokens 及对应 details 的变体。[官方 Chat 示例](https://docs.x.ai/developers/rest-api-reference/inference/chat-completions)、[sub2api 对照](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/pkg/xai/usage.go) |
| 真实 0、缺失或非法计数 | 有效数字 0 覆盖旧累计值；缺失、null、负数、分数和非数值不冒充真实 0。累计分项按字段更新，失效的旧 total 不用于证明后续推理独立计数。 |
| 总量与缓存时长明细矛盾 | 创建总量优先，明细超过总量时按比例缩小；显式总量 0 同样优先。未返回总量时可从有效时长明细恢复，未说明时长的余量独立记录，以普通创建价计费。[sub2api 约束](https://github.com/Wei-Shaw/sub2api/blob/9c2cca9e627291bc719bada3d57de23b625a4180/backend/internal/service/billing_service.go) |
| 缺失 usage 时估算 | 估算值覆盖完整输入或输出，已知缓存/推理子桶不能再额外添加。保留哪些部分是估算的标记；新日志能展开查看。 |

对照 sub2api 的字段兼容和缓存时长约束，APISHARE 保留了有效零值的存在性，并对独立推理采用严格总量对应条件。没有引入按模型名称猜测全部推理计数的规则。

## 定价和后台配置

### DeepSeek Flash 有记录但零扣费的修复（2026-10-14）

用户报告 DeepSeek 4.1 Flash 有调用记录但不扣费，并要求所有名称含 DeepSeek 和 Flash 的模型都按 `deepseek-flash` 计价。代码检查确认：原先按原始请求名查询价格；价格库未收录该名称、没有手动覆盖且内置表也没有对应项时，会落到默认零价，有日志但不扣费。另一种显示问题是小于 $0.00005 的真实费用被四位小数显示成零。没有用户生产日志的费用值，因此不能断言用户实例属于哪一种情况。

现将名称同时含 `deepseek` 和 `flash`（不区分大小写）的模型统一转换为计价名 `deepseek-flash`，涵盖 `deepseek-v4-flash`、`deepseek-v4.1-flash`、视觉实验名称、供应商前缀和未来 Flash 名称。仅变更价格查询，路由与额度池标识保持原请求模型；管理员给 `deepseek-flash` 的单价完整优先，其他 Flash 名称下的单独手动价格不再覆盖统一单价。

2026-10-14 读取的 [LiteLLM 价格源](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) 收录 `deepseek-flash`、`deepseek-v4-flash` 与视觉实验名称，未收录字面名称 `deepseek-v4.1-flash`。[DeepSeek 官方定价](https://api-docs.deepseek.com/quick_start/pricing/) 将 `deepseek-flash` 标为 V4.1 Flash，并说明旧 Flash/视觉实验名称按 Flash 价格服务。离线内置兜底采用该日峰时快照：普通输入 $0.30、输出 $1.20、缓存读取 $0.006 / 1M tokens。同步价和手动价优先；网关仍使用固定单价，不自动推算峰谷时段或节假日折扣。

无专用价格的自定义别名，先查询实际映射的上游模型价格，再回落 `default`；非 Flash 客户模型明确配置零价时，仍尊重该免费覆盖。调用日志保存计价模型、实际上游模型、共享/独立额度池，可展开核对。后台单价查询显示统一计价名，后台和余额页对微小金额自适应增加精度，避免显示成零。

新增 3 个回归函数覆盖统一名称的手动/自动/离线定价、别名映射与明确免费覆盖，以及统一单价扣到原请求模型的共享/独立池。回归定义中以小于 $0.00005 的正费用核对日志、累计使用与模型池，同时确认没有误建 `deepseek-flash` 额度池；未执行这些测试。旧日志与旧余额不追溯改价或补扣。

所有单价单位为 USD / 1M tokens，以下字段均属于某个模型的价格对象：

| 字段 | 用途 | 缺省行为 |
|---|---|---|
| in | 普通输入与 Gemini 工具提示 Token | 沿用现有价格选择规则 |
| out | 正文输出 | 沿用现有价格选择规则 |
| cache_read | 缓存读取 | 使用 in |
| cache_write | 普通缓存创建，包括未说明时长的部分 | 使用 in |
| cache_write_1h | 明确的 1 小时缓存创建 | 使用 cache_write；后者未填则使用 in |
| reasoning | 推理 Token | 使用 out |

可选字段缺失或为 null 表示继承，明确填写 0 表示免费。手动模型价格仍完整优先于自动定价，DeepSeek Flash 家族统一读取 `deepseek-flash` 的手动配置；不把管理员设置的价格与不同来源的缓存折扣混合。前后端拒绝非有限数、负单价、空模型价格对象类型和拼错的价格字段。

自动价格同步增加读取 LiteLLM 的 cache_read_input_token_cost、cache_creation_input_token_cost、cache_creation_input_token_cost_above_1hr 与 output_cost_per_reasoning_token，并保留可选字段和有效 0。旧价格缓存仍可离线使用基础价，但启动时安排更新，避免把升级前丢失缓存分项的快照继续视为最新。[价格字段 Schema](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.schema.json)

后台单价查询显示这些分项的有效价格；价格 JSON 编辑器保留现有布局，错误提示关联原字段。新调用日志使用键盘可操作的原生 details/summary 展开用量和费用，历史日志没有新快照时不补造明细。

## 日志和额度一致性

新日志的 prompt_tokens 是普通输入、缓存读写和工具提示之和；completion_tokens 是正文与推理之和。站点统计沿用这两个总量字段，不再遗漏新调用的缓存或推理。旧日志的总量和费用保持原值，历史汇总不会被重新核算。

每条新日志保存原始已解析计数、归一化后的桶、估算标记、当次价格及来源、费用分项。日后修改价格不会改变旧记录的计算依据。billing_json 列以幂等迁移添加，旧行保持没有快照的状态。

额度扣除与日志插入使用同一事务。共享池/独立模型池仍按请求入站时选定的规则扣除；任一写入失败会一起回滚，并记录服务日志。已开始的流中断仍按现有规则记录失败和可采集/估算用量，不因这一轮改动重放工具或重新发起模型请求。

## 尚未接入的费用

长上下文分档、优先级/地区/加速倍率、服务端工具按次费用、缓存存储时长费用、图片/音视频分模态价格尚未接入。这一轮支持主要 Token 分类，不代表全部供应商账单项目完全等价。[Claude 分档和工具计费](https://platform.claude.com/docs/en/about-claude/pricing)、[Gemini 定价类别](https://ai.google.dev/gemini-api/docs/pricing)

特殊接口只给模糊总数、分项矛盾或完全缺少有效用量时，仍不能恢复供应商真实账单；回退价格与估算会在配置或日志中可见。恢复运行验证时，应优先检查新旧数据库迁移、失败回滚、独立配额、零单价、时长混合缓存和长流的累计更新，再观察真实供应商扣费结果。
