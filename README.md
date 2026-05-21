# Claude Code Codex Proxy

这个小工具把 Claude Code 的 Anthropic Messages 请求转成 OpenAI Responses 请求，
适合后端是任意 `POST /v1/responses` 风格 OpenAI-format 接口的场景。

代理同时提供：

- `/healthz` 健康检查
- `/v1/messages` / `/v1/messages/count_tokens` / `/v1/models` 主桥接接口
- `/v1/usage` JSON 用量摘要 与 `/v1/usage/dashboard` HTML 仪表板（按天 / 周 / 月聚合）
- 多客户端共享密钥、可选请求间隔限流、按后端模型注入额外 system prompt
- 可选 SQLite 持久化的 Token 使用统计

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
  - Claude Code: 当 `env.ANTHROPIC_BASE_URL` 是非 loopback 后端地址时，读取 `env.ANTHROPIC_AUTH_TOKEN`
- backend model
  - Codex: 顶层 `model`
  - Claude Code: `env.ANTHROPIC_MODEL`
- client API key
  - 显式环境变量：`CLAUDE_CODE_PROXY_CLIENT_API_KEY` / `CLAUDE_CODE_PROXY_CLIENT_API_KEYS`
  - Claude Code: 当 `env.ANTHROPIC_BASE_URL` 指向 `127.0.0.1` / `localhost` 之类的本地代理地址时，读取 `env.ANTHROPIC_AUTH_TOKEN`

说明：

- 如果 Claude Code 配置的是 Anthropic 风格地址（例如以 `/anthropic` 结尾），代理会自动归一化成 Responses 后端 base URL
- 如果 Claude Code 配置指向的是本地代理地址（如 `127.0.0.1` / `localhost`），代理会自动忽略这类值，避免回环请求
- 如果 Claude Code 当前已经指向本地代理地址，生成脚本会复用其中的 `ANTHROPIC_AUTH_TOKEN` 作为 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`
- 代理**不会**再自动读取当前项目目录下的 `.claude/settings*.json` 作为后端 fallback，避免不可信仓库重定向请求目标
- 仍然推荐显式设置 `CLAUDE_CODE_PROXY_BACKEND_MODEL`，这样不会受客户端展示模型名影响

## Docker 分发与运行

说明：

- 不要把后端 URL、后端 API key、客户端共享密钥写进 Dockerfile、镜像或仓库。
- 运行镜像时请把本地 `.env.local` 只读挂载到 `/app/.env.local`。
- Docker 模式下容器内默认监听 `0.0.0.0:8787`，因此 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`（或 `CLAUDE_CODE_PROXY_CLIENT_API_KEYS`）应视为必填。

镜像启动时会自动读取：

```text
/app/.env.local
```

### Quick Start

#### 1. 准备配置文件

```powershell
git clone https://github.com/starnotes-xj/claude-code-proxy.git
cd claude-code-proxy
```

方式 1：从本机已有的 `~/.codex` / `~/.claude` 配置生成 `.env.local`

```powershell
# Windows PowerShell
.\scripts\write-env-from-config.ps1
```

```bash
# Linux
bash scripts/write-env-from-config.sh
```

```bash
# macOS Terminal
bash scripts/write-env-from-config.sh
```

```bash
# macOS Finder / double-click
open scripts/write-env-from-config.command
```

方式 2：手动复制模板后填写

```powershell
Copy-Item env.example .env.local
notepad .env.local
```

#### 2. 构建镜像

```powershell
docker build -t claude-codex-proxy:latest .
```

如需启用 Token 使用统计功能，请使用 `-tags sqlite` 构建：

```powershell
go build -tags sqlite -o claude-codex-proxy.exe .
```

`Dockerfile` 当前默认按无 SQLite 模式构建（`noopUsageStore`），如有需要可自行添加 `--build-arg` 或调整 `RUN go build` 一行加上 `-tags sqlite`。

#### 3. 运行容器

```powershell
docker run -d --name claude-codex-proxy `
  -p 127.0.0.1:8787:8787 `
  -v "${PWD}/.env.local:/app/.env.local:ro" `
  claude-codex-proxy:latest
```

你只需要把本地生成好的 `.env.local` 以只读方式挂载进去即可。如果同时使用 `-e` 传入环境变量，显式传入的环境变量优先。

如果你更想直接传环境变量，也仍然可以改用：

```powershell
docker run -d --name claude-codex-proxy `
  -p 127.0.0.1:8787:8787 `
  -e CLAUDE_CODE_PROXY_BACKEND_BASE_URL="https://your-backend.example.com" `
  -e CLAUDE_CODE_PROXY_BACKEND_API_KEY="your-backend-api-key" `
  -e CLAUDE_CODE_PROXY_BACKEND_MODEL="gpt-5.4" `
  -e CLAUDE_CODE_PROXY_CLIENT_API_KEY="replace-with-a-local-shared-key" `
  claude-codex-proxy:latest
```

## 环境变量参考

### 必填

- `CLAUDE_CODE_PROXY_BACKEND_BASE_URL`：OpenAI 兼容后端基础地址（可被 Codex/Claude 配置 fallback 自动发现）
- `CLAUDE_CODE_PROXY_BACKEND_API_KEY`：后端 API key（可被 fallback 自动发现）
- `CLAUDE_CODE_PROXY_CLIENT_API_KEY` 或 `CLAUDE_CODE_PROXY_CLIENT_API_KEYS`：当 `CLAUDE_CODE_PROXY_LISTEN_ADDR` 不是 loopback 时必填；本地 loopback 默认可省略

### 监听 / 路由

- `CLAUDE_CODE_PROXY_LISTEN_ADDR`，默认 `127.0.0.1:8787`；非 loopback 必须设置 client API key
- `CLAUDE_CODE_PROXY_BACKEND_PATH`，默认 `/v1/responses`
- `CLAUDE_CODE_PROXY_REQUEST_TIMEOUT`，默认 `120s`

### 模型与 reasoning

- `CLAUDE_CODE_PROXY_BACKEND_MODEL`：固定后端模型，避免被客户端展示模型名覆盖
- `CLAUDE_CODE_PROXY_WARMUP_MODEL`：可选预热用模型
- `CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS`：覆盖返回给 Claude Code 的模型名
- `CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT`，可选 `low|medium|high`
- `CLAUDE_CODE_PROXY_EXTRA_SYSTEM_PROMPTS`：JSON 字符串，按**后端模型名**匹配并在 `instructions` 末尾追加额外提示，例如：
  ```json
  {"gpt-5-mini":"Be concise.","gpt-5.4":"Prefer concrete code over abstract advice."}
  ```

### 客户端鉴权

- `CLAUDE_CODE_PROXY_CLIENT_API_KEY`：单个共享密钥
- `CLAUDE_CODE_PROXY_CLIENT_API_KEYS`：逗号分隔多个密钥（与单密钥并存时合并去重）；代理接受 `Authorization: Bearer <key>` 或 `x-api-key: <key>`

### Count tokens / Claude API 接入

- `CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY`，启用真实 `/v1/messages/count_tokens`（也可回退使用 `ANTHROPIC_API_KEY`）
- `CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL`，默认 `https://api.anthropic.com`
- `CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER`，Claude fallback 估算倍率，默认 `1.15`

### Metadata / privacy 开关

- `CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA=true`，把 Claude 请求里的 metadata 与部分头透传给后端（默认关闭）
- `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=true|false`，控制是否转发用户原始 `metadata`；若与旧变量 `CLAUDE_CODE_PROXY_DISABLE_USER_METADATA_FORWARDING` 同时存在，以这个新变量为准
- `CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST=trace,tenant`，仅允许精确、大小写敏感匹配的用户 metadata key 透传；不会放开 `user_id` / `claude_code_*` 等永久阻断项；仅在 `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA=true` 且 `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=false` 时生效
- `CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA=true`，不再发送 bridge 派生的 `claude_code_*` continuity metadata
- `CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY=true`，不再发送顶层 `prompt_cache_key`
- `CLAUDE_CODE_PROXY_ANONYMOUS_MODE=true`，最高优先级匿名外发总开关；会同时关闭用户 metadata、continuity metadata、`prompt_cache_key` 与固定 bridge header metadata（如 `x-claude-code-model` / `x-claude-code-config-hash`）；默认关闭

### 能力探测 / 流式

- `CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT=true`：启动时主动探测后端模型能力
- `CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL`，默认 `30m`：能力探测的复探时间窗
- `CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY=true`，可选启用 phase-aware commentary；默认关闭
- `CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING=true`，强制后端走非流式

### 请求体积限制（默认值见括号）

- `CLAUDE_CODE_PROXY_MAX_INBOUND_BODY_BYTES`（默认 `8388608`，即 8 MiB）：来自 Claude Code 的请求体上限
- `CLAUDE_CODE_PROXY_MAX_BACKEND_REQUEST_BYTES`（默认 `5242880`，即 5 MiB）：转发给后端的请求体上限
- `CLAUDE_CODE_PROXY_MAX_BACKEND_ERROR_BODY_BYTES`（默认 `65536`，即 64 KiB）：后端错误体最大读取长度

### 限流（默认关闭）

- `CLAUDE_CODE_PROXY_RATE_LIMIT_INTERVAL`：相邻请求最小间隔（例如 `2s`）
- `CLAUDE_CODE_PROXY_RATE_LIMIT_WAIT=true|false`：超过频率时，`true` 表示在代理内排队等候（直到间隔满足或 ctx 取消），`false`（默认）则直接返回 `429 rate_limit_error` 并带 `Retry-After` header

### 使用统计

- `CLAUDE_CODE_PROXY_USAGE_DB_PATH`：SQLite 文件路径；仅在以 `-tags sqlite` 编译的二进制下生效。设置后会把每次 `/v1/messages` 的 input/output tokens 异步写入 `token_usage_events` 表，并通过 `/v1/usage` 与 `/v1/usage/dashboard` 暴露按 day / week / month 聚合的摘要。未启用 sqlite tag 时使用空实现 `noopUsageStore`，不持久化任何内容
- 端点请求方式：`GET /v1/usage?period=day|week|month`（默认 day，返回 JSON）；`GET /v1/usage/dashboard?period=...`（HTML 视图）

### 其他

- `CLAUDE_CODE_PROXY_DEBUG=true`，打印请求摘要与后端错误详情

说明：

- 本地 loopback 开发默认仍可不配 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`
- 如果监听地址是 `0.0.0.0`、`:` 前缀或其他非 loopback 地址，启动时必须设置 client API key
- Docker 模式下容器内监听 `0.0.0.0:8787`、宿主机只绑定 `127.0.0.1:8787`，因此 client API key 应视为必填

## HTTP 接口

| Method | Path | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查，返回 `{ok:true, backend_url:...}`；不需要 client key |
| POST | `/v1/messages` | Anthropic Messages，支持流式与非流式 |
| POST | `/v1/messages/count_tokens` | 优先走 Anthropic 官方计数端点（需 `ANTHROPIC_API_KEY`），否则回退本地估算 |
| GET | `/v1/models` | 优先透传后端 `/v1/models`，否则回退到本地合成 |
| GET | `/v1/usage` | JSON 摘要，参数 `period=day\|week\|month` |
| GET | `/v1/usage/dashboard` | HTML 仪表板，同上 |

除 `/healthz` 外，所有端点都受 `CLAUDE_CODE_PROXY_CLIENT_API_KEY(S)` 鉴权（当存在时）。

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
- 多客户端共享密钥（`CLAUDE_CODE_PROXY_CLIENT_API_KEYS`）
- 按后端模型注入额外 system prompt（`CLAUDE_CODE_PROXY_EXTRA_SYSTEM_PROMPTS`）
- 相邻请求间隔限流（`CLAUDE_CODE_PROXY_RATE_LIMIT_INTERVAL` / `_WAIT`）
- 可选 SQLite Token 用量统计 + JSON / HTML 仪表板（`-tags sqlite` 构建并配置 `CLAUDE_CODE_PROXY_USAGE_DB_PATH`）

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
- 默认二进制不带 sqlite tag，`CLAUDE_CODE_PROXY_USAGE_DB_PATH` 在默认构建下会被忽略并使用空实现

## 参考与致谢

- 本项目的部分功能设计与代码实现参考了 [caozhiyuan/copilot-api](https://github.com/caozhiyuan/copilot-api)
- 感谢 [caozhiyuan/copilot-api](https://github.com/caozhiyuan/copilot-api) 项目提供的功能思路与实现参考
