# Claude Code ↔ OpenAI 格式桥接通用化迁移计划

## 背景

当前项目中的 `cmd/claude-codex-proxy` / `internal/claudecodexproxy` 已经可以把 Claude Code 的 Anthropic Messages 请求转换成 OpenAI Responses 风格请求，并在兼容的 OpenAI-format 后端上完成基本可用的桥接。

但目标不应绑定某一个站点或默认后端，而应当是：

> **只要后端提供“足够兼容的 OpenAI 格式接口/能力”，就能通过本工具让 Claude Code 尽可能正常地使用 coding / MCP / subagent / team 能力。**

参考仓库：

- 本地：`D:\IdeaProjects\copilot-api-test`
- 远程：`https://github.com/caozhiyuan/copilot-api`

该参考仓库虽面向 GitHub Copilot，但其中有不少 **Claude Code 语义兼容层** 是通用可迁移的。

---

## 通用桥接目标

### 目标

为 Claude Code 提供一个 **通用的 Anthropic → OpenAI-format bridge**，要求：

1. 不与任何单一站点或供应商强绑定
2. 尽量保留 Claude Code 的：
   - tool use
   - MCP
   - deferred tools / ToolSearch
   - subagent / team / task
   - streaming
   - thinking / compact / count_tokens
3. 对“只兼容部分 OpenAI Responses 语义”的后端提供降级路径

### 非目标

1. 不复制 GitHub Copilot 的认证、用量、企业路由体系
2. 不要求所有 OpenAI-format 后端都能完整支持 Claude Code 全量能力
3. 不承诺没有 `messages` / `responses` / SSE / reasoning 语义的后端也能工作

---

## 兼容级别建议

### Level 0：最小文本桥接

后端要求：

- 支持 `POST /v1/responses`
- 支持普通文本输入输出

能力：

- 普通问答
- 非流式文本返回

### Level 1：Claude Code 基础 coding 可用

后端要求：

- 支持 `POST /v1/responses`
- 支持 function calling
- 支持 SSE 或允许非流式 fallback

能力：

- tool use / tool result
- assistant history
- system → instructions
- `/v1/models`

### Level 2：MCP / deferred tools 可用

额外要求：

- function call 参数与输出语义稳定
- 对 tool_result / tool_reference / mixed content 宽容

能力：

- MCP 工具
- ToolSearch / deferred tools
- 复杂 tool_result follow-up

### Level 3：subagent / team / long-running workflows

额外要求：

- 更完整模型能力广告
- 更稳的 count_tokens / compact / thinking 处理
- 更好的 session/subagent continuity

能力：

- Claude Code agent team
- task / send-message / team-create
- 更稳定的长会话与分工协作

---

## 参考仓库中值得迁移的能力

### 已完成迁移

以下能力已移植到当前 `claude-codex-proxy`：

1. **更丰富的 `/v1/models` 能力广告**
   - `supported_endpoints`
   - `capabilities.supports.*`
   - `limits` / `vendor` / `version`
   - 后端 `/v1/models` 最佳努力透传，失败时回退本地合成模型描述

2. **Anthropic `system` → Responses `instructions`**

3. **assistant 历史文本映射为 `output_text`**

4. **流式 function_call 参数去重**
   - 避免 `delta + done` 重复拼接导致 `Invalid tool parameters`

5. **`tool_reference` 兼容**
   - 转成 `Tool <name> loaded`

6. **结构化 tool result 解包**
   - 例如 `{"result":"..."}` → 纯文本

7. **tool_result / text 预处理**
   - 剥离 `Tool loaded.` 边界文本
   - 把同轮 `text` 合并进 `tool_result`

8. **基础 subagent marker 解析**
   - 识别 `__SUBAGENT_MARKER__...`

---

## 仍值得迁移的高价值能力

### P1：`count_tokens` 特殊处理

参考位置：

- `src/routes/messages/count-tokens-handler.ts`

建议迁移内容：

1. Claude 模型时优先走真实 `/v1/messages/count_tokens`
2. 对 `mcp__*` / `Skill` 场景做 token 估算修正

价值：

- 直接影响 compact / deferred tools / MCP 长会话稳定性

当前状态：

- 已完成 **Claude 模型优先走真实 `/v1/messages/count_tokens`** 的可选支持
- 已完成 Claude fallback 估算倍率配置
- 已完成基础的 `mcp__*` / `Skill` 场景估算修正，避免为 deferred tools 额外叠加固定 tool system overhead

### P1：结构化 `tool_result` 与 tool schema normalization

建议迁移内容：

1. 保留结构化 `tool_result`，避免过早 flatten 成纯文本
2. 统一 `tool_result.is_error` / `status` 语义，让失败、取消、完成状态可以被后续流程稳定消费
3. 对 Claude Code 与 OpenAI-format 的 tool schema 做归一化，减少后端字段差异带来的兼容分叉

价值：

- 提升 MCP、tool use、deferred tools 和失败重试场景的语义稳定性

当前状态：

- 结构化 `tool_result` 已能在部分场景下保留原始 JSON / block 形态；剩余工作是把“保留结构”前移到统一内部 envelope，减少过早 flatten
- `tool_result.is_error` 到 `status` 的映射已经进入转译链路，但它仍是后端输入项级别的状态，不能直接替代最终业务结果判定
- tool schema normalization 还需要继续收口，目标是让 schema 差异停留在桥接层内部，而不是扩散到后续翻译 / fallback 逻辑

兼容提示：

- 若后端只接受字符串输出，结构化 `tool_result` 仍需要降级为文本
- 若后端严格校验 tool schema 或 `status` 字段，必须保留降级路径，不能把结构化保真当成硬依赖

### P1：补齐 `prepareMessagesApiPayload`

参考位置：

- `src/routes/messages/preprocess.ts`

建议迁移内容：

1. `cache_control` 清洗
2. 无效 thinking block 过滤
3. `tool_choice` 与 thinking 兼容性处理
4. 根据模型能力注入 `thinking` / `output_config`

价值：

- 直接提升 Claude Code 对 messages 语义的兼容度

当前状态：

- 已完成基础的 `thinking` / `output_config.effort` → `reasoning.effort` 映射
- 已完成 invalid thinking 过滤、`cache_control` 清洗与 `tool_choice=any/tool` 时禁用 reasoning 映射的基础兼容

### P2：更完整的流式 Responses → Anthropic 翻译

参考位置：

- `src/routes/messages/responses-stream-translation.ts`
- `src/routes/messages/stream-translation.ts`

建议迁移内容：

1. reasoning_text / reasoning_opaque 交错场景处理
2. 更稳的 tool block / thinking block 开关
3. 结束态与异常态的更完整处理

价值：

- 提升流式 tool / thinking / MCP 稳定性

当前状态：

- 已完成基础增强：
  - `output_item.added` 自带初始函数参数时立即发出 `input_json_delta`
  - `output_text.done` / `content_part.done` 在没有 delta 时回填文本
- 已完成 `response.incomplete` / `response.failed` 的基础流式映射
- 已完成 `output_item.done` 对 message / function_call 的基础 fallback
- 尚未完成完整的 block state machine 与全部异常结束处理对齐

### P2：phase-aware commentary / final_answer

参考位置：

- `src/lib/config.ts`
- `src/routes/messages/responses-translation.ts`

价值：

- 改善 GPT-family 模型在 Claude Code 中的交互体验
- 减少“突然爆工具调用”的割裂感

当前状态：

- phase-aware commentary 是可选增强，不是通用后端的硬前提；默认关闭，开启时只尽量保留 commentary / tool action / result / final_answer 的边界
- 正在向 phase-aware commentary 方向收敛：把 commentary、tool action、result、final_answer 的边界保留下来，再交给后续翻译层决定如何落地
- 兼容风险主要来自两类后端：一类不接受额外 phase hint，另一类虽然接受但会把 phase 当成噪声；这两类都必须能自动回退到现有线性语义

### P2：流式语义保真增强

参考位置：

- `src/routes/messages/responses-stream-translation.ts`
- `src/routes/messages/stream-translation.ts`

价值：

- 提升流式输出在 chunk 边界、终态和异常态上的一致性
- 让 SSE 与非流式 fallback 的用户感知更接近

当前状态：

- 正在补强中，重点不是增加新的后端专用分支，而是提高通用语义保真度

### P3：更完整的 session / initiator 内部状态保真

参考位置：

- `src/lib/api-config.ts`
- `src/services/copilot/create-messages.ts`
- `src/services/copilot/create-responses.ts`

注意：

- 不应直接照搬 Copilot 专属 header
- 但可以借鉴其 **内部状态建模和调试日志思路**

### P1：运行时能力探测 / 自动降级

价值：

- 这是把“通用 OpenAI-format bridge”真正做稳的关键能力
- 后端只要大体兼容 Responses，不需要一次性完整支持 metadata / reasoning / phase / structured tool output / backend stream，也能逐步降级后继续工作

当前状态：

- 已完成并验证：
  - metadata unsupported → 自动重试去掉 metadata，并 sticky disable
  - reasoning unsupported → 自动重试去掉 top-level reasoning，并 sticky disable
  - `include=["reasoning.encrypted_content"]` unsupported → 自动去掉 include 重试
  - phase unsupported → 自动去掉 commentary/final_answer phase hint 重试
  - structured `function_call_output.output` unsupported → 自动降级为文本 output
  - backend `stream=true` unsupported → 自动改走 backend JSON，再翻成 Anthropic SSE
  - request model passthrough unsupported → 自动探测 `/v1/models`，选择 preferred backend model 重试并 sticky 使用
- 当前实现是 **generic capability probing**，不是 rawchat 专属逻辑

剩余风险：

- 当前 capability state 已从“单一全局 sticky”收口到 **request-model / configured-backend-model 作用域**；更进一步仍可继续升级成 backend family / effective backend model 分桶
- 仍以错误文案匹配为主，后续可继续升级为结构化 error classifier + TTL/re-probe
- 当前仓库已完成 TTL-reprobe 第一轮最小落地：
  - `CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL`
  - 对 `metadata`、`reasoning`、`reasoning_include`、`phase`、`stream`、`structured_output`、`prompt_cache_key`、`context_management`、`compaction_input`、`model passthrough` 启用过期后自然重探
  - 仍不把 `parallel_tool_calls` / thinking budget / profile limit 这类 preflight 项写进 TTL 状态
 - 当前仓库已完成 TTL-reprobe 第二轮最小落地：
   - 通过 `reprobeUntil` lease 侧表实现 single-leader reprobe
   - TTL 到期后只有一个 leader 请求重探，其他 follower 继续沿用旧降级路径
   - 不共享响应、不阻塞 follower、不改变现有 adaptive retry 主流程

---

## 2026-04-16 对照参考仓库后的新增迁移判断

在 `responses` 本体翻译、tool_result 兼容、stream fallback、`/v1/models` fallback、count_tokens 基础链路之外，当前最值得继续从参考仓库吸收的能力已经不再是“再堆一层基础翻译”，而是以下通用增强。

### 本轮参考仓库新增且确认值得关注的点

本次重新对照 `D:\IdeaProjects\copilot-api-test` 最新更新，确认参考仓库最近新增/强化了这些实战能力：

1. **compact / auto-continue 检测更细**
   - 新增 `src/lib/compact.ts`
   - 区分 `COMPACT_REQUEST` 与 `COMPACT_AUTO_CONTINUE`
   - compact 请求仍会做 `tool_result + text` 合并，但会 **skip last message**，避免把最终 compact message 误并入上一个 tool result

2. **warmup 无工具请求自动切 small model**
   - `handler.ts` 里在 `anthropic-beta + no tools + 非 compact` 条件下直接切到 small model
   - 这属于高价值降本逻辑，不绑定 raw 或单一站点

3. **session/request continuity 更完整**
   - `parseUserIdMetadata`
   - `getRootSessionId`
   - `generateRequestIdFromPayload`
   - `prepareInteractionHeaders` / `prepareForCompact`
   - 说明参考仓库已经把 subagent / session / compact 的 continuity 走到“稳定请求上下文”层，而不仅是 marker parse

4. **IDE tool sanitize**
   - 删除 `mcp__ide__executeCode`
   - 重写 `mcp__ide__getDiagnostics` 描述
   - 这类桥接层轻量净化对 Claude Code/IDE 场景兼容性有直接帮助

5. **Messages API payload 更贴近 Claude/Copilot 真实行为**
   - adaptive thinking 默认补 `display: summarized`
   - `advanced-tool-use` beta 默认并入 header
   - 说明参考仓库正在收口“请求形状更像真实客户端”，而不是只做协议翻译

6. **模型/能力驱动的 endpoint routing 更明确**
   - 通过 `supported_endpoints` 与 `capabilities.supports.*` 选择 Messages / Responses / Chat Completions 路线
   - 这比单纯 fallback 更适合通用 OpenAI-format bridge

### 高优先级

1. **session / subagent continuity**
   - 把当前已解析的 subagent marker、session 信息真正转成稳定的内部 request id / root session / interaction metadata
   - 目标不是复刻 Copilot header，而是保留 Claude Code team / task / subagent 的连续性
   - 最新参考实现已明确包含 `user_id -> root session -> request id` 这条链，优先级继续维持最高
   - 当前仓库已完成第一轮最小落地：
     - `metadata.user_id` 的 JSON / legacy `user_id` 解析
     - root session id 派生
     - deterministic request id 派生
     - subagent continuity metadata 注入
     - `prompt_cache_key` 注入与 unsupported 自动降级
   - 当前仓库已完成第二轮最小落地：
     - `session affinity` / `parent session` / `inbound request id` / `trace id` / `interaction type` / `interaction id` 的 continuity metadata 注入
     - 仍然受现有 `metadata` capability 控制，不额外引入逐字段 retry
     - 不再把 raw `metadata.user_id`、raw `x-claude-code-session-id`、raw marker session 直接镜像进 backend metadata
     - metadata value 已做基础 trim / 截断 / 控制字符清洗
   - 下一步剩余重点：
     - 评估是否需要为 long-running team / task 流程补更多 continuity header/field
     - 把 continuity metadata 从 `claude_code_*` 前缀进一步抽象成更稳定的 bridge schema（如后续确有必要）

2. **compact / warmup / auto-continue 降本预处理**
   - 识别 warmup / compact / auto-continue 请求
   - 在通用桥接层按能力选择更轻量的 backend model 或更保守的 payload
   - 本轮确认还应包含：compact 时 `mergeToolResultForClaude(skipLastMessage)` 的细粒度规则
   - 当前仓库已完成其中的第一批低风险项：
     - compact request / auto-continue 检测
     - compact request 下的 `skipLastMessage` 合并保护
     - IDE/tool sanitize（删除 `mcp__ide__executeCode`，重写 diagnostics 描述）
     - warmup routing 的第一轮保守实现：仅在 `anthropic-beta + 无 tools + 非 compact + 无固定 BackendModel` 时启用
   - 当前仓库已完成第二轮最小落地：
     - `anthropic-beta + no tools + non-compact` 请求的 warmup model 路由
     - `BackendWarmupModel` 与 `/v1/models` 推断 `WarmupModel` 的最小联动
   - 仍待继续：
     - compact / auto-continue 与 model-driven capability init 的联动策略
     - 更细的 warmup-like 启发式与误判控制

3. **model-driven capability initialization**
   - 不只把 `/v1/models` 当 fallback 数据源
   - 还要把 `capabilities.supports.*` / `supported_endpoints` 真正用于请求构造初值，减少首次 unsupported retry
   - 当前仓库已完成第一轮最小落地：
     - 通过 `/v1/models` 的 `capabilities.supports.*` 预禁用 `reasoning` / `include` / `phase` / backend `stream` / structured tool output
     - 通过 backend-model scope 避免 warmup mini 模型的能力降级污染正式模型
     - 保持 seeded pre-disable 与现有 runtime adaptive retry 可共存
   - 当前启用方式是**显式开关**：`CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT=true`
   - 当前实现细节：
     - 预取 `/v1/models` 后，把 `streaming` / `structured_outputs` / `adaptive_thinking` / `phase` 接入 `optionsForRequest(...)`
     - 对 pinned backend model 与自动 warmup model 都能基于 model profile 做初始裁剪
     - profile 不足时继续保留 runtime capability probing 兜底
   - 当前仓库已完成第二轮最小落地：
     - 读取 `capabilities.limits.max_output_tokens`
     - 在启用 model capability init 时对 `max_output_tokens` 做 profile-driven pre-clamp
     - `flag=false` 时，即便先调过 `/v1/models`，profile 也不会影响消息请求行为
     - 已补覆盖：limit 小于请求值会裁剪、flag=false 不裁剪、profile limit 大于请求值不裁剪、profile limit 缺失时不裁剪
   - 当前仓库已完成第三轮最小落地：
     - 读取 `capabilities.limits.max_prompt_tokens` / `max_context_window_tokens`
     - 当 prompt 规模超过当前 backend model profile 可承载范围时，优先 fallback 到更合适的 backend model
     - `supports.tool_calls=false` 且请求带 tools 时，也会优先 fallback 到更合适 backend model
   - 当前仓库已完成第四轮最小落地：
     - `supports.parallel_tool_calls=true` 且请求带 tools 时才发送 `parallel_tool_calls=true`
     - `parallel_tool_calls` 未声明或为 false 时不发送该字段，避免误伤兼容后端
     - 读取 `supports.max_thinking_budget`，在已启用 model capability init 时对过高 reasoning effort 做保守降级
     - 读取 `supports.min_thinking_budget` / `max_thinking_budget`，仅作为候选 backend model eligibility gate，不自动抬高 reasoning / budget
   - 仍待继续：
     - 更细粒度的 endpoint/profile 初始化
     - thinking budget 精准数值语义（当前 Responses bridge 仍只做 effort 级保守映射和候选过滤）
     - `PreferredModel` 仍是当前默认候选，不是 best-fit router；后续只应谨慎增强，不做激进全量扫描

### 中优先级

1. **stream runaway guard**
   - 为异常 `function_call_arguments.delta` / 空白 runaway 增量添加保护
   - 当前仓库已完成第一轮最小实现：连续空白参数增量超过阈值时中止流并返回 error event
   - 当前仓库已完成第二轮最小实现：
     - `delta after done` 直接报错
     - 超大 tool 参数累计直接报错
     - 冲突性的 duplicate `function_call_arguments.done` 直接报错
   - 当前仓库已完成第三轮最小实现：
     - `excessive empty delta flood` 显式测试
     - `output_item.done` 单包超大参数保护
     - “合法长参数流不误杀”正向测试
   - 后续仍可继续扩成更细的 tool/block 级异常流保护（如更细 provider 顺序兼容）

2. **compaction / context carrier**
   - 当前仓库已完成 compaction / context carrier 的多轮落地：
     - Anthropic `cm1#<encrypted>@<id>` carrier 可转为 OpenAI `input[].type="compaction"`
     - OpenAI `output[].type="compaction"` 可回转为 Anthropic `thinking` block（非流式 / 流式 fallback 均已覆盖）
     - latest compaction input shrinking 已和 `context_management` 注入解耦；后端不支持 `context_management` 时仍保留 shrinking
     - 若后端不支持 `input[].type="compaction"`，会在同一 backend-model scope 内禁用 compaction input，并把最新 compaction carrier 降级为 `reasoning` carrier 后重试
   - 后续仍可继续增强：
     - compaction input fallback 的更多 provider 错误文案识别
     - 完全不支持 reasoning input 的更弱 text/omit fallback 策略（需谨慎，避免泄漏或语义污染）

3. **IDE/tool sanitize**
   - 对明显不适合桥接层直接透传的工具描述做轻量净化，例如 IDE diagnostics 类工具的 schema / description 收口
   - 参考仓库最新已把这条从“建议”推进为默认 preprocess

4. **更精细的 token fallback estimator**
   - 继续保留 Anthropic exact count 优先
   - 但当 exact count 不可用时，fallback 可以更贴近真实 tokenizer 行为

### 低优先级

1. refusal / unknown output content 聚合
2. 多 endpoint family 路由（仅在未来需要兼容 Responses 以外 wire API 时再做）

---

## 明确不建议迁移的内容

以下内容强绑定 GitHub Copilot，不适合作为通用 bridge 的目标：

1. GitHub / Copilot 认证体系
2. Copilot usage dashboard
3. 企业/组织/套餐路由
4. Copilot 专属 header
   - `editor-device-id`
   - `copilot-integration-id`
   - `x-agent-task-id`
   - `vscode-machineid`

---

## 当前对后端的通用要求

为了让 Claude Code 尽可能正常工作，建议后端至少满足：

1. `POST /v1/responses`
2. function calling
3. SSE 或允许 JSON fallback
4. 能接受：
   - `instructions`
   - `input`
   - `tools`
   - `tool_choice`
   - `reasoning`（至少忽略而不是报错）
5. 不要求强依赖 `metadata`
   - 因大量兼容后端会拒绝它

推荐额外支持：

1. `/v1/models`
2. `/v1/messages/count_tokens` 或等价 token counting
3. 更完整的 reasoning / compaction 语义

### 隐私增强模式

当前 bridge 额外提供一个保守隐私开关：

- `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=false`
- 兼容旧变量：`CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING=true`
- 可继续叠加更强的细粒度控制：
  - `CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA=true`
  - `CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY=true`
- 若需要**完整匿名外发模式**：
  - `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`

作用：

- `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=false`
  - 不再转发用户原始 `metadata` map
  - 但仍保留 bridge 派生的 continuity metadata
  - 仍保留顶层 `prompt_cache_key` 能力字段（若后端支持）
- `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`
  - 作为**最高优先级总开关**
  - 不再转发用户原始 `metadata`
  - 不再发送 bridge 派生的 continuity metadata
  - 不再发送顶层 `prompt_cache_key`
  - 不再发送固定 bridge header metadata（如 `x-claude-code-model` / `x-claude-code-config-hash`）
  - 仅关闭**外发**身份线索；内部 continuity 派生仍可保留给代理本身使用

这意味着：

- **兼容模式**：默认不设置该开关，保持现有转发行为（仍受 blocklist/sanitize 约束）
- **隐私增强模式**：设置 `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=false`，仅保留桥接层派生的安全 metadata
- **完整匿名模式**：设置 `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`，覆盖 user metadata / continuity metadata / bridge header metadata / `prompt_cache_key` 的外发
- 若新旧变量同时存在：以 **`CLAUDE_CODE_PROXY_FORWARD_USER_METADATA`** 为准
- 优先级为：`AnonymousMode` > `ForwardUserMetadata / DisableUserMetadataForwarding` > `USER_METADATA_ALLOWLIST`
- `CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST` 为可选增强：仅对白名单中的用户 metadata key 做精确、大小写敏感匹配；不会影响 bridge-derived continuity metadata；且仅在 `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=false` 时有意义
- 若希望进一步降低 session-like 信息暴露：
  - `CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA=true` → 不再发送 bridge-derived `claude_code_*` metadata
  - `CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY=true` → 不再发送顶层 `prompt_cache_key`

---

## 推荐迁移顺序

### 第一阶段（已基本完成）

1. `count_tokens` 特殊处理
2. `prepareMessagesApiPayload` 核心兼容逻辑
3. runtime capability probing / automatic downgrade
4. `/v1/models` passthrough + synthetic fallback

### 第二阶段（建议下一批）

1. session / subagent continuity（第一轮已落地，继续补 interaction continuity）
2. compact / warmup / auto-continue 降本预处理（第二轮最小 warmup routing 已落地，继续补 compact 联动）
3. model-driven capability initialization（第一轮已落地，继续补 limits / endpoint 粒度）
4. IDE/tool sanitize（已落地）

### 第三阶段（增强项）

1. 更完整的流式异常保护
2. compaction / context carrier
3. 更细粒度 observability / TTL / re-probe
4. 更精细的 token fallback estimator

---

## 验证矩阵（摘要）

每一阶段都至少验证：

1. `POST /v1/messages`
2. `POST /v1/messages/count_tokens`
3. `GET /v1/models`
4. system / assistant history
5. tool_result / tool_reference
6. ToolSearch / deferred tools
7. MCP 工具调用
8. team / task / send-message
9. streaming / non-streaming

---

## 当前判断

从最近日志与真实 E2E 看，当前 bridge 已经跨过：

1. MCP 工具调用可用
2. `TeamCreate` / `Agent` / `SendMessage` / `TeamDelete` 链路可用
3. runtime capability probing 的第一轮高价值降级项可用

因此，下一步最值得继续迁移的不再是 rawchat 专属兼容，而是：

1. **session continuity 的第二轮增强（interaction/session affinity）**
2. **model-driven capability initialization 的第二轮增强（limits / endpoint 粒度）**
3. **compact / warmup / auto-continue 的第三轮增强（更细的 warmup-like 启发式）**
4. **compaction / context carrier**
5. **更细粒度的 stream/tool 异常保护**

也就是说，路线已经从“让 Claude Code 能跑起来”进入：

> **让通用 OpenAI-format backend 在 Claude Code / agent team / MCP 场景下跑得更稳、更省、更可持续。**
