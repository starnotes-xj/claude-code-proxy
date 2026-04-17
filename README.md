# Claude Code Codex Proxy

这个小工具把 Claude Code 的 Anthropic Messages 请求转成 OpenAI Responses 请求，
适合后端是任意 `POST /v1/responses` 风格 OpenAI-format 接口的场景。

## 配置来源优先级

代理会按下面顺序解析后端配置，前者优先级更高：

1. 显式环境变量 `CLAUDE_CODE_PROXY_*`
2. Codex 配置
   - `%USERPROFILE%\\.codex\\config.toml` / `$CODEX_HOME\\config.toml`
   - `%USERPROFILE%\\.codex\\auth.json` / `$CODEX_HOME\\auth.json`
3. 当前用户的 Claude Code 配置
   - `%USERPROFILE%\\.claude\\settings.local.json`
   - `%USERPROFILE%\\.claude\\settings.json`

当前自动读取字段：

- backend base URL
  - Codex: `model_providers.<model_provider>.base_url`
  - Claude Code: `env.ANTHROPIC_BASE_URL`
- backend API key
  - Codex: `auth.json` 里的 `OPENAI_API_KEY`
  - Claude Code: `env.ANTHROPIC_AUTH_TOKEN`
- backend model
  - Codex: 顶层 `model`
  - Claude Code: `env.ANTHROPIC_MODEL`

说明：

- 如果 Claude Code 配置的是 Anthropic 风格地址（例如以 `/anthropic` 结尾），代理会自动归一化成 Responses 后端 base URL
- 如果 Claude Code 配置指向的是本地代理地址（如 `127.0.0.1` / `localhost`），代理会自动忽略这类值，避免回环请求
- 代理**不会**再自动读取当前项目目录下的 `.claude/settings*.json` 作为后端 fallback，避免不可信仓库重定向请求目标
- 仍然推荐显式设置 `CLAUDE_CODE_PROXY_BACKEND_MODEL`，这样不会受客户端展示模型名影响

## 启动

PowerShell:

```powershell
$env:CLAUDE_CODE_PROXY_BACKEND_API_KEY = "<你的后端 key>"
$env:CLAUDE_CODE_PROXY_BACKEND_MODEL = "gpt-5-codex"
$env:CLAUDE_CODE_PROXY_BACKEND_BASE_URL = "https://example.com"
$env:CLAUDE_CODE_PROXY_LISTEN_ADDR = "127.0.0.1:8787"
# 非 loopback / 容器部署时，务必设置客户端共享密钥
# $env:CLAUDE_CODE_PROXY_CLIENT_API_KEY = "<你的客户端 key>"

go run ./cmd/claude-codex-proxy
```

可选环境变量：

- `CLAUDE_CODE_PROXY_BACKEND_PATH`，默认 `/v1/responses`
- `CLAUDE_CODE_PROXY_REQUEST_TIMEOUT`，默认 `120s`
- `CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS`，覆盖返回给 Claude Code 的模型名
- `CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT`，可选 `low|medium|high`
- `CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY`，为 Claude 模型启用真实 `/v1/messages/count_tokens` 计数（也可回退使用 `ANTHROPIC_API_KEY`）
- `CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL`，默认 `https://api.anthropic.com`
- `CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER`，Claude fallback 估算倍率，默认 `1.15`
- `CLAUDE_CODE_PROXY_CLIENT_API_KEY`，客户端访问代理时使用的共享密钥；当 `CLAUDE_CODE_PROXY_LISTEN_ADDR` 不是 loopback（如 `0.0.0.0:8787`）时必填。代理接受 `Authorization: Bearer <key>` 或 `x-api-key: <key>`
- `CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA=true`，显式把 Claude 请求里的 metadata 与部分头透传给后端（默认关闭，兼容不支持 metadata 的 Responses 后端）
- `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=true|false`，控制是否转发用户原始 `metadata`；若与旧变量 `CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING` 同时存在，以这个新变量为准
- `CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST=trace,tenant`，仅允许精确、大小写敏感匹配的用户 metadata key 透传；不会放开 `user_id` / `claude_code_*` 等永久阻断项；仅在 `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=true` 且 `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=false` 时生效
- `CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA=true`，不再发送 bridge 派生的 `claude_code_*` continuity metadata
- `CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY=true`，不再发送顶层 `prompt_cache_key`
- `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`，最高优先级匿名外发总开关；会同时关闭用户 metadata、continuity metadata、`prompt_cache_key` 与固定 bridge header metadata（如 `x-claude-code-model` / `x-claude-code-config-hash`）；默认关闭
- `CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY=true`，可选启用 phase-aware commentary；默认关闭，适合愿意接收 phase hint 的后端
- `CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING=true`，强制后端走非流式
- `CLAUDE_CODE_PROXY_DEBUG=true`，打印请求摘要与后端错误详情，便于排障

如果你已经在 Codex / Claude Code 配置里填好了 URL 与 key，最小启动可以只保留监听地址：

```powershell
$env:CLAUDE_CODE_PROXY_LISTEN_ADDR = "127.0.0.1:8787"
go run ./cmd/claude-codex-proxy
```

说明：

- 本地 loopback 开发默认仍可不配 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`
- 如果监听地址是 `0.0.0.0`、`:` 前缀或其他非 loopback 地址，启动时必须设置 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`

## Claude Code 配置

PowerShell:

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8787"
# 如果代理配置了 CLAUDE_CODE_PROXY_CLIENT_API_KEY，这里应设置成相同的值
$env:ANTHROPIC_AUTH_TOKEN = "proxy-client-key-or-placeholder"
$env:ANTHROPIC_MODEL = "claude-sonnet-4-5"
```

说明：

- 如果代理启用了 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`，Claude Code 侧也要把 `ANTHROPIC_AUTH_TOKEN` 设成相同密钥
- 代理同时接受 `Authorization: Bearer <key>` 和 `x-api-key: <key>`，这样既兼容本地开发，也兼容容器/远程部署
- 如果代理仅监听 `127.0.0.1` 且你没有配置 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`，`ANTHROPIC_AUTH_TOKEN` 仍可使用占位值
- 实际请求会使用 `CLAUDE_CODE_PROXY_BACKEND_API_KEY` 去访问后端
- 代理默认把 Claude Code 请求里的 `model` 映射到 `CLAUDE_CODE_PROXY_BACKEND_MODEL`
- 为兼容部分不接受 `metadata` 参数的 OpenAI-format 后端，代理默认**不**向后端发送 metadata；如确实需要，可显式开启 `CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA=true`
- 如果只想停止转发用户原始 `metadata`，优先使用 `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=false`
- 如果希望彻底匿名外发，使用 `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`；该模式会覆盖 allowlist / ForwardUserMetadata / continuity / prompt_cache_key 等外发路径，但不会关闭代理内部 continuity 派生逻辑
- 已实测可用的后端模型名：`gpt-5.4`、`gpt-5.3-codex`

## 当前支持范围

- `POST /v1/messages`
- `POST /v1/messages/count_tokens`
- `GET /v1/models`
- 流式和非流式响应
- 当客户端要求流式、但后端被配置为非流式或实际返回 JSON 时，代理会把完整响应再包装成 Anthropic SSE 返回给 Claude Code
- 文本消息、tool use、tool result
- Claude `system` 提示词会映射到 Responses API 的 `instructions` 字段，避免部分后端拒绝 `system` role message
- 历史 assistant 文本消息会映射成 Responses 可接受的 `output_text` 内容块，兼容更严格的后端校验
- Claude `thinking` / `output_config.effort` 会尽量映射到 Responses `reasoning.effort`
- `image` 输入块
- `document` / `file` 输入块
- `thinking` / `redacted_thinking` 的兼容接收与代理内 opaque carrier 透传
- `tool_result` 会尽量保留结构化内容；只有在必须适配字符串型后端边界时，才会把结果展开为纯文本
- `tool_result.is_error` 会沿着转译链路携带失败语义，避免下游再从正文猜测是否报错
- tool schema 差异尽量在统一的内部转译层收口，避免 Claude / OpenAI / MCP 的字段差异直接暴露给后续阶段
- Claude `count_tokens` 可选接入 Anthropic 官方计数端点；未配置时会回退到本地估算，并对 Claude 模型应用保守倍率
- `/v1/models` 会优先尝试透传后端模型列表；若后端不支持或返回不完整，再回退到本地合成能力广告

## 正在补齐的能力

### 高优先级

- 结构化 `tool_result` 保留：优先保留 `content` 里的块结构与 JSON 载荷，只在后端边界确实只接受文本时才 flatten
- `tool_result.is_error` / `status` 语义：把失败态直接带进 `function_call_output.status`，让后续流程消费状态，而不是解析正文里的错误前缀
- tool schema normalization：先把 Claude Code / OpenAI-format / MCP 的 tool 定义收敛成稳定内部结构，再统一转成后端需要的 schema

### 中优先级

- phase-aware commentary：默认关闭的可选增强；在愿意接收 phase hint 的后端上按 commentary / action / result / final 等阶段理解模型输出，减少语义跳变
- 流式语义保真增强：尽量保留 chunk、阶段边界和终态语义，让 SSE 与非流式 fallback 的行为更一致

## 已知限制

- `count_tokens` 基于字符长度的近似估算（`len([]rune(text))/4` 上取整），不代表后端真实 tokenizer
- `thinking` / `redacted_thinking`：Anthropic 原生 signed thinking 无法无损映射到 OpenAI reasoning
  - 当前仅保证由本代理生成并回传的 opaque carrier（`ccp-reasoning-v1:`）可以用于续接后端上下文（`reasoning.encrypted_content`）
- `phase-aware commentary` 只能作为可选增强存在；如果后端不接受 phase hint 或严格校验额外字段，必须回退到现有线性 text/tool/result 语义
- 流式响应优先适配 OpenAI `/v1/responses` 风格 SSE 事件；若后端事件类型/字段明显偏离，仍需要再补兼容
  - SSE 单个 event 读取缓冲区上限为 2MB（Go `bufio.Scanner` 限制），超过会导致流解析失败
  - 非流式请求若后端返回流式响应，会先聚合完整事件流再转换（增加延迟与内存占用）
  - 流式请求若后端不返回 SSE，会先接完整 JSON 再转成 Anthropic SSE（首 token 延迟更高）
- 参数映射：`stop_sequences` 不转发到后端（后端不支持）
- 本地合成的 `/v1/models` fallback 只广告单个模型；若后端原生支持模型列表透传，代理会优先返回后端提供的多模型数据
