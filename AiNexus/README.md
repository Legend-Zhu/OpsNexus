# AiNexus

AI 对话网关 — 支持任意 OpenAI 兼容 / Anthropic 兼容 API，内置命令执行、MCP 工具调用，对外提供双格式流式接口。

## 特性

- 🤖 **多后端支持** — 同时对接 OpenAI、GLM、DeepSeek、Moonshot、Ollama、Claude 等任意兼容 API
- 📦 **多模型配置** — 每个 Provider 可配置多个模型，按 model 名称自动路由
- 🔄 **双格式接口** — 对外同时暴露 OpenAI (`/v1/chat/completions`) 和 Anthropic (`/v1/messages`) 格式的流式/非流式接口
- 🛠️ **内置工具** — 命令执行、HTTP 请求、文件读取
- 🔌 **MCP 集成** — 支持 stdio / SSE / Streamable HTTP 三种 MCP 传输协议，自动发现和调用远程工具
- 🧠 **ReAct Agent** — 自动多轮工具调用循环，支持并发执行
- 🌊 **流式输出** — 完整的 SSE 流式响应，实时推送文本增量和工具调用状态
- 🔐 **API Key 鉴权** — 支持 Bearer Token 和 x-api-key 两种鉴权方式

## 快速开始

### 构建

```bash
cd AiNexus
go mod tidy
go build -o ainx ./cmd/ainx
```

### 配置

复制示例配置并修改：

```bash
cp configs/config.yaml configs/config.local.yaml
# 编辑 config.local.yaml，填入 API Key
```

核心配置说明：

```yaml
providers:
  # 智谱 GLM — 使用 openai_compatible 类型
  - name: "zhipu"
    type: "openai_compatible"
    base_url: "https://open.bigmodel.cn/api/paas/v4"
    api_key: "your-zhipu-api-key"
    models:
      - name: "glm-5.1"
      - name: "glm-4-plus"

  # OpenAI
  - name: "openai"
    type: "openai_compatible"
    base_url: "https://api.openai.com/v1"
    api_key: "sk-xxx"
    models:
      - name: "gpt-5.4"
      - name: "gpt-4o"

  # DeepSeek — 也是 openai_compatible
  - name: "deepseek"
    type: "openai_compatible"
    base_url: "https://api.deepseek.com/v1"
    api_key: "your-deepseek-api-key"
    models:
      - name: "deepseek-chat"

  # 本地 Ollama — 也是 openai_compatible
  - name: "ollama"
    type: "openai_compatible"
    base_url: "http://localhost:11434/v1"
    api_key: "ollama"
    models:
      - name: "qwen3:32b"

  # Anthropic Claude — 使用 anthropic_compatible 类型
  - name: "anthropic"
    type: "anthropic_compatible"
    base_url: "https://api.anthropic.com"
    api_key: "sk-ant-xxx"
    models:
      - name: "claude-sonnet-4-20250514"
```

### Provider 类型说明

| type | 含义 | 适用服务 |
|------|------|---------|
| `openai_compatible` | 兼容 OpenAI Chat Completion API 格式 | OpenAI、GLM、DeepSeek、Moonshot、Ollama、vLLM、LiteLLM、任何 OpenAI 兼容服务 |
| `anthropic_compatible` | 兼容 Anthropic Messages API 格式 | Anthropic Claude、任何 Anthropic 兼容服务 |

**关键点**：只要 API 格式兼容 OpenAI，`type` 就填 `openai_compatible`。智谱、DeepSeek、Ollama 等都是 OpenAI 兼容格式，所以都用这个类型。

### 运行

```bash
./ainx -config configs/config.local.yaml
```

启动后输出：

```
[AiNexus] Model routed: glm-5.1 -> zhipu (openai_compatible)
[AiNexus] Model routed: gpt-5.4 -> openai (openai_compatible)
[AiNexus] Model routed: deepseek-chat -> deepseek (openai_compatible)
[AiNexus] Model routed: claude-sonnet-4-20250514 -> anthropic (anthropic_compatible)
[AiNexus] Server starting on :8080
[AiNexus]   OpenAI API:    http://localhost:8080/v1/chat/completions
[AiNexus]   Anthropic API: http://localhost:8080/v1/messages
[AiNexus]   Available models: [glm-5.1 gpt-5.4 deepseek-chat claude-sonnet-4-20250514]
```

## API 接口

### OpenAI 格式 — 通过 model 字段自动路由

```bash
# 使用 GLM 5.1
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_GATEWAY_KEY" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'

# 使用 GPT 5.4
curl -X POST http://localhost:8080/v1/chat/completions \
  -d '{"model": "gpt-5.4", "messages": [{"role": "user", "content": "hello"}], "stream": false}'

# 使用 DeepSeek
curl -X POST http://localhost:8080/v1/chat/completions \
  -d '{"model": "deepseek-chat", "messages": [{"role": "user", "content": "hello"}]}'

# 使用 Ollama 本地模型
curl -X POST http://localhost:8080/v1/chat/completions \
  -d '{"model": "qwen3:32b", "messages": [{"role": "user", "content": "hello"}]}'
```

### Anthropic 格式 — 同样通过 model 字段路由

```bash
# 使用 Claude
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: YOUR_GATEWAY_KEY" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 4096,
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### 列出所有可用模型

```bash
curl http://localhost:8080/v1/models
```

返回示例：

```json
{
  "object": "list",
  "data": [
    {"id": "glm-5.1", "object": "model", "owned_by": "zhipu"},
    {"id": "gpt-5.4", "object": "model", "owned_by": "openai"},
    {"id": "deepseek-chat", "object": "model", "owned_by": "deepseek"},
    {"id": "claude-sonnet-4-20250514", "object": "model", "owned_by": "anthropic"}
  ]
}
```

### 管理接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET  | /health | 健康检查 |
| GET  | /v1/models | 列出所有可用模型 |
| GET  | /api/models | 模型详情（含 provider、type） |
| GET  | /api/tools | 列出已注册工具 |
| GET  | /api/mcp | 列出 MCP Server 状态 |

## 内置工具

### command_executor — 命令执行

执行 shell 命令并返回输出。

```json
{
  "command": "ls -la /tmp",
  "work_dir": "/home/user",
  "timeout": 10
}
```

参数：
- `command` (string, 必需) — 要执行的命令
- `work_dir` (string, 可选) — 工作目录
- `timeout` (number, 可选) — 超时秒数

### http_request — HTTP 请求

发送 HTTP 请求。

```json
{
  "url": "https://api.example.com/data",
  "method": "GET",
  "headers": {"Authorization": "Bearer xxx"},
  "timeout": 10
}
```

参数：
- `url` (string, 必需) — 请求 URL
- `method` (string, 可选, 默认 GET) — HTTP 方法
- `headers` (object, 可选) — 请求头
- `body` (string, 可选) — 请求体
- `timeout` (number, 可选) — 超时秒数

### file_read — 文件读取

读取文件内容，支持指定行范围。

```json
{
  "path": "/var/log/app.log",
  "offset": 1,
  "limit": 100
}
```

参数：
- `path` (string, 必需) — 文件路径
- `offset` (number, 可选) — 起始行号 (1-based)
- `limit` (number, 可选) — 最大行数

## MCP 集成

AiNexus 支持连接外部 MCP Server，自动发现其提供的工具并集成到 Agent 的工具调用中。

### Stdio 模式

```yaml
mcp_servers:
  - name: "filesystem"
    transport: "stdio"
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
```

### SSE 模式

```yaml
mcp_servers:
  - name: "legacy-tools"
    transport: "sse"
    url: "http://localhost:3002/sse"
```

### Streamable HTTP 模式

```yaml
mcp_servers:
  - name: "remote-tools"
    transport: "streamable-http"
    url: "http://localhost:3001/mcp"
```

## 架构

```
                    ┌─────────────────────────────────────────┐
                    │            AiNexus Gateway              │
                    │                                         │
  OpenAI 格式 ─────►│  ┌──────────┐                          │
  /v1/chat/          │  │ OpenAI   │   ┌───────────┐         │
  completions        │  │ Handler  │──►│           │         │
                    │  └──────────┘   │           │         │
                    │                 │   Agent   │         │
  Anthropic 格式───►│  ┌──────────┐   │  (ReAct)  │         │
  /v1/messages      │  │Anthropic │   │           │         │
                    │  │ Handler  │──►│           │         │
                    │  └──────────┘   └─────┬─────┘         │
                    │                       │               │
                    │              ┌────────┴────────┐      │
                    │              │                 │      │
                    │        ┌─────▼─────┐   ┌──────▼──┐   │
                    │        │   Tool    │   │   MCP   │   │
                    │        │ Registry  │   │ Manager │   │
                    │        └─────┬─────┘   └──────┬──┘   │
                    │              │                │      │
                    └──────────────┼────────────────┼──────┘
                                   │                │
              ┌─────────┬─────────┼──────┬─────────┼──────┬──────────┐
              │         │         │      │         │      │          │
         ┌────▼───┐ ┌──▼───┐ ┌──▼───┐ ┌▼──────┐ ┌▼─────┐ ┌▼────────┐
         │  GLM   │ │ GPT  │ │Deep  │ │Moon   │ │Ollama│ │Claude   │
         │  5.1   │ │ 5.4  │ │Seek  │ │shot   │ │本地  │ │Sonnet 4 │
         └────────┘ └──────┘ └──────┘ └───────┘ └──────┘ └─────────┘
```

## 项目结构

```
AiNexus/
├── cmd/ainx/main.go               # 入口
├── internal/
│   ├── config/config.go           # 配置加载（providers 列表 + 多模型）
│   ├── handler/
│   │   ├── openai_handler.go      # OpenAI 格式接口
│   │   └── anthropic_handler.go   # Anthropic 格式接口
│   ├── provider/
│   │   ├── provider.go            # Provider 接口
│   │   ├── openai.go              # OpenAI 兼容客户端（GLM/GPT/DeepSeek/Ollama 等）
│   │   └── anthropic.go           # Anthropic 兼容客户端（Claude 等）
│   ├── tool/
│   │   ├── types.go               # 工具类型
│   │   ├── registry.go            # 工具注册中心
│   │   ├── executor.go            # 命令执行
│   │   ├── http_request.go        # HTTP 请求
│   │   └── file_read.go           # 文件读取
│   ├── mcp/
│   │   ├── manager.go             # MCP Server 管理
│   │   └── mcp_tool.go            # MCP 工具适配
│   ├── agent/
│   │   ├── agent.go               # ReAct Agent
│   │   └── conversation.go        # 对话上下文
│   └── server/server.go           # HTTP 服务 + 模型路由
├── pkg/
│   ├── sse/writer.go              # SSE 流式写入
│   └── jsonutil/jsonutil.go       # JSON 工具
├── configs/config.yaml            # 示例配置
├── go.mod
└── README.md
```

## 兼容的客户端

由于 AiNexus 提供标准 OpenAI/Anthropic 格式接口，可直接对接：

- **OpenAI Python SDK** — `openai.ChatCompletion.create()`
- **Anthropic Python SDK** — `anthropic.Anthropic().messages.create()`
- **LangChain** — `ChatOpenAI` / `ChatAnthropic`
- **Cursor / Continue / Cline** — 设置 API Base URL 为 `http://localhost:8080/v1`
- 任何兼容 OpenAI/Anthropic API 的客户端

## 许可证

GNU Affero General Public License v3.0
