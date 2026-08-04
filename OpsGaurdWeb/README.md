# OpsGaurd Web 管理端

智能运维平台的**集群管理端**（类 Rancher）：管理多个 Docker Swarm 集群（经各集群 Worker 的 HTTP API / MCP），并把 **AiNexus**（AI 对话网关/Agent）**内嵌进后端进程**做异常排查——非独立服务，无独立端口，无进程间 HTTP。

```
┌──────────────── OpsGaurdWeb 管理端（单进程） ────────────────┐
│                                                              │
│  web/       前端：Vue3 + TypeScript + Element Plus           │
│              ├─ 仪表盘 / 集群管理 / 工作负载 / 异常排查        │
│              └─ axios → /api（开发期代理）                    │
│                                                              │
│  server/    后端：Go + Gin                                   │
│              ├─ /api/v1/clusters        多集群管理            │
│              ├─ /api/v1/clusters/:name/workloads             │
│              ├─ /api/v1/clusters/:name/events|audit          │
│              ├─ /api/v1/ainexus/*       AiNexus（进程内直调） │
│              └─ /ainexus/*              内嵌 AiNexus 原生端点 │
│                    （providers/tools/MCP/Agent 同进程）        │
└──────────────┬───────────────────────────────────────────────┘
               │ 代理（Worker HTTP API + MCP）
        ┌──────▼──────┐
        │ 集群 Worker  │  ← 内嵌 AiNexus 的 MCP 客户端连 Worker /mcp
        │ (swarm 编排/ │    采集证据（16 工具：编排/监控/命令执行）
        │  监控/MCP)   │
        └─────────────┘
```

## 目录结构

```
OpsGaurdWeb/
├── web/                          # 前端（Vite + Vue3 + TS + Element Plus）
│   ├── src/
│   │   ├── api/                  # axios 封装 + 各模块 API 骨架
│   │   ├── router/               # 路由（仪表盘/集群/工作负载/异常排查）
│   │   ├── layout/               # 主布局（侧边栏 + 顶栏）
│   │   ├── views/                # 占位页面
│   │   └── types/                # 领域类型
│   └── vite.config.ts            # dev 代理 /api → :8090
└── server/                       # 后端（Go + Gin）
    ├── cmd/server/main.go        # 入口（初始化内嵌 AiNexus 网关）
    ├── internal/
    │   ├── api/                  # handlers（骨架占位 + ainexus 内嵌端点）
    │   ├── ainexus/              # AiNexus 内嵌网关（vendor 自仓库根 ./AiNexus）
    │   │   ├── config/           #   网关配置（providers/tools/mcp_servers/agent）
    │   │   ├── provider/         #   多模型 Provider（openai/anthropic 兼容）
    │   │   ├── tool/             #   工具注册中心 + 内置工具
    │   │   ├── mcp/              #   MCP 客户端管理（stdio/SSE/Streamable HTTP）
    │   │   ├── agent/            #   ReAct Agent
    │   │   ├── handler/          #   OpenAI/Anthropic 格式 gin handlers
    │   │   └── server/           #   内嵌网关（无独立 HTTP 层，直接挂载路由）
    │   ├── config/               # 管理端配置加载
    │   └── router/               # 路由注册（含 /ainexus/* 原生端点）
    └── configs/config.yaml       # 配置示例（ainexus.providers / mcp_servers）
```

## 快速开始

### 后端

```bash
cd OpsGaurdWeb/server
cp configs/config.yaml configs/config.local.yaml   # 按需修改（ainexus.providers 至少配一个模型）
go mod tidy
go run ./cmd/server -config configs/config.local.yaml
# 默认 :8090，healthz: GET /healthz
# 内嵌 AiNexus：GET /ainexus/health、POST /ainexus/v1/chat/completions（SSE 流式）
```

### 前端

```bash
cd OpsGaurdWeb/web
npm install
npm run dev        # http://localhost:5173（/api 代理到 :8090）
npm run build      # 产物 dist/
```

## 技术栈

| 层 | 选型 |
|---|---|
| 前端 | Vue 3 + TypeScript + Vite + Element Plus + Pinia + Vue Router |
| 后端 | Go + Gin |
| AI 排查 | AiNexus（**内嵌进后端**，vendor 自仓库根 `./AiNexus`，单进程运行） |

> 当前为**骨架**：路由/页面/API 签名已就位，内嵌 AiNexus 网关已可运行；业务逻辑（集群接入、Worker 代理、深度排查闭环）待迭代。
