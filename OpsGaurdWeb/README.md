# OpsGaurd Web 管理端

智能运维平台的**集群管理端**（类 Rancher）：管理多个 Docker Swarm 集群（经各集群 Worker 的 HTTP API / MCP），并整合 **AiNexus**（AI 对话网关/Agent 服务，仓库根 `./AiNexus`）做异常排查。

```
┌──────────────── OpsGaurdWeb 管理端 ────────────────┐
│                                                    │
│  web/       前端：Vue3 + TypeScript + Element Plus  │
│              ├─ 仪表盘 / 集群管理 / 工作负载 / 异常排查 │
│              └─ axios → /api（开发期代理）            │
│                                                    │
│  server/    后端：Go + Gin                         │
│              ├─ /api/v1/clusters       多集群管理    │
│              ├─ /api/v1/clusters/:name/workloads    │
│              ├─ /api/v1/clusters/:name/events|audit │
│              └─ /api/v1/ainexus/*      AiNexus 代理  │
└──────────────┬───────────────────┬─────────────────┘
               │ 代理              │ 代理（SSE 透传）
        ┌──────▼──────┐     ┌──────▼──────┐
        │ 集群 Worker  │     │  AiNexus    │
        │ (swarm 编排/ │     │  AI 网关    │
        │  监控/MCP)   │     │  (Agent)    │
        └─────────────┘     └─────────────┘
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
    ├── cmd/server/main.go        # 入口
    ├── internal/
    │   ├── api/                  # handlers（骨架占位）
    │   ├── config/               # 配置加载
    │   └── router/               # 路由注册
    └── configs/config.yaml       # 配置示例
```

## 快速开始

### 后端

```bash
cd OpsGaurdWeb/server
cp configs/config.yaml configs/config.local.yaml   # 按需修改
go mod tidy
go run ./cmd/server -config configs/config.local.yaml
# 默认 :8090，healthz: GET /healthz
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
| AI 排查 | AiNexus（独立服务，SSE 代理整合，见仓库根 `./AiNexus`） |

> 当前为**骨架**：路由/页面/API 签名已就位，业务逻辑（集群接入、Worker 代理、AiNexus SSE 透传）待迭代。
