# Docker 本地运行

这份说明只覆盖**本地自用**场景：单机、单服务、最少挂载、默认安全。

目标是：

- 宿主机只开放 `127.0.0.1:8787`
- 容器内监听 `0.0.0.0:8787`
- 默认通过 Dockerfile 构建镜像，再用 `docker run` 运行
- 默认只读挂载 `.env.local`，不挂源码、`~/.codex`、`~/.claude`

> 为什么容器里要监听 `0.0.0.0`？
>
> 因为 Docker 端口映射要求服务监听容器内可达地址。这个项目又要求“非 loopback 监听必须配置 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`”，所以在 Docker 模式下请把 client key 当成必填项。

## 1. 准备 `.env.local`

如果你本机已经配置过 Codex / Claude Code，可以直接从本机配置生成 `.env.local`。

方式 1：从本机已有配置生成 `.env.local`

Windows PowerShell：

```powershell
.\scripts\write-env-from-config.ps1
```

Linux：

```bash
bash scripts/write-env-from-config.sh
```

macOS Terminal：

```bash
bash scripts/write-env-from-config.sh
```

macOS Finder / double-click：

```bash
open scripts/write-env-from-config.command
```

> Linux / macOS 脚本依赖系统可用的 `python3`。`.command` 文件只是对 `write-env-from-config.sh` 的 Finder 友好包装；如果 Finder 提示没有执行权限，可先运行 `chmod +x scripts/write-env-from-config.command`。

脚本会读取：

- 当前 shell 里的 `CLAUDE_CODE_PROXY_*`
- `%USERPROFILE%\.codex\config.toml` / `auth.json`
- `%USERPROFILE%\.claude\settings.local.json` / `settings.json`

并生成：

```text
.env.local
```

如果 `.env.local` 已存在，脚本默认不会覆盖；确认要覆盖时使用：

```powershell
.\scripts\write-env-from-config.ps1 -Force
```

```bash
bash scripts/write-env-from-config.sh --force
```

方式 2：手动复制示例配置并填入真实值

```powershell
Copy-Item env.example .env.local
notepad .env.local
```

`.env.local` 至少需要包含：

```dotenv
CLAUDE_CODE_PROXY_BACKEND_BASE_URL=https://your-backend.example.com
CLAUDE_CODE_PROXY_BACKEND_API_KEY=your-backend-api-key
CLAUDE_CODE_PROXY_BACKEND_MODEL=gpt-5.4

# Docker 容器内会监听 0.0.0.0:8787，所以这里必须提供 client key。
CLAUDE_CODE_PROXY_CLIENT_API_KEY=replace-with-a-local-shared-key

# 可选：容器内监听地址，默认就是 0.0.0.0:8787
# CLAUDE_CODE_PROXY_LISTEN_ADDR=0.0.0.0:8787

# 可选：按需打开
# CLAUDE_CODE_PROXY_REQUEST_TIMEOUT=120s
# CLAUDE_CODE_PROXY_ANTHROPIC_MODEL_ALIAS=claude-sonnet-4-5
# CLAUDE_CODE_PROXY_BACKEND_REASONING_EFFORT=medium
# CLAUDE_CODE_PROXY_ANTHROPIC_API_KEY=your-anthropic-key
# CLAUDE_CODE_PROXY_ENABLE_BACKEND_METADATA=false
# CLAUDE_CODE_PROXY_DEBUG=false
```

说明：

- `env.example` 可以提交到远程仓库，`.env.local` 不应提交
- Docker 方案推荐把 `.env.local` 以只读挂载方式映射到容器内 `/app/.env.local`
- 也可以直接在 shell 里临时导出环境变量
- 默认方案不依赖 `~/.codex` / `~/.claude` 的自动发现逻辑

## 2. Quick Start

### 2.1 Build image

```powershell
docker build -t claude-codex-proxy:latest .
```

### 2.2 Run container

```powershell
docker run --rm `
  -p 127.0.0.1:8787:8787 `
  -v "${PWD}/.env.local:/app/.env.local:ro" `
  claude-codex-proxy:latest
```

### 2.3 Run published image

如果你已经从远程镜像仓库拉取了镜像，也可以不克隆源码，直接运行：

```powershell
docker run --rm `
  -p 127.0.0.1:8787:8787 `
  -v "${PWD}/.env.local:/app/.env.local:ro" `
  ghcr.io/YOUR_ORG/claude-codex-proxy:latest
```

镜像启动时会自动读取：

```text
/app/.env.local
```

所以只要把本地 `.env.local` 以只读方式挂载进去即可。

如果你更想直接传环境变量，则可以使用：

```powershell
docker run --rm `
  -p 127.0.0.1:8787:8787 `
  -e CLAUDE_CODE_PROXY_BACKEND_BASE_URL="https://your-backend.example.com" `
  -e CLAUDE_CODE_PROXY_BACKEND_API_KEY="your-backend-api-key" `
  -e CLAUDE_CODE_PROXY_BACKEND_MODEL="gpt-5.4" `
  -e CLAUDE_CODE_PROXY_CLIENT_API_KEY="replace-with-a-local-shared-key" `
  ghcr.io/YOUR_ORG/claude-codex-proxy:latest
```

镜像内默认 `CLAUDE_CODE_PROXY_LISTEN_ADDR=0.0.0.0:8787`，因此 `docker run` 通常不需要额外设置监听地址。若同时使用 `-e` 和挂载的 `/app/.env.local`，显式传入的环境变量优先。

查看日志：

```powershell
docker logs -f <container-id-or-name>
```

停止示例：

```powershell
docker stop <container-id-or-name>
```

## 3. 健康检查

服务起来后，可直接访问匿名健康检查：

```powershell
Invoke-WebRequest http://127.0.0.1:8787/healthz
```

如果你更习惯 `curl`：

```powershell
curl http://127.0.0.1:8787/healthz
```

## 4. Claude Code 侧如何指向这个代理

本地 Docker 方案下，Claude Code 侧通常这样配置：

```powershell
$env:ANTHROPIC_BASE_URL = "http://127.0.0.1:8787"
$env:ANTHROPIC_AUTH_TOKEN = "replace-with-a-local-shared-key"
$env:ANTHROPIC_MODEL = "claude-sonnet-4-5"
```

其中：

- `ANTHROPIC_BASE_URL` 指向 `docker run -p 127.0.0.1:8787:8787` 暴露给宿主机的地址
- `ANTHROPIC_AUTH_TOKEN` 要与 `CLAUDE_CODE_PROXY_CLIENT_API_KEY` 保持一致
- `ANTHROPIC_MODEL` 是 Claude Code 侧展示/请求的模型名，不必和后端真实模型名完全一致

## 5. 默认安全设计

当前 Docker 镜像运行方案刻意做了这些限制：

- **宿主机绑定**：`127.0.0.1:8787:8787`
  - 这样默认只允许本机访问，不直接暴露到局域网
- **容器内监听**：`0.0.0.0:8787`
  - 这是 Docker 端口映射所必需
- **必须配置 client key**
  - 因为容器内监听不是 loopback，程序会强制要求 `CLAUDE_CODE_PROXY_CLIENT_API_KEY`
- **不把个人配置或密钥打进镜像**
  - 远程仓库 / 公开镜像只包含程序本体
  - 后端 URL、后端 API key、客户端共享密钥都在运行时通过 env-file 或环境变量注入
- **默认最小挂载**
  - 只读挂载 `.env.local -> /app/.env.local`
  - 不挂源码
  - 不挂 home 目录
  - 不依赖宿主本地配置目录
- **最小权限**
  - 非 root 用户运行
  - 只读根文件系统
  - `tmpfs` 仅提供临时目录
  - 丢弃 Linux capabilities

## 6. 什么时候需要额外挂载

默认情况下至少需要一个只读挂载：

```powershell
-v "${PWD}/.env.local:/app/.env.local:ro"
```

这个挂载只用于把运行时配置注入镜像，不会把源码或 home 配置目录暴露给容器。

如果你确实还想复用宿主机已有的 `~/.codex` 或 `~/.claude` 配置，可以在 `docker run` 时额外增加**只读挂载**，例如：

```powershell
docker run --rm `
  -p 127.0.0.1:8787:8787 `
  -v "${USERPROFILE}\\.codex:/home/app/.codex:ro" `
  -v "${USERPROFILE}\\.claude:/home/app/.claude:ro" `
  claude-codex-proxy:latest
```

但这不属于默认推荐路径，因为：

- 你会把更多本地配置暴露给容器
- 可复现性不如显式 `.env.local`
- 不同机器上的 home 配置差异更大

## 7. 常见问题

### 启动时报 missing backend URL / API key

说明 `.env.local` 里没有配置：

- `CLAUDE_CODE_PROXY_BACKEND_BASE_URL`
- `CLAUDE_CODE_PROXY_BACKEND_API_KEY`

补齐后重新执行：

```powershell
docker build -t claude-codex-proxy:latest .
docker run --rm `
  -p 127.0.0.1:8787:8787 `
  -v "${PWD}/.env.local:/app/.env.local:ro" `
  claude-codex-proxy:latest
```

### 启动时报 missing client API key

说明你现在是 Docker 场景，容器内监听地址是非 loopback，所以必须设置：

- `CLAUDE_CODE_PROXY_CLIENT_API_KEY`

### 访问 `/v1/messages` 返回 401

说明客户端没有带认证，或者认证值不匹配。确认 Claude Code 侧：

- `ANTHROPIC_AUTH_TOKEN`

与容器内：

- `CLAUDE_CODE_PROXY_CLIENT_API_KEY`

完全一致。

### 访问不到 `127.0.0.1:8787`

优先检查：

1. `docker ps -a`
2. `docker logs <container-id-or-name>`
3. 是否已有别的程序占用了宿主机 `8787` 端口

### HTTPS 后端访问失败

镜像里已经包含了 `ca-certificates`。如果仍失败，优先检查：

- backend URL 是否正确
- 本机 Docker 网络是否能访问外网
- 后端证书链是否异常
