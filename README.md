# Claude Code Codex Proxy

将 Claude Code 的 Anthropic Messages 请求转发给任意 OpenAI Responses 风格后端（`POST /v1/responses`）。

## 快速开始

### 1. 克隆并配置

```bash
git clone https://github.com/starnotes-xj/claude-code-proxy.git
cd claude-code-proxy
```

**方式 A：从已有的 `~/.codex` / `~/.claude` 配置自动生成**

```powershell
# Windows PowerShell
.\scripts\write-env-from-config.ps1
```

```bash
# macOS / Linux
bash scripts/write-env-from-config.sh
```

**方式 B：手动填写**

```bash
cp env.example .env.local
# 编辑 .env.local，填入后端地址和 API Key
```

### 2. 启动

**Docker（推荐）：**

```bash
docker build -t claude-codex-proxy:latest .

docker run -d --name claude-codex-proxy \
  -p 127.0.0.1:8787:8787 \
  -v "${PWD}/.env.local:/app/.env.local:ro" \
  claude-codex-proxy:latest
```

**本地直接运行：**

```bash
go run .

# 启用 SQLite 用量统计时
go build -tags sqlite -o claude-codex-proxy && ./claude-codex-proxy
```

### 3. 配置 Claude Code

将 Claude Code 的后端地址指向代理：

```
ANTHROPIC_BASE_URL=http://127.0.0.1:8787
```

---

## 功能

- 将 Claude Code Anthropic Messages API 转发到 OpenAI Responses 兼容后端
- 支持流式与非流式响应、tool use、图片/文档输入
- 多客户端共享密钥（`CLAUDE_CODE_PROXY_CLIENT_API_KEYS`）
- 按后端模型注入额外 system prompt
- 可选请求间隔限流
- 可选 SQLite Token 用量统计（`/v1/usage` JSON + `/v1/usage/dashboard` HTML 仪表板）
- `/healthz` 健康检查

## 环境变量参考

### 必填

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_BACKEND_BASE_URL` | OpenAI 兼容后端基础地址 |
| `CLAUDE_CODE_PROXY_BACKEND_API_KEY` | 后端 API Key |
| `CLAUDE_CODE_PROXY_CLIENT_API_KEY` 或 `CLAUDE_CODE_PROXY_CLIENT_API_KEYS` | 客户端共享密钥；监听非 loopback 地址时必填（Docker 模式应视为必填） |

### 监听 / 路由

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_PROXY_LISTEN_ADDR` | `127.0.0.1:8787` | 监听地址；非 loopback 时必须设置 client API key |
| `CLAUDE_CODE_PROXY_BACKEND_PATH` | `/v1/responses` | 后端路径 |
| `CLAUDE_CODE_PROXY_REQUEST_TIMEOUT` | `120s` | 请求超时 |

### 模型与 reasoning

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_BACKEND_MODEL` | 固定后端模型名，避免被客户端展示名覆盖（推荐设置） |
| `CLAUDE_CODE_PROXY_WARMUP_MODEL` | 启动预热用模型（可选） |
| `CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS` | 覆盖返回给 Claude Code 的模型名 |
| `CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT` | `low\|medium\|high` |
| `CLAUDE_CODE_PROXY_EXTRA_SYSTEM_PROMPTS` | JSON，按后端模型名追加 system prompt，例如 `{"gpt-5":"Be concise."}` |

### 客户端鉴权

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_CLIENT_API_KEY` | 单个共享密钥 |
| `CLAUDE_CODE_PROXY_CLIENT_API_KEYS` | 逗号分隔多个密钥，与单密钥合并去重 |

代理接受 `Authorization: Bearer <key>` 或 `x-api-key: <key>`。

### Count tokens / Claude API 接入

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY` | 启用真实 `/v1/messages/count_tokens`（也可回退 `ANTHROPIC_API_KEY`） |
| `CLAUDE_CODE_PROXY_ANTHROPIC_API_BASE_URL` | 默认 `https://api.anthropic.com` |
| `CLAUDE_CODE_PROXY_CLAUDE_TOKEN_MULTIPLIER` | 本地估算倍率，默认 `1.15` |

### Privacy / metadata

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_PROXY_ANONYMOUS_MODE` | `false` | 最高优先级匿名总开关，关闭所有 metadata 外发 |
| `CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA` | `false` | 透传 Claude 请求 metadata 与部分头到后端 |
| `CLAUDE_CODE_PROXY_FORWARD_USER_METADATA` | — | `true\|false`，控制用户原始 metadata 转发 |
| `CLAUDE_CODE_PROXY_USER_METADATA_ALLOWLIST` | — | 逗号分隔允许透传的 metadata key（严格大小写匹配） |
| `CLAUDE_CODE_PROXY_DISABLE_CONTINUITY_METADATA` | `false` | 不发送 `claude_code_*` continuity metadata |
| `CLAUDE_CODE_PROXY_DISABLE_PROMPT_CACHE_KEY` | `false` | 不发送顶层 `prompt_cache_key` |

### 限流（默认关闭）

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_RATE_LIMIT_INTERVAL` | 相邻请求最小间隔，例如 `2s` |
| `CLAUDE_CODE_PROXY_RATE_LIMIT_WAIT` | `true` 则排队等待；`false`（默认）则返回 `429` |

### 用量统计

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_USAGE_DB_PATH` | SQLite 文件路径；仅在 `-tags sqlite` 构建下生效 |

启用后每次 `/v1/messages` 的 token 用量会异步写入数据库，通过以下端点访问：

- `GET /v1/usage?period=day|week|month` — JSON 摘要
- `GET /v1/usage/dashboard?period=...` — HTML 仪表板

默认构建不带 sqlite tag，此变量会被忽略（使用空实现）。

### 能力探测 / 流式

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_PROXY_ENABLE_MODEL_CAPABILITY_INIT` | `false` | 启动时主动探测后端模型能力 |
| `CLAUDE_CODE_PROXY_CAPABILITY_REPROBE_TTL` | `30m` | 能力探测复探时间窗 |
| `CLAUDE_CODE_PROXY_DISABLE_BACKEND_STREAMING` | `false` | 强制后端走非流式 |
| `CLAUDE_CODE_PROXY_ENABLE_PHASE_COMMENTARY` | `false` | 启用 phase-aware commentary（实验性） |

### 请求体积限制

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `CLAUDE_CODE_PROXY_MAX_INBOUND_BODY_BYTES` | `8388608`（8 MiB） | 来自 Claude Code 的请求体上限 |
| `CLAUDE_CODE_PROXY_MAX_BACKEND_REQUEST_BYTES` | `5242880`（5 MiB） | 转发给后端的请求体上限 |
| `CLAUDE_CODE_PROXY_MAX_BACKEND_ERROR_BODY_BYTES` | `65536`（64 KiB） | 后端错误体最大读取长度 |

### 调试

| 变量 | 说明 |
| --- | --- |
| `CLAUDE_CODE_PROXY_DEBUG` | `true` 时打印请求摘要与后端错误详情 |

## HTTP 接口

| Method | Path | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查，返回 `{"ok":true,"backend_url":"..."}` |
| POST | `/v1/messages` | Anthropic Messages，支持流式与非流式 |
| POST | `/v1/messages/count_tokens` | 优先走 Anthropic 官方计数，否则本地估算 |
| GET | `/v1/models` | 优先透传后端模型列表，否则本地合成 |
| GET | `/v1/usage` | Token 用量 JSON 摘要 |
| GET | `/v1/usage/dashboard` | Token 用量 HTML 仪表板 |

`/healthz` 无需鉴权，其余端点在配置了 client API key 时均需鉴权。

## 配置来源

代理按以下优先级读取后端配置（前者优先）：

1. 显式环境变量 `CLAUDE_CODE_PROXY_*`
2. Codex 配置（`~/.codex/config.toml` / `auth.json`）
3. Claude Code 配置（`~/.claude/settings.local.json` / `settings.json`）

> 不会读取当前项目目录下的 `.claude/settings*.json`，避免不可信仓库重定向请求目标。

## 已知限制

- `count_tokens` 使用字符长度近似估算，不代表后端真实 tokenizer
- `thinking` / `redacted_thinking`：Anthropic 原生 signed thinking 无法无损映射到 OpenAI reasoning；代理生成的 opaque carrier（`ccp-reasoning-v1:`）可用于续接上下文，但不保证跨后端兼容
- SSE 单个 event 缓冲区上限 2 MB，超过会导致流解析失败
- `stop_sequences` 不转发给后端
- Docker 模式容器内监听 `0.0.0.0:8787`，client API key 应视为必填

## 参考与致谢

部分功能设计参考了 [caozhiyuan/copilot-api](https://github.com/caozhiyuan/copilot-api)，感谢该项目的思路与实现。
