# Claude Code ↔ OpenAI 格式桥接通用化迁移计划

## 背景

当前项目中的 `cmd/claude-codex-proxy` / `internal/claudecodexproxy` 已经可以把 Claude Code 的 Anthropic Messages 请求转换成 OpenAI Responses 风格请求，并在兼容的 OpenAI-format 后端上完成基本可用的桥接。

但目标不应绑定某一个站点或默认后端，而应当是：

> **只要后端提供“足够兼容的 OpenAI 格式接口/能力”，就能通过本工具让 Claude Code 尽可能正常地使用 coding / MCP / subagent / team 能力。**

参考仓库：

- 本地：`D:\IdeaProjects\copilot-api-test`
- 远程：`https://github.com/caozhiyuan/copilot-api`

该参考仓库虽面向 GitHub Copilot，但其中有不少 **Claude Code 语义兼容层** 是通用可迁移的。

结合当前仓库的 `README.md`、`internal/claudecodexproxy/config.go` 与 `internal/claudecodexproxy/proxy.go`，可以先明确一个现实基线：

- 当前仓库的**运维/接入基线**已经不只是“能转协议”
- 已经形成了：
  - Codex / Claude Code 配置发现与后端 fallback
  - 非 loopback 监听下的 client API key 鉴权
  - metadata / continuity / `prompt_cache_key` 的隐私分级开关
  - Docker / 本地运行的文档化接入路径

因此，这份迁移计划后续应把重心继续放在 **通用协议语义、能力探测、流式稳定性、continuity 保真** 上，而不是把大量精力重新投入到部署包装层。

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
  - round-trip history item persistence unsupported → 首次失败后在同一 backend-model scope 内 sticky disable 持久化历史项 `id`，并以 strip-id 形态重试
- 当前实现是 **generic capability probing**，不是 rawchat 专属逻辑

剩余风险：

- 当前 capability state 已从“单一全局 sticky”收口到 **request-model / configured-backend-model 作用域**；更进一步仍可继续升级成 backend family / effective backend model 分桶
- 仍以错误文案匹配为主，后续可继续升级为结构化 error classifier + TTL/re-probe
- `input item persistence` 已纳入 backend-model scoped capability 与 TTL/reprobe；后续若需要可继续与更多 provider 错误分类或更细分的 history item family 收口
- 当前仓库已完成 TTL-reprobe 第一轮最小落地：
  - `CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL`
  - 对 `metadata`、`reasoning`、`reasoning_include`、`phase`、`stream`、`structured_output`、`prompt_cache_key`、`context_management`、`compaction_input`、`input_item_persistence`、`model passthrough` 启用过期后自然重探
  - 仍不把 `parallel_tool_calls` / thinking budget / profile limit 这类 preflight 项写进 TTL 状态
 - 当前仓库已完成 TTL-reprobe 第二轮最小落地：
   - 通过 `reprobeUntil` lease 侧表实现 single-leader reprobe
   - TTL 到期后只有一个 leader 请求重探，其他 follower 继续沿用旧降级路径
   - 不共享响应、不阻塞 follower、不改变现有 adaptive retry 主流程

---

## 2026-04-20 结合当前仓库实现后的新增迁移判断

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
   - 当前仓库已完成第三轮最小落地：
     - continuity 派生值与外发 metadata projection 已分层收口
     - `buildMetadata(...)` 不再直接展开 continuity 投影细节，而是通过独立 projection helper 落地
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
   - 当前仓库已完成第三轮最小落地：
     - warmup-like 请求收紧为更保守的单轮纯文本判定
     - 带 compaction carrier、多轮文本历史、带 system 指令的请求不再误走 warmup
     - warmup / compact / compaction-carrier 请求会提前 prime model profiles，降低 warmup model profile 不兼容时的误路由
   - 仍待继续：
     - compact / auto-continue 与 model-driven capability init 的联动策略
     - 更细的 warmup-like 启发式只在确有收益时谨慎推进，避免重新放大误判面

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
   - 为异常 `function_call_arguments.delta` flood、超大参数、顺序冲突等流式异常添加保护
   - 当前仓库已完成第一轮最小实现：保留空 delta flood / 超大参数 / 顺序冲突等保护；空白参数增量按原样透传，避免误杀合法流式 JSON 片段
   - 当前仓库已完成第二轮最小实现：
     - `delta after done` 直接报错
     - 超大 tool 参数累计直接报错
     - 冲突性的 duplicate `function_call_arguments.done` 直接报错
   - 当前仓库已完成第三轮最小实现：
     - `excessive empty delta flood` 显式测试
     - `output_item.done` 单包超大参数保护
     - “合法长参数流不误杀”正向测试
   - 当前仓库已完成第四轮最小实现：
     - 首个 `response.function_call_arguments.done` 与已累计 delta 冲突时显式报错
     - late-closed `output_item.added` / `output_item.done` 参数在可接受扩展场景下不误报
   - 后续仍可继续扩成更细的 tool/block 级异常流保护（如更细 provider 顺序兼容）

2. **compaction / context carrier**
   - 当前仓库已完成 compaction / context carrier 的多轮落地：
     - Anthropic `cm1#<encrypted>@<id>` carrier 可转为 OpenAI `input[].type="compaction"`
     - OpenAI `output[].type="compaction"` 可回转为 Anthropic `thinking` block（非流式 / 流式 fallback 均已覆盖）
     - latest compaction input shrinking 已和 `context_management` 注入解耦；后端不支持 `context_management` 时仍保留 shrinking
     - 若后端不支持 `input[].type="compaction"`，会在同一 backend-model scope 内禁用 compaction input，并把最新 compaction carrier 降级为 `reasoning` carrier 后重试
     - `input[n].type` / enum / expected-values 风格的 provider 报错已纳入 compaction input fallback 分类
   - 后续仍可继续增强：
     - compaction input fallback 的更多 provider 错误文案识别
     - 完全不支持 reasoning input 的更弱 text/omit fallback 策略（需谨慎，避免泄漏或语义污染）

3. **IDE/tool sanitize**
   - 对明显不适合桥接层直接透传的工具描述做轻量净化，例如 IDE diagnostics 类工具的 schema / description 收口
   - 参考仓库最新已把这条从“建议”推进为默认 preprocess

4. **更精细的 token fallback estimator**
   - 继续保留 Anthropic exact count 优先
   - 当前仓库已完成第一轮最小落地：
     - fallback estimator 已从纯字符粗略估算推进到更细粒度 token-unit 近似
     - `count_tokens` fallback 路径增加了 tool overhead / multiplier 的 debug 观测

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

## 推荐迁移顺序（执行清单版）

下面把迁移计划改写成更适合持续跟踪的任务清单。

### Phase 1：基线能力（已基本完成）

- [x] `count_tokens` 特殊处理
- [x] `prepareMessagesApiPayload` 核心兼容逻辑
- [x] runtime capability probing / automatic downgrade
- [x] `/v1/models` passthrough + synthetic fallback
- [x] IDE/tool sanitize

完成标志：

- Claude Code 基本 messages/tool/MCP 流程可跑通
- 后端不支持 `metadata` / `reasoning` / `phase` / `stream` / structured tool output 时可自动降级
- `/v1/models` 与 `count_tokens` 都有 fallback 路径

### Phase 2：连续性与模型路由（建议优先推进）

#### Task 2.1：session / subagent continuity 第二轮收口

目标：

- 让 team / task / subagent / compact 请求链路拥有更稳定的 continuity 语义

具体事项：

- [ ] 评估 long-running team / task 是否还需要补充 continuity header/field
- [ ] 明确哪些 continuity 字段应继续保留在 `claude_code_*` 命名下，哪些值得抽象成更稳定的 bridge schema
- [ ] 补齐对应测试，覆盖 session affinity / parent session / interaction id 续接场景

完成判定：

- 长链路 agent/team 请求在后端侧具有稳定关联信息
- continuity 语义与隐私开关边界不冲突

#### Task 2.2：model-driven capability initialization 第二轮增强

目标：

- 进一步减少首次 unsupported retry，提升模型选择与 payload 初值准确度

具体事项：

- [ ] 继续细化 `supported_endpoints` / `capabilities.supports.*` 的 pre-disable 初始化
- [ ] 评估 endpoint/profile 粒度是否需要继续细分
- [ ] 继续补 `limits` 驱动的 request shaping / model eligibility 测试
- [ ] 明确 `PreferredModel` 是否只保留保守 fallback，不扩成激进 best-fit router

完成判定：

- 常见兼容后端首次请求即更接近可接受 payload
- warmup model 与正式 model 的 capability 污染被持续隔离

#### Task 2.3：compact / warmup / auto-continue 第三轮增强

目标：

- 继续把降本预处理做成通用 bridge 能力，而不是站点特化逻辑

具体事项：

- [ ] 细化 warmup-like 请求识别与误判控制
- [ ] 评估 compact / auto-continue 与 model capability init 的联动策略
- [ ] 补齐 compact `skipLastMessage`、warmup model routing、非 warmup 请求不误路由的回归测试

完成判定：

- warmup 降本收益可持续保留
- 普通请求不会被误导向小模型或过度裁剪 payload

### Phase 3：稳定性与语义保真（增强项）

#### Task 3.1：compaction / context carrier 收口

- [ ] 继续扩充 `compaction input` unsupported 的 provider 错误识别
- [ ] 谨慎评估完全不支持 reasoning input 时的更弱 fallback（text / omit）
- [ ] 补齐 compaction input / output / fallback 的端到端测试矩阵

完成判定：

- compaction carrier 在支持与不支持该语义的后端上都能稳定落地或安全降级

#### Task 3.2：stream/tool 异常保护与 round-trip history fallback 收口

- [ ] 继续增强 tool/block 级顺序异常保护
- [ ] 评估 `input item persistence` 是否升级成 scoped capability state，而不只停留在单请求自愈 fallback
- [ ] 补齐异常流、provider 顺序偏差、历史项 id 剥离后的回归测试

完成判定：

- 流式异常不会轻易打断正常会话
- round-trip history fallback 在行为稳定的后端上可预测、可观测

#### Task 3.3：observability / TTL / token fallback estimator

- [ ] 继续细化 error classifier 与 TTL / reprobe 策略
- [ ] 增加更面向 feature scope 的 debug / trace 观测点
- [ ] 在保留 Anthropic exact count 优先的前提下，改进 fallback estimator

完成判定：

- unsupported feature 的重探行为更可解释
- token fallback 与真实 tokenizer 的偏差进一步收敛

### 建议执行顺序（简版）

1. Task 2.1 session / subagent continuity 第二轮收口
2. Task 2.2 model-driven capability initialization 第二轮增强
3. Task 2.3 compact / warmup / auto-continue 第三轮增强
4. Task 3.1 compaction / context carrier 收口
5. Task 3.2 stream/tool 异常保护与 round-trip history fallback 收口
6. Task 3.3 observability / TTL / token fallback estimator

### 对应到当前仓库的实际 todo（可直接开工）

下面把上面的阶段任务进一步映射到当前仓库中的主要落点，方便直接拆 issue / PR。

#### Todo A：continuity 语义第二轮收口

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `deriveContinuityContext(...)`
  - `deriveSessionAffinity(...)`
  - `deriveParentSessionID(...)`
  - `deriveInteractionType(...)`
  - `deriveInteractionID(...)`
  - `buildBackendRequestWithOptions(...)`
- `internal/claudecodexproxy/proxy_test.go`
  - 已有 continuity 基线测试可继续扩：
    - `TestBuildBackendRequestDerivesContinuityMetadataFromSessionAndSubagent`
    - `TestBuildBackendRequestAddsSecondWaveContinuityMetadata`
    - `TestBuildBackendRequestDerivesCompactAutoContinueInteractionType`
    - `TestHandleMessagesDoesNotSendContinuityMetadataWhenDisabled`

直接 todo：

- [x] 梳理现有 continuity 字段，把“内部稳定标识”和“外发 metadata 字段”明确分层
- [ ] 评估 long-running team/task 是否还需要额外 continuity 载体
- [ ] 为 interaction continuity 新增正向 + 隐私开关回归测试

建议验收：

- 打开/关闭 `DisableContinuityMetadata`、`AnonymousMode` 时，请求行为符合预期
- subagent / compact / 普通 conversation 三类 interaction 都能稳定区分

#### Todo B：model-driven capability initialization 第二轮增强

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `seedCapabilitiesFromModels(...)`
  - `extractBackendModelProfile(...)`
  - `optionsForRequest(...)`
  - `selectBackendModel(...)`
  - `modelProfileSupportsRequest(...)`
- `internal/claudecodexproxy/proxy_test.go`
  - 已有模型能力初始化测试可继续扩：
    - `TestBuildBackendRequestEnablesParallelToolCallsWhenProfileSupportsIt`
    - `TestBuildBackendRequestDowngradesHighReasoningWhenMaxThinkingBudgetIsSmall`
    - `TestBuildBackendRequestFallsBackWhenThinkingBudgetBelowProfileMinimum`
    - `TestHandleMessagesSetsParallelToolCallsWhenFetchedProfileAllows`
    - `TestHandleMessagesCapsReasoningEffortWhenFetchedMaxThinkingBudgetTooLow`

直接 todo：

- [ ] 继续细化 `supported_endpoints` / `capabilities.supports.*` 到请求初值的映射
- [ ] 明确 `PreferredModel` 的边界：仅保守 fallback，还是允许更积极的 best-fit 选择
- [ ] 补齐 profile-driven request shaping 与 model fallback 的回归测试矩阵

建议验收：

- 常见不兼容字段能在首次请求前就被预裁剪
- warmup model 的能力状态不会污染正式模型

#### Todo C：compact / warmup / auto-continue 第三轮增强

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `detectCompactType(...)`
  - `isWarmupRequest(...)`
  - `resolveWarmupModel(...)`
  - `mergeToolResultForClaude(...)`
- `internal/claudecodexproxy/proxy_test.go`
  - `TestBuildBackendRequestSkipsLastMergeForCompactRequest`
  - `TestBuildBackendRequestRoutesWarmupNoToolsAnthropicBetaToWarmupModel`
  - `TestBuildBackendRequestDoesNotUseWarmupModelForOrdinaryRequests`
  - `TestHandleMessagesRoutesWarmupRequestsToConfiguredWarmupModel`
  - `TestHandleMessagesDoesNotRouteWarmupModelForCompactRequests`

直接 todo：

- [x] 收紧 warmup-like 请求识别条件，减少误判
- [x] 评估 compact / auto-continue 与 capability init 的联动顺序
- [x] 为普通请求误路由、小模型污染、compact 合并边界继续补测试

建议验收：

- warmup 只命中预期场景
- compact 与 auto-continue 不会破坏 tool_result 合并和后续续接语义

#### Todo D：compaction / context carrier 收口

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `compactInputByLatestCompaction(...)`
  - `downgradeCompactionInputItems(...)`
  - `dropCompactionInputItems(...)`
  - `requestContainsCompactionCarrier(...)`
- `internal/claudecodexproxy/proxy_test.go`
  - `TestBuildBackendRequestConvertsCompactionCarrier`
  - `TestBuildBackendRequestInjectsContextManagementCompactionWhenProfileAllows`
  - `TestBuildBackendRequestKeepsOnlyLatestCompactionAndFollowingItems`
  - `TestHandleMessagesRetriesWithDowngradedCompactionInputAfterUnsupportedType`
  - `TestHandleMessagesContextManagementTTLReprobeStillKeepsLatestCompactionShrinking`

直接 todo：

- [x] 扩充 `compaction input unsupported` 的错误识别文案
- [ ] 评估 reasoning input 完全不可用时的最弱安全降级策略
- [ ] 补齐 compaction input/output/fallback 的端到端场景

建议验收：

- compaction carrier 在支持/不支持后端都能稳定工作或安全降级
- latest compaction shrinking 不因 capability fallback 而失效

#### Todo E：stream/tool 异常保护与 persisted-history fallback 收口

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `sseTranslator.trackToolArgumentDelta(...)`
  - `classifyCapabilityFailure(...)`
  - `requestUsesRoundTripHistoryItems(...)`
  - `matchesInputItemPersistenceFailure(...)`
- `internal/claudecodexproxy/proxy_test.go`
  - `TestHandleMessagesStreamAllowsWhitespaceOnlyFunctionCallArgumentDeltas`
  - `TestHandleMessagesStreamRejectsDeltaAfterToolDone`
  - `TestHandleMessagesStreamRejectsOversizedToolArguments`
  - `TestHandleMessagesStreamRejectsExcessiveEmptyDeltaFlood`
  - `TestHandleMessagesRetriesWithoutPersistedHistoryIDsAfterStatelessSessionError`

直接 todo：

- [x] 收口 `input item persistence` 为 scoped capability state，并补 sticky disable / TTL 回归测试
- [x] 补齐“首个 done 与已累计 delta 冲突”的异常流回归测试
- [ ] 继续补 provider 顺序偏差等更细粒度的异常流测试

建议验收：

- 合法长参数流、空白 JSON 片段、done 扩展前缀等场景不误报
- 真正异常的 delta flood / oversized / after-done 冲突能稳定拦截

#### Todo F：observability / TTL / token fallback estimator

主要落点：

- `internal/claudecodexproxy/proxy.go`
  - `effectiveCapabilityStateLocked(...)`
  - `setCapabilityCooldownLocked(...)`
  - `clearCapabilityCooldownLocked(...)`
  - `countTokensViaAnthropic(...)`
  - token fallback / debug 输出相关逻辑
- `internal/claudecodexproxy/config.go`
  - `CapabilityReprobeTTL` 配置项
- `internal/claudecodexproxy/proxy_test.go`
  - `TestHandleMessagesStreamTTLReprobeUsesSingleLeaderAndKeepsAnthropicSSE`
  - `TestHandleMessagesAnonymousModeDoesNotConsumePromptCacheKeyTTLReprobeLease`

直接 todo：

- [ ] 继续细化 error classifier，减少纯字符串匹配的脆弱性
- [x] 增加按 feature scope 观察 TTL / reprobe 行为的调试信息
- [x] 改进 fallback token estimator，同时保持 Anthropic exact count 优先

建议验收：

- capability 降级、冷却、重探路径更易解释
- token 估算在 MCP / Skill / tool-heavy 请求下更稳定

#### 推荐拆分方式

如果要按小步提交推进，建议拆成 6 个独立 PR：

1. PR-1：continuity 收口
2. PR-2：model capability init 第二轮增强
3. PR-3：warmup / compact 第三轮增强
4. PR-4：compaction / context carrier 收口
5. PR-5：stream/tool guard + persisted-history fallback 收口
6. PR-6：observability / TTL / token estimator

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

从当前仓库代码、README、真实 E2E 与本轮回归测试看，当前 bridge 已经跨过：

1. MCP 工具调用可用
2. `TeamCreate` / `Agent` / `SendMessage` / `TeamDelete` 链路可用
3. runtime capability probing 的高价值降级项已具备 backend-scope sticky disable + TTL/reprobe
4. 配置发现、client 鉴权、隐私分级开关已经形成稳定的接入基线
5. warmup / compact / compaction / stream guard / persisted-history fallback 已完成一轮实装与验收

因此，后续最值得继续推进的事项已经收敛为：

1. **continuity 的剩余增强**：是否还需要 long-running team/task 的额外 continuity 载体
2. **model-driven capability init 的细粒度收口**：更细 endpoint/profile 初始化，但继续保持保守 fallback
3. **compaction 的更弱降级策略**：仅在确有必要时评估 text / omit 路径
4. **stream/provider 顺序兼容**：补更细粒度 provider 顺序偏差测试，而不是继续放宽主逻辑
5. **error classifier 结构化升级**：逐步减少纯字符串匹配依赖

也就是说，路线已经从“让 Claude Code 能跑起来”进入：

> **让通用 OpenAI-format backend 在 Claude Code / agent team / MCP 场景下持续稳定运行，并把剩余边角兼容问题收敛成更小、更明确的增量工作。**
