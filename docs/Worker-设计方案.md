# Worker 设计方案

> 版本：v0.2（草案）
> 日期：2026-08-06
> 状态：调研完成，待评审（历史方案文档；2026-08-21 已按当前代码复核并修正明显漂移，实现现状以《OpsGaurd-系统技术总览.md》为准）

> **v0.2 变更摘要**：管理端↔Worker 的管理 API 从 **HTTP 改为 gRPC**。事件/审计投递由 webhook 推送改为 **gRPC 双向流订阅**（server 发起连接，顺网络策略方向）；`EventStore`/`audit.Store` 由内存 ring buffer 改为 **SQLite 持久化队列**；Worker 拆为**双端口**（HTTP 仅留 `/mcp` + `/healthz` + node 本地 API，gRPC 承载完整 `ManagementService`）；leader 写转发由 HTTP `ProxyWriteToLeader` 改为 **gRPC internal client**；新增 `internal/grpcapi`、`internal/authz/grpc.go`，移除 `internal/monitor/webhook.go`、`internal/orchestrator/api.go`、`ProxyWriteToLeader` 及 `/api/v1/events`/`/api/v1/audit`/`/api/v1/services*` 管理 HTTP 路由。MCP 端点（`/mcp`）、node 本地 API（`/api/v1/local/*`）、Docker 直连客户端不变。

## 一、背景与目标

OpsGaurd 定位为"智能运维全链路自动化平台"。`Worker` 是部署在被纳管 Docker 主机上的核心组件，承担三类职责：

| 模块 | 职责 | 对标 |
|---|---|---|
| ① 编排 | 暴露 gRPC `ManagementService`，接收管理端下发的 `config`，自动拉取镜像并启动/更新/销毁容器 | 类 Rancher，但基于 **Docker Swarm** |
| ② 监控 | 根据配置，监控 **端口连通性 / 接口异常 / 日志异常 / 资源使用** | 内置轻量监控探针 |
| ③ MCP | 暴露 MCP（Model Context Protocol）Server，供 Agent 调用编排与监控能力 | 让 LLM Agent 可运维集群 |

**为什么选 Docker Swarm 而非 Kubernetes**：Swarm 原生内置于 Docker 引擎，`docker swarm init` 一条命令成集群，无额外控制面组件，运维与学习成本低，服务模型（副本/滚动更新/回滚/健康检查/资源限制/overlay 网络/secret）已覆盖大多数无状态与轻量有状态场景，契合 OpsGaurd"轻量、快速起步"的定位。大规模/Operator/复杂调度场景才需 K8s，不在 Worker 当前目标内。

**调研结论速览**（详见附录 A/B）：
- Docker Swarm：服务全生命周期走 `POST /services/create`、`POST /services/{id}/update`、`DELETE /services/{id}`、`GET /services/{id}/logs`；就绪判据=任务 `State==running` 且 `DesiredState==running` 且副本数达标 + 容器 `Health.Status==healthy`；官方 Go SDK `github.com/docker/docker/client` 为 Tier-1。
- MCP：**最新规范版本 `2026-07-28`**，已**无状态化**（取消 `initialize` 握手与协议级会话），每请求在 `_meta` 携带 `protocolVersion`/`clientCapabilities`；标准传输为 **stdio + Streamable HTTP** 两种；远程 HTTP 配 **OAuth 2.1**；原语用 **Tools/Resources/Prompts/Elicitation**，**勿用 Roots/Sampling/Logging**（已弃用）。官方 Go SDK `github.com/modelcontextprotocol/go-sdk` 为 Tier-1。

---

## 二、总体架构

### 2.1 部署模型

Worker 以 **Docker Swarm 全局服务（global service）** 形式部署到所有纳管节点，挂载 `/var/run/docker.sock`。单二进制，按节点角色启用不同能力集：

```
┌─────────────────────────────── Swarm Cluster ───────────────────────────────┐
│                                                                             │
│  ┌──────── Manager Node ────────┐        ┌──────── Worker Node ──────────┐  │
│  │  Worker (manager-role)        │        │  Worker (worker-role)         │  │
│  │  ├─ Orchestrator   (swarm API)│◀──────▶│  └─ Local Monitor             │  │
│  │  ├─ ServiceMonitor (svc logs, │  汇聚  │     (本地容器 stats / 本地日志) │  │
│  │  │  port/http probe)          │  上报   │                               │  │
│  │  ├─ gRPC Server (Management)  │        │  ├─ HTTP API (只读/代理)       │  │
│  │  ├─ MCP Server (Stream HTTP)  │        │  └─ MCP Server (只读/代理)      │  │
│  │  └─ EventStore (SQLite 队列)   │        │                               │  │
│  └───────────────────────────────┘        └───────────────────────────────┘  │
│       │ :9080 gRPC │ :8080 HTTP                │ :8080 HTTP（node 角色不启 gRPC）│
│                  ▲                                                             │
│                  │ docker.sock / 2376(TLS)                                      │
│                  ▼                                                             │
└─────────────────┬───────────────────────────────────────────────────────────┘
                  │ gRPC（双向流：server 发起 SubscribeEvents/SubscribeAudit）
        ┌─────────┴─────────┐
        │  OpsGaurdWeb（管理端）│  ──▶ ManagementService RPC（config 下发、事件流回推）
        └───────────────────┘
                  ▲
                  │ MCP（Streamable HTTP + OAuth2.1）
        ┌─────────┴─────────┐
        │  LLM Agent / Host  │
        └───────────────────┘
```

**角色判定**：Worker 通过 `/info` 的 `Swarm.NodeID`（权威，非 hostname 匹配）读取自身节点，结合 `Spec.Role` 判定；agent config `worker.role` 可显式指定 `manager`/`node` 覆盖 auto 推导。

- **manager-role Worker**：独占编排能力（`ensureSwarmManager` 守卫写操作）；承担**服务级**监控（service logs、routing-mesh 端口/HTTP 探活）；聚合所有节点上报的本地指标；是事件总线中心；MCP Server 端点。**双端口**：`:9080`（gRPC，承载完整 `ManagementService`，供管理端与 leader 写转发）+ `:8080`（HTTP，仅 `/mcp` + `/healthz` + node 本地 API）。
- **node-role Worker**（每台纳管服务器都部署）：提供**本地接口** `GET /api/v1/local/stats`、`GET /api/v1/local/processes`（宿主机进程）、`POST /api/v1/local/exec`、`POST /api/v1/local/host`、`GET /api/v1/local/logs`（SSE 流式日志）、`GET /api/v1/local/check/port` / `POST /api/v1/local/check/http`（一次性探测），供 manager 跨节点代理采集。
- **跨节点代理（manager→node）**：manager 的 MCP 工具（`get_resource_usage`/`exec_in_container`/`exec_host_command`）按任务所在节点路由到对应 node worker 的本地接口，实现**跨节点指标聚合与命令执行**。
- **Leader 选举**：swarm manager 集群本身有 Raft leader。manager-role Worker 中仅 `ManagerStatus.Leader==true` 者执行"写"编排与全局事件汇聚；其余 manager 实例做热备——非 leader 实例上的写 RPC（Deploy/Update/Scale/Restart/Remove）经 gRPC 内部客户端转发到 leader 的 `:9080`，转发时携带原 bearer token，使 leader 记录同一审计 actor。

> **简化部署（小集群可选）**：仅部署 manager 节点一个 Worker 实例，跳过 per-node agent；但跨节点 stats/exec 会受限（只能覆盖 manager 本机副本），不推荐用于 >1 节点。

### 2.2 组件分层

```
Worker 单二进制（Go）
├── internal/docker/        Docker Engine REST API 客户端（stdlib net/http，直接 HTTP，无 SDK）
│   ├── client.go           构造（DOCKER_HOST/env）+ do/getJSON/postJSON 等
│   ├── transport.go        unix socket / tcp+TLS（2376）双 transport
│   ├── service.go          Create/Update/Remove/Scale/Restart/List/Inspect
│   ├── task.go             List（就绪判定）
│   ├── node.go             List/Self（角色判定）
│   ├── image.go            ImagePull / GetSecret（registry auth）
│   ├── logs.go / stats.go  ServiceLogs / ContainerList / ContainerStats / ContainerInspect
│   └── types.go            Engine API 请求/响应结构体（仅子集）
├── internal/orchestrator/  模块①：配置驱动编排（管理 HTTP handler 已移除，能力由 gRPC ManagementService 暴露）
│   ├── translator.go       Config → swarm.ServiceSpec 双向映射
│   ├── lifecycle.go        Create→轮询就绪→事件；Update→滚动
│   ├── operation.go        异步 operation 记录
│   └── proxy.go / nodes.go 跨节点 NodeClient 代理 / 节点地址解析
├── internal/grpcapi/       管理 API（gRPC，契约 proto/opsguard.proto）
│   ├── server.go           ManagementService 实现（liveness/workload/nodes/probes/events/audit）+ leader 写转发
│   ├── stream.go           SubscribeEvents / SubscribeAudit 双向流；StreamLogs server-streaming
│   ├── tunnel.go           Tunnel 反向隧道（流池；承载 IdP 代理与 /v2 镜像中继）
│   └── pb/                 protoc-gen-go 生成代码（proto/opsguard.proto 生成本模块与 server 模块）
├── internal/authz/         鉴权
│   ├── authz.go            HTTP bearer-token 中间件（Wrap 全路由，PublicPaths 白名单放行 /healthz 等）
│   └── grpc.go             gRPC bearer-token 拦截器（unary + stream，与 HTTP 同 token 集）
├── internal/monitor/       模块②：监控
│   ├── manager.go          监控调度器（按 config 起停 checker）
│   ├── portcheck.go        TCP 连通性探针
│   ├── httpcheck.go        HTTP 健康探针（状态码/体匹配）
│   ├── logcheck.go         服务日志流 + 关键词/正则匹配
│   ├── rescheck.go         资源使用采集（阈值告警）
│   └── event.go            事件存储（SQLite 持久化队列：Add/Since/Ack/GC/WaitNew）
├── internal/audit/         审计（SQLite 持久化队列，actor 取 "token名@客户端IP"，auth 关闭时为客户端 IP，stdio 会话为 "stdio"）
├── internal/idpproxy/      /idp-proxy/ 本地入口：IdP 请求经反向隧道转发管理端（发现文档改写）
├── internal/registryproxy/ /v2/ 镜像中继（pull/push 经隧道流式转发 + blob 磁盘 LRU 缓存）
├── internal/mcp/           模块③：MCP Server
│   ├── server.go           go-sdk 注册 tools/resources（Streamable HTTP + stdio）
│   ├── tools.go            编排/日志/事件/操作类工具
│   ├── metrics.go          exec/资源采集类工具
│   └── checks.go           拨测类工具（check_port/check_http/check_flow/list_host_processes）
├── internal/config/        Config schema + 手写校验（config.go/validate.go/defaults.go 等）
├── （无独立 ha 包：leader 写转发在 grpcapi/server.go 内以内部 gRPC 客户端实现）
├── cmd/worker/main.go      入口
├── proto/opsguard.proto    gRPC 契约（仓库根）
└── deploy/                 Dockerfile、stack.yml、systemd unit
```

> **已移除（gRPC 迁移）**：`internal/monitor/webhook.go`（WebhookPusher）、`internal/orchestrator/api.go`（管理 HTTP handler）、`ProxyWriteToLeader`（HTTP 写转发）、`/api/v1/events`、`/api/v1/audit`、`/api/v1/services*` 等 HTTP 管理路由。（注：`EventStore.AddSink` 实际仍保留，供事件/审计挂 sink 使用。）

### 2.3 数据流

1. **部署流**：OpsGaurdWeb 调 gRPC `ManagementService.Deploy`（带 config）→ leader manager-role Worker → `translator` 转 `swarm.ServiceSpec` → `cli.ServiceCreate` → swarm 调度 task 到各节点 → 各节点 Worker 观察本地 task → 就绪/异常事件 → `EventStore.Add` → gRPC `SubscribeEvents` 流回推管理端（管理端 ack seq 后 Worker GC）→ 回写部署结果。非 leader manager 收到 Deploy 等写 RPC 时，经 gRPC 内部客户端转发到 leader `:9080`（携带原 bearer token，保持审计 actor 一致）。
2. **监控流**：各 checker 周期性探活/采集 → 命中阈值 → `EventStore.Add`（SQLite，分配单调 `seq`）→ 管理端通过已建立的 `SubscribeEvents` 双向流收到事件（阻塞式 `WaitNew(lastSeen)` 在有新事件时唤醒）→ 管理端回 `Ack(seq)` 后 Worker `GC`；MCP 侧（若启用 `subscriptions/listen`）走 SSE 通知订阅方拉取。
3. **Agent 流**：Agent `tools/call` → MCP Server（`/mcp`，Streamable HTTP）→ 调 orchestrator/monitor 能力 → 返回 `structuredContent`。

---

## 三、技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 语言 | **Go 1.25**（go.mod） | .gitignore 已含 Go 模式；Docker/MCP 均 Tier-1 官方 Go SDK；单二进制便于全局服务部署 |
| Docker 客户端 | **直接 HTTP 调用 Engine REST API**（stdlib `net/http` + 自定义结构体） | 见下方说明 |
| HTTP 路由 | ~~`github.com/go-chi/chi/v5`~~ 实际用 stdlib `http.ServeMux` | 初版选 chi，实现时未引入（路由面小：`/mcp` + `/healthz` + node 本地 API） |
| MCP SDK | `github.com/modelcontextprotocol/go-sdk` v1.7.0 | 官方 Tier-1，**目标协议版本即 2026-07-28**（stateless + `server/discover` + MRTR + subscriptions + Streamable HTTP 全内置） |
| 管理 API | **gRPC**（`google.golang.org/grpc` + `protoc-gen-go`） | 双向流原生支持 `SubscribeEvents`/`SubscribeAudit`；强类型契约 `proto/opsguard.proto`；服务端可发起连接（顺网络策略方向，仅 server→worker 可达） |

> **实现期变更（P0）**：Docker 客户端从官方 Go SDK 改为**直接 HTTP 调用 Engine REST API**（stdlib `net/http` + 自定义请求/响应结构体）。原因：`github.com/docker/docker` SDK 的 Go 模块结构不稳定——新版把 `api`/`client` 拆成路径不匹配的嵌套模块（`github.com/moby/moby/api`），旧版（v24）又触发 `distribution/reference` 传递依赖损坏，跨版本都无法干净构建。直接走 REST API 零该依赖、二进制更小（10MB）、攻击面更少，且与本文档已枚举的 REST 端点一致。代价是自维护 Engine API JSON 结构体（仅子集，未知字段忽略）。transport 支持 `unix:///var/run/docker.sock`（默认/Linux）与 `tcp://host:2376`（+TLS）；Windows 本地开发用 Docker Desktop 的 `tcp://localhost:2375`（npipe 暂不支持，保持 stdlib-only）。
| 配置校验 | ~~`github.com/santhosh-tekuri/jsonschema/v6`~~ 实际手写 Go 校验（`internal/config/validate.go`） | 初版选 JSON Schema 2020-12，实现时改为纯代码校验 |
| 日志 | `log/slog`（标准库结构化日志） | Go 1.21+ 内置，契合 MCP 弃用 `logging` 后走 stderr 的指引 |
| 探针 | `net.Dialer`（TCP）、`net/http`（HTTP）、`cli.ServiceLogs`（日志流） | 标准库 + Docker SDK |
| 存储 | **SQLite 持久化队列**（`modernc.org/sqlite`，纯 Go 无 CGO） | 事件/审计可跨重启、抗事件风暴（上限=磁盘）；单调 `seq` + `Ack`/`GC` 支撑 gRPC 流回推；替代旧内存 ring buffer |
| 部署 | Dockerfile + `deploy/stack.yml`（swarm stack） | Worker 自身也用 swarm 管理，自举 |

---

## 四、模块①：配置驱动的容器编排

### 4.1 管理 API（gRPC ManagementService）与残留 HTTP 接口

> **v0.2 变更**：编排/管理类接口（服务 CRUD、操作跟踪、事件、审计、节点、探活代理等）已**整体迁移到 gRPC**（`proto/opsguard.proto` 的 `ManagementService`）。原 `internal/orchestrator/api.go` 的 HTTP handler、`/api/v1/services*`、`/api/v1/events`、`/api/v1/audit` 等 HTTP 管理路由**已删除**。HTTP 端口现在仅承载三类**与 gRPC 无关**的端点：`/mcp`（MCP Streamable HTTP）、`/healthz`、node 本地 API（`/api/v1/local/*`，供 manager 跨节点代理）。

Worker 暴露**两个端口**（部署相关的具体端口号可调，下文以默认值示意）：

| 端口 | 协议 | 承载 |
|---|---|---|
| `:9080`（默认） | **gRPC** | 完整 `ManagementService`（见下表）：liveness、workload CRUD、nodes、probes、events/audit 点查、`StreamLogs`（server-streaming）、`SubscribeEvents`/`SubscribeAudit`（bidirectional streaming） |
| `:8080`（默认） | **HTTP** | 仅 `/mcp` + `/healthz` + node 本地 API（`/api/v1/local/*`） |

#### 4.1.1 gRPC ManagementService（`:9080`）

契约见仓库根 `proto/opsguard.proto`，生成代码落在 `internal/grpcapi/pb`（并同时生成到管理端模块）。鉴权：bearer-token 经 gRPC unary+stream 拦截器（`internal/authz/grpc.go`）校验，与 HTTP 同一 token 集（见 §七）。

| RPC | 类型 | 说明 |
|---|---|---|
| `Ping` | unary | 存活/就绪探针（管理端健康检查、leader 探测） |
| `Self` | unary | 本节点 swarm 角色（nodeId/role/leader/swarmManager） |
| `ListServices` / `GetService` | unary | 服务列表（`?label`）/ 详情 + task 概览 + 健康 |
| `Deploy` | unary（写） | 创建服务（入参=完整 config）；同名已存在 → 失败并提示用 `Update` |
| `Update` | unary（写） | 更新服务（新 config 全量替换；乐观并发用 `version`） |
| `Scale` | unary（写） | 调整副本数 `{replicas:N}` |
| `Restart` | unary（写） | 强制重调度（`--force` 等价） |
| `Remove` | unary（写） | 删除服务 |
| `GetOperation` | unary | 操作跟踪（`operationId`） |
| `ListNodes` / `NodeStats` | unary | 集群节点列表 / 节点资源聚合 |
| `WatchNodeStats` | **server-streaming** | 节点列表 + 各节点指标流（首帧基础列表立即返回，后续逐节点补指标，慢节点不阻塞他人） |
| `NodeProcesses` | unary | 节点进程代理（`top`/`limit`/`filter` 转发；id 支持 node ID 或 hostname） |
| `NodeContainers` / `RestartContainer` | unary | 节点容器列表（standalone 纳管用）/ 重启指定容器 |
| `CheckPort` / `CheckHTTP` / `CheckFlow` | unary | 节点级 TCP / HTTP / 多步事务探测代理（转发到目标 node 的本地探测） |
| `ListEvents` | unary | 事件点查（`?service=&type=&limit=`，从 SQLite 队列读，新→旧） |
| `ListAudit` | unary | 审计点查（从 SQLite 审计队列读） |
| `StreamLogs` | **server-streaming** | 服务日志流（等价 `docker service logs -f`，单连接单向推送） |
| `SubscribeEvents` | **bidirectional** | 管理端发起；Worker 推事件（带 `seq`），管理端回 `Ack(seq)`，Worker 据此 `GC` 已确认条目 |
| `SubscribeAudit` | **bidirectional** | 同上，针对审计条目 |
| `Tunnel` | **bidirectional** | 反向隧道：server 发起，承载 IdP 代理与 `/v2` 镜像中继（流池化，见《镜像隧道中继方案.md》） |

> 注（2026-08-21 复核）：方案期规划的 `Rollback` RPC 及 `GetNode`/`GetEvent` 点查**未实现**；主动回滚能力未落地（swarm 引擎级 `failureAction=rollback` 仍生效）。

**leader 写转发**：非 leader 的 manager-role Worker 收到写 RPC（`Deploy`/`Update`/`Scale`/`Restart`/`Remove`）时，gRPC server 持有的内部 gRPC 客户端将请求转发到 leader 的 `:9080`，转发时携带原 bearer token（metadata），使 leader 记录同一审计 actor。这取代了旧的 HTTP `ProxyWriteToLeader`。

**幂等**：`Deploy` 若同名已存在 → 失败并提示用 `Update`；`Scale` 可重试。

#### 4.1.2 残留 HTTP 接口（`:8080`）

**本地节点接口（每台纳管服务器，`/api/v1/local/...`，供 manager 跨节点代理；未迁移 gRPC）**

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/api/v1/local/stats` | 本节点 swarm 容器实时 CPU%/Mem%（快照差值） |
| `GET` | `/api/v1/local/processes` | 宿主机进程列表（纯 Go 扫 /proc；`?top=cpu|mem&limit=N&filter=名称/cmdline 子串`） |
| `POST` | `/api/v1/local/exec` | 容器内执行命令（`{container|service+slot, command}`，走命令策略） |
| `POST` | `/api/v1/local/host` | 宿主机执行命令（nsenter，走命令策略） |
| `GET` | `/api/v1/local/logs` | **SSE 流式日志**：`?service=&follow=true|false&tail=N&since=`，等价 `docker service logs -f` |
| `GET` | `/api/v1/local/check/port` | 一次性 TCP 探测任意 host:port（`?host=&port=&timeout=`，只读不挂命令策略） |
| `POST` | `/api/v1/local/check/http` | 一次性 HTTP 探测任意 URL（`{url,method,headers,expectedStatus,expectedBody,timeout}`） |

**其他 HTTP 端点**：`/mcp`（MCP，见 §六）、`/healthz`（公开）。`/.well-known/oauth-protected-resource`（RFC 9728）公开。

**鉴权**：HTTP 侧用 bearer-token 中间件（`internal/authz/http.go`）包住 `/api/v1/local/*`（可执行宿主机命令）与 `/mcp`；`/healthz`、`/.well-known/*` 公开。`/local/*` 含命令执行能力，生产须置于仅内网/带鉴权代理后。管理端→Worker 的 gRPC 可叠加 mTLS（与 swarm manager 2376 同套体系）。

### 4.2 Config Schema（YAML/JSON，JSON Schema 2020-12）

config 是 Worker 的"单一事实来源"，同时驱动编排与监控。设计目标：比裸 swarm API 更友好，但**可无损映射**到 `swarm.ServiceSpec`。

```yaml
# ---- 服务定义 ----
service:
  name: my-app
  image: registry.example.com/my-app:v1.2.3
  mode: replicated          # replicated | global
  replicas: 3               # mode=replicated 时生效

  # 镜像拉取
  imagePullPolicy: always   # always | missing | never（映射 TaskTemplate.PullMode）
  registryAuth:
    secretRef: regcred      # swarm secret 名，内容为 ~/.docker/config.json base64

  # 网络与端口
  networks: [my-overlay]
  ports:
    - { published: 8080, target: 80, protocol: tcp, mode: ingress }  # ingress|host

  # 运行时
  env: [DEBUG=true, DB_URL=postgres://...]
  command: ["./app"]
  args: ["--port", "8080"]
  workdir: /app
  user: "1000:1000"
  mounts:
    - { type: volume, source: data, target: /data, readonly: false }
  secrets: [db-password]    # swarm secret 引用名
  configs: [app-conf]       # swarm config 引用名
  labels: { app: my-app, tier: web }

  # 资源
  resources:
    limits:    { cpu: "1.0", memory: "512Mi" }
    reservations: { cpu: "0.5", memory: "256Mi" }

  # 健康检查
  healthcheck:
    test: ["CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"]
    interval: 10s
    timeout: 5s
    retries: 3
    startPeriod: 30s

  # 调度
  placement:
    constraints: ["node.labels.role==web"]
    preferences: [{ spread: node.labels.zone }]

  # 更新/回滚
  update:
    parallelism: 1
    delay: 10s
    failureAction: rollback   # pause | continue | rollback
    monitor: 30s
    maxFailureRatio: 0.0
  rollback:                    # 同 update 字段
    parallelism: 1
    failureAction: pause

  # 重启策略
  restart:
    condition: any            # any | on-failure | none
    delay: 5s
    maxAttempts: 3
    window: 120s

  # 日志驱动
  logDriver: { name: json-file, options: { "max-size": "10m", "max-file": "3" } }

# ---- 监控定义（模块②） ----
monitoring:
  enabled: true
  portChecks:
    - { port: 8080, protocol: tcp, interval: 10s, timeout: 3s, retries: 2 }
  httpChecks:
    - { url: "http://localhost:8080/health", method: GET,
        expectedStatus: [200], expectedBody: "", interval: 15s, timeout: 5s }
  logChecks:
    - { pattern: "ERROR|Exception|panic|fatal", level: error,
        ignore: ["expected-warning"], action: alert }   # alert | restart
  resourceThresholds:
    - { metric: cpu, threshold: 80, action: alert }     # 百分比
    - { metric: memory, threshold: 85, action: alert }
```

### 4.3 Config → swarm.ServiceSpec 映射要点

| Config 字段 | swarm.ServiceSpec 路径 | 备注 |
|---|---|---|
| `image` | `TaskTemplate.ContainerSpec.Image` | |
| `imagePullPolicy` | `TaskTemplate.PullMode` | `always`→`always`，`missing`→`missing`，`never`→`never` |
| `registryAuth.secretRef` | `TaskTemplate.ContainerSpec.Secrets` + `AuthConfig` | 拉取前用 `cli.RegistryLogin` 或 secret 注入 |
| `mode`/`replicas` | `Mode.Replicated.Replicas` / `Mode.Global` | |
| `ports[]` | `EndpointSpec.Ports[]` | `PublishedPort`/`TargetPort`/`Protocol`/`PublishMode` |
| `env`/`command`/`args`/`workdir`/`user` | `ContainerSpec` 对应字段 | |
| `mounts` | `ContainerSpec.Mounts` | |
| `secrets`/`configs` | `ContainerSpec.Secrets`/`ContainerSpec.Configs`（带 `File`/`Mode`） | |
| `resources.limits` | `TaskTemplate.Resources.Limits` | `cpu`→`NanoCPUs`（×1e9），`memory`→`MemoryBytes`（解析 `512Mi`） |
| `resources.reservations` | `TaskTemplate.Resources.Reservations` | 同上 |
| `healthcheck` | `ContainerSpec.Healthcheck` | `Test`/`Interval`/`Timeout`/`StartPeriod`/`StartInterval`/`Retries`，Duration 用 `time.Duration`（Go SDK 接受，序列化为纳秒） |
| `placement.constraints` | `TaskTemplate.Placement.Constraints` | |
| `placement.preferences` | `TaskTemplate.Placement.Preferences` | `spread=<label>` |
| `update.*` | `UpdateConfig` | `Parallelism`/`Delay`/`FailureAction`/`Monitor`/`MaxFailureRatio` |
| `rollback.*` | `RollbackConfig` | 字段同 UpdateConfig |
| `restart.*` | `TaskTemplate.RestartPolicy` | `Condition`/`Delay`/`MaxAttempts`/`Window` |
| `logDriver` | `TaskTemplate.LogDriver` | `Name`/`Options` |
| `labels` | `Labels`（map） | |

### 4.4 生命周期与就绪判定

部署不是"调 `ServiceCreate` 即返回"，而是**异步收敛 + 状态回写**：

```
gRPC Deploy  ──▶ ServiceCreate  ──▶ 返回 operationId + 初始状态
        │
        └─▶ 后台轮询（指数退避，上限 30s/次，总超时默认 5min）
              ├─ ServiceInspect            → ServiceStatus.RunningTasks / DesiredTasks
              ├─ TaskList?service={id}     → 逐 task 检查
              └─ 就绪判据：
                    ① running task 数 == replicas（global：每节点一个 running）
                    ② 每个 task Status.State==running 且 DesiredState==running
                    ③ 容器 Health.Status==healthy（若配了 healthcheck）
              ├─ 命中就绪 → EventStore.Add(status=healthy) → 经 SubscribeEvents 流回推管理端
              └─ 超时/失败 → EventStore.Add(status=deploy_failed) + 错误摘要
                              （失败 task 的 `Status.Err` 字段是关键诊断信息）
```

**任务状态机**（单调推进，失败即整体重建替代任务）：
```
new → allocated → pending → assigned → accepted → preparing → ready → starting → running → complete
                                                                                   └→ shutdown
分支终态：rejected / failed / orphaned
```
- `pending` 长期停留 → 多半是节点全 drain、预留内存不足、placement 约束无法满足。
- `failed` → 取 `Task.Status.Err` + 容器退出码定位。

**滚动更新**：gRPC `Update`（新 config）→ `ServiceUpdate`（带 `version`=上次 inspect 的 `Version.Index`，乐观并发）→ swarm 按 `UpdateConfig` 滚动。`failureAction=rollback` 时失败自动回滚；也可主动调 `Rollback`。

**镜像拉取**：`imagePullPolicy=always` 在 create/update 前用 `cli.ImageList`+`cli.ImagePull` 预拉取（带 `RegistryAuth`），避免 task 在节点 `preparing` 阶段因拉取慢被误判超时。私有仓库凭证走 `registryAuth.secretRef`（swarm secret 存 `~/.docker/config.json`）。

### 4.5 编排事件流（给 OpsGaurdWeb / Agent）

每次 create/update/scale/restart 产生一条 `Operation` 记录：`{operationId, type, service, status: pending|healthy|failed, startedAt, finishedAt, steps:[], error}`。通过：
- gRPC `GetOperation(operationId)` 查询；
- MCP 工具 `get_operation`；
- 状态变更同时写 `EventStore`，经 `SubscribeEvents` 双向流回推管理端（取代旧的 webhook 推送）。

### 4.6 多副本语义与边界

多副本是 Swarm 的核心能力，但"多副本"在调度、网络、就绪、监控、缩容、部分失败这些维度的语义有差异。Worker 须明确边界，否则管理端易误判（如把滚动期"新旧共存"当故障，或把 host 模式下"该节点本无副本"当端口不通）。

#### 4.6.1 副本模式

| 模式 | 语义 | 缩放 |
|---|---|---|
| `replicated` | 期望副本数 == `replicas`，swarm 在满足约束的节点上调度 N 个 task | `scale_service` 可调 `replicas` |
| `global` | 每个可用节点一个 task，副本数 == 节点数 | 不可 `scale`，`replicas` 字段忽略 |

config `mode` 二选一。`global` 适合"每节点常驻 agent"类服务；`replicated` 适合应用服务。

#### 4.6.2 调度与分布

- **调度依据**：`placement.constraints`（节点标签硬约束）、`resources.reservations`（预留 CPU/内存，不足则 `pending`）、`placement.preferences`（spread 软偏好，跨可用区打散）。
- **单节点多副本**：默认允许同一节点跑多个副本（受资源预留限制）；若要"一节点一副本"用 `constraints: ["node.labels.unique==..."]` 或 `global` 模式。
- **节点 drain**：`docker node update --availability drain` 后该节点 task 被重调度到其他可用节点，副本数不变（期望状态收敛）。Worker 监测到 task 迁移产 `task_migrated` 事件。

#### 4.6.3 网络模式与端口语义（影响监控探活口径）

| 端口 `mode` | 行为 | portCheck / httpCheck 口径 |
|---|---|---|
| `ingress`（默认） | routing mesh：任意节点 IP + PublishedPort → 自动 LB 到所有副本 | 一次探活覆盖整个服务；探的是 LB 后端，某副本宕机被自动剔除（剔除前后短时可能超时） |
| `host` | PublishedPort 仅在运行 task 的节点上监听，无 LB | 需遍历运行 task 的节点 IP 逐副本探活；某节点无 task 则该端口"不通"是正常而非故障 |

- 未指定 `PublishedPort` 时 swarm 自动分配 `30000–32767`。
- `host` 模式下"端口不通"必须用 task→node 映射区分"该节点本无副本"与"副本异常"。
- 多副本 + `host`：同一 `PublishedPort` 不能在多节点冲突，通常 `mode=host` 的服务用 `global` 或每节点一副本。

#### 4.6.4 就绪与健康判定

| 场景 | 服务级状态 | 说明 |
|---|---|---|
| `running == replicas` 且全 healthy | `healthy` | 部署成功 |
| `0 < running < replicas` | `partial` | 非全就绪；产 `service_partial`，管理端决定是否告警 |
| `running == 0` 且 `desired == replicas` | `unhealthy`/`deploy_failed` | 严重告警 |
| 单副本 healthcheck unhealthy | — | swarm 自动重建替代 task（容器级自愈），不直接拉低服务级状态除非持续失败 |

- 默认**全副本就绪才算部署成功**；可选 `minReadyReplicas`（v2 扩展）容忍滚动期短暂部分不就绪，v1 不实现。
- healthcheck 配 `ContainerSpec.Healthcheck`，单副本连续 `retries` 次失败 → unhealthy → swarm 重建。

#### 4.6.5 滚动更新与回滚

- `update.parallelism`：同时更新的副本数（默认 1）。
- `update.delay`：每批间隔。
- `update.failureAction`：`pause`（停更，留新旧混合）/ `continue`（继续）/ `rollback`（回滚到上一版本）。
- `update.maxFailureRatio`：容忍失败率，超阈值触发 `failureAction`。
- **滚动期 service 处于"新旧版本共存"状态**，Worker 标注 `updating`，监控不得误判为故障。
- 回滚依赖 `failureAction=rollback` 自动触发（主动 `Rollback` RPC 未实现，可以旧 config 重新 `Update` 达成）。

#### 4.6.6 监控聚合口径（多副本下）

| 监控项 | 单副本口径 | 多副本聚合 |
|---|---|---|
| portCheck（ingress） | 单次探活 | 同（routing mesh 已聚合到副本集） |
| portCheck（host） | 该节点端口 | 遍历 task 节点 IP，全部通才算通；部分不通标注哪个副本 |
| httpCheck | 单次请求 | ingress 同 portCheck 口径；host 需逐副本 |
| logCheck | 单流 | `ServiceLogs` 聚合所有 task 流，按行标注来源 task/节点 |
| resourceCheck | 单容器 stats | per-container 采集，按 service 聚合：`max`（峰值告警）/ `p95`（稳态展示）/ `sum`（总量）。单副本超阈值产 `replica_over`，聚合超阈值产 `service_over` |

- 资源聚合策略在 config 可配（默认 `max` 用于告警、`p95` 用于展示）。

#### 4.6.7 缩容边界

- `scale_service(replicas=N)`：`N>0` 正常缩放；`N==0` = 停止所有副本（**高危，需 elicitation 确认**）；`N<0` 非法。
- replicated 模式下多副本可叠在同一节点（受资源限制），不会被"节点数"卡住；global 模式副本数恒=节点数，`scale` 无效。
- 缩容产 `task_shutdown` 事件（被缩掉的副本）。

#### 4.6.8 部分失败语义

| 场景 | 服务级状态 | 事件 |
|---|---|---|
| `running < desired` 但 `>0` | `partial` | `service_partial`（告警） |
| `running == 0` | `unhealthy` | `service_down`（严重告警） |
| 单副本 healthcheck unhealthy | — | `replica_unhealthy` → `task_recreated` |
| 单副本 OOM/崩溃 | 由 `restartPolicy` 决定 | `replica_failed`；超 `maxAttempts` 触发 `service_degraded` |

#### 4.6.9 pending 诊断

task 长期 `pending`（`State` 停在 `pending`/`allocated`）多半是非应用层原因，Worker 产 `service_pending` 事件并附诊断：
- 节点全 `drain`/`pause`（无可用节点）；
- `resources.reservations` 无节点能满足（预留内存/CPU 不足）；
- `placement.constraints` 无匹配节点；
- 镜像拉取失败（`preparing` 阶段卡住，取 `Task.Status.Err`）。

### 4.7 命令执行入口与安全策略（agent config 下发）

Worker 提供 **SSH 类命令执行入口**：容器内（`exec_in_container`）与宿主机（`exec_host_command`）。命令策略（黑白名单/超时/开关）由 **agent config** 下发（区别于编排用的 service config），每节点挂载 `/etc/opsguard/agent-config.yaml`（模板 `deploy/agent-config.yaml.example`）：

```yaml
worker:
  role: auto            # auto | manager | node（auto 按 swarm 节点类型推导）
  listen: ":8080"       # HTTP：/mcp + /healthz + /api/v1/local/*
  grpcListen: ":9080"   # gRPC：完整 ManagementService（双向流事件/审计回推）
  dataDir: "/var/lib/opsguard"  # SQLite 队列落盘目录（events.db / audit.db）
commandPolicy:
  mode: blacklist       # blacklist | whitelist
  allowHostExec: false  # 宿主机命令执行开关，默认关（需显式开启）
  allowContainerExec: true
  timeout: 30s
  blacklist:            # blacklist 模式：命中即拒（与内置 21 条危险命令合并）
    - "curl http://|sh"
  # whitelist:          # whitelist 模式：仅允许前缀命中 + 禁止 shell 链（; && | $( ）绕过
# webhooks:             # 已弃用/忽略（v0.2 起事件改由 gRPC SubscribeEvents 流回推）
```

**策略语义**：
- **blacklist 模式**（默认）：内置危险命令（`rm -rf /`、`shutdown`、`reboot`、`mkfs`、`dd of=/dev/sd*`、fork bomb 等）+ 用户追加子串，`strings.Contains` 命中即拒。
- **whitelist 模式**：命令须以白名单前缀开头（如 `cat`/`ls`/`ps`/`df`/`docker ps`），且**禁止 shell 链接符**（`;` `&&` `||` `|` 反引号 `$(`），防 `cat x; rm -rf /` 绕过。
- **超时**：单条命令默认 30s，超时终止。
- **执行路径**：
  - 容器内：Engine API `POST /containers/{id}/exec`（经任务所在节点的 worker）；
  - 宿主机：`nsenter -t 1 -m -u -i -n -p` 进宿主机 PID1 命名空间执行（完整宿主机环境）。注：swarm 部署会忽略 `privileged`/`pid: host`（见 stack.yml 注记），需要完整宿主机可见时用裸 `docker run`。
- **确认守卫**：MCP 层 `exec_host_command`/`exec_in_container` 与 `remove_service`/`scale=0` 一样需 `confirm=true`。

**真机验证（双节点集群）**：`exec_host_command hostname` 在两台宿主机返回各自主机名；`exec_host_command "shutdown -h now"` 被黑名单拦截（`matches blacklist pattern "shutdown"`）；`exec_in_container` 跨节点路由到任务所在节点执行成功。

---

## 五、模块②：监控体系

监控完全由 `monitoring` 配置驱动，与 service 一同下发、一同生效。四类 checker：

### 5.1 端口连通性（portCheck）

- **方式**：`net.DialTimeout("tcp", nodeIP:publishedPort, timeout)`。
- **节点选择**：manager-role Worker 通过 routing mesh 对**任意节点 IP + PublishedPort** 探活（ingress 模式保证可达）；`mode=host` 则需遍历运行该 task 的节点 IP。
- **判定**：连续 `retries` 次失败 → `port_down` 事件。
- **频率**：`interval`，默认 10s。

### 5.2 接口异常（httpCheck）

- **方式**：`http.Client` 发 `method`（默认 GET），校验 `expectedStatus`（数组）与 `expectedBody`（正则，空则不校验）。
- **判定**：状态码不在期望集 / 正则不匹配 / 超时 → `http_unhealthy` 事件。
- **与 healthcheck 的关系**：Docker `HEALTHCHECK` 是容器内自检；httpCheck 是平台外探活，二者互补（容器自报 healthy 但 ingress 不通时由 httpCheck 兜底）。

### 5.3 日志异常（logCheck）

- **方式**：manager-role Worker 调 `cli.ServiceLogs(ctx, id, LogsOptions{Follow:true, Tail:"0", Timestamps:true})` 获取流式日志（`io.ReadCloser`）；worker-role 可用本地 task logs。
- **限制**：仅 `json-file`/`journald` 驱动支持；`logDriver` 非此二者时 logCheck 自动降级为"禁用 + 告警提示改驱动"。
- **匹配**：`pattern` 为 Go 正则；`ignore` 为排除正则数组；`level` 标注事件级别。
- **action**：`alert`（写事件）/ `restart`（触发 gRPC `ManagementService.Restart` 强制重调度）。
- **背压**：日志流断开自动重连（`since=lastTimestamp`）；匹配命中后做 `time.Now()` 去抖，避免日志洪流淹没事件库。

### 5.4 资源使用（resourceCheck）

- **采集**：worker-role Worker 对本节点 task 对应容器调 `cli.ContainerStats(ctx, cid, false)`（one-shot，`stream=false`）。
- **指标**：
  - CPU% = Δ`cpu_usage.total_usage` / (Δ`system_cpu_usage` × `online_cpus`) × 100
  - Mem% = `memory_stats.usage` / `memory_stats.limit` × 100（注意扣 `inactive_file`，cgroup v2 用此字段）
  - Net I/O、Block I/O（参考 `docker stats`）
- **阈值**：`resourceThresholds` 按 `metric`(cpu|memory) + `threshold`(百分比) 触发 `resource_over` 事件；恢复触发 `resource_recovered`。
- **聚合**：manager-role Worker 汇聚各节点上报，按 service 聚合（取 max / p95）。

### 5.5 事件存储与外发

- `EventStore`：**SQLite 持久化队列**（`modernc.org/sqlite`，纯 Go 无 CGO；库文件 `dataDir` 下 `events.db`）。每条事件分配单调递增的 `seq`。事件结构：`{seq, id, ts, service, type, level, msg, detail, labels}`。核心方法：
  - `Add(ev)`：落盘并分配 `seq`，通过 `sync.Cond` 唤醒所有 `WaitNew` 阻塞者；
  - `Since(seq)`：读取大于该 `seq` 的事件（供流回推与点查）；
  - `Ack(seq)`：记录管理端已确认的最大 `seq`；
  - `GC(maxAge)`：清理已被 ack 且超龄的条目（由 `SubscribeEvents` 收到 ack 后触发）；
  - `WaitNew(ctx, lastSeen)`：阻塞至有 `> lastSeen` 的新事件或 ctx 取消（被 `Append` 的 cond 唤醒），供双向流长连接低开销等待。
- 审计存储 `audit.Store` 同构（SQLite 队列，库文件 `audit.db`），方法集相同。
- 与旧实现对比：旧版为内存 ring buffer（默认 10000 条）+ 可选 JSON Lines 落盘，**重启即丢、风暴超容量即驱逐**；新版落 SQLite，**重启不丢、上限=磁盘**，且 `seq`/`Ack`/`GC` 语义天然支撑 gRPC 双向流回推。旧 `EventStore.AddSink` 接口已移除。
- 外发：
  - **gRPC 双向流**：管理端在 Worker 的 `:9080` 上发起 `SubscribeEvents`（事件）/ `SubscribeAudit`（审计）双向流；Worker 把 `Since(lastSeen)` 的事件/审计逐条推回，管理端每收到一条回 `Ack(seq)`，Worker 据此 `GC`。**由 server 发起连接**，契合网络策略（仅 server→worker 可达），Worker 无需主动外联管理端。
  - **MCP 订阅**（若启用）：`subscriptions/listen` 的 SSE 流，按 `toolsListChanged`/`resourcesListChanged` 模式通知订阅方拉取新事件。
  - **webhook（已弃用）**：旧的 `WebhookPusher`（`internal/monitor/webhook.go`，POST 到管理端 `/api/v1/ingest/events`，fire-and-forget + 退避重试）已移除；agent config 中的 `webhooks:` 字段保留兼容但被忽略。

> **P2 实现说明（已落地，`internal/monitor/` + `internal/grpcapi/`）**：
> - **EventStore**：SQLite 持久化队列（`modernc.org/sqlite`），线程安全；点查走 gRPC `ListEvents?service=&type=&limit=`（新→旧）。
> - **四类探针**由 `monitor.Manager` 统一调度，按服务注册/注销（实现 `orchestrator.MonitorRegistrar`，Deploy/Update 自动注册、Remove 自动注销）。
> - **portCheck**：解析服务 task 所在节点 IP，对 `node:port` 做 TCP 探活（ingress 模式任一节点可达）；`retries` 次连续失败才产 `port_down`。
> - **httpCheck**：校验状态码（`expectedStatus`）与正则 body（`expectedBody`）。**URL 为 localhost/127.0.0.1 时自动改写为 task 节点 IP**（Worker 容器内 localhost 指向自身，否则误报）。
> - **logCheck**：`ServiceLogs(follow)` 流 + Docker 日志帧解码（8 字节头 + uint32 BE 长度），正则匹配 + ignore 排除 + 10s 去抖；断流自动重连（指数退避）。
> - **resCheck**：容器 stats 单次快照；**CPU% 用两次快照差值计算**（`docker stats` 算法），否则累计计数只能得"终身均值≈0"；内存按 `usage-inactive_file` 口径。多副本按均值聚合。
> - **已知限制（v1）**：资源按均值聚合（非 max/p95）；服务级探活默认 `retries=2` 固定（httpCheck）；`action=restart` 联动已接（log/resource 阈值可触发 gRPC `Restart`）但未在 e2e 验证；worker 重启后不会自动恢复既有服务的监控注册（需重新 Deploy/Update）；事件/审计在重启后保留（SQLite），管理端游标已持久化（LevelDB cursor，先 PutCursor 再 Ack），重连从最后游标续传。

### 5.6 监控与编排的联动

`logCheck.action=restart` 或 httpCheck 连续失败 `N` 次 → 自动触发 gRPC `ManagementService.Restart`（`--force` 重调度）。阈值与联动规则在 config 中声明，避免硬编码。

---

## 六、模块③：MCP Server（供 Agent 调用）

按 MCP `2026-07-28` 规范实现，使用官方 Go SDK。**目标版本：`2026-07-28`**。

### 6.1 传输与授权

| 场景 | 传输 | 授权 |
|---|---|---|
| Agent 远程调用（生产） | **Streamable HTTP**（单 endpoint，如 `https://worker/mcp`，仅 POST） | **OAuth 2.1**（PKCE + `resource` 参数 + RFC9728 Protected Resource Metadata + RFC9207 `iss` 校验） |
| 本地调试 / 同机 Agent | **stdio**（换行分隔 JSON-RPC） | 从环境取凭证（规范：stdio 不应走 OAuth） |

- **无状态**：不实现 `initialize` 握手、不维护 `Mcp-Session-Id`；每请求在 `_meta` 携带 `protocolVersion`（`"2026-07-28"`）与 `clientCapabilities`。
- **能力校验**：写操作工具要求 client 声明 `clientCapabilities` 含特定标志，否则返回 `-32021 MissingRequiredClientCapability`。
- **版本协商**：请求版本不支持时返回 `-32022 UnsupportedProtocolVersion` + `supportedVersions` 列表。
- **服务端发现**：实现 `server/discover`，返回 `supportedVersions`/`capabilities`/`serverInfo`。
- **变更订阅**：实现 `subscriptions/listen`，供 Agent 订阅服务列表变更/事件流。

> 兼容旧版客户端（2025-11-25 及更早）：按规范"现代请求→遇 400/404/405 且响应体非现代 JSON-RPC 错误→回退 initialize"实现探测（仅 HTTP）。

### 6.2 原语取舍

| 原语 | 采用 | 用途 |
|---|---|---|
| **Tools** | ✅ | Agent 调用的编排/监控操作（主体） |
| **Resources** | ✅ | 以 URI 暴露只读视图：`worker://services/{name}`、`worker://services/{name}/logs`、`worker://nodes/{id}`、`worker://events` |
| **Prompts** | ✅（可选） | 预置运维 playbook 模板（如"故障排查一键启动"） |
| **Elicitation** | ✅ | 需用户确认的高危操作（删服务/缩容到 0）经 MRTR 征求 |
| ~~Roots~~ | ❌ | 2026-07-28 已弃用 |
| ~~Sampling~~ | ❌ | 已弃用；如需 LLM 调用直接对接模型 API |
| ~~Logging~~ | ❌ | 已弃用；改用 `log/slog` → stderr / OpenTelemetry |

### 6.3 Tools 清单

每个工具均提供 `inputSchema`（JSON Schema 2020-12）与可选 `outputSchema`（`structuredContent` 校验），结果带 `resultType`。

| 工具 | 入参 | 出参 | 权限 |
|---|---|---|---|
| `list_services` | `?label`,`?status` | `services[]`（name, mode, replicas, status, health） | 读 |
| `get_service` | `name` | service 详情 + tasks + health + endpoint | 读 |
| `deploy_service` | `config`（完整 config） | `operationId` + 初始状态 | 写（需 elicitation 确认） |
| `update_service` | `name`,`config` | `operationId` | 写 |
| `scale_service` | `name`,`replicas` | `operationId` | 写（replicas=0 需确认） |
| `restart_service` | `name` | `operationId` | 写 |
| `remove_service` | `name` | `result` | 写（**必须 elicitation 确认**） |
| `get_service_logs` | `name`,`?tail`,`?since`,`?follow` | 日志行/订阅 | 读 |
| `get_resource_usage` | `name` | **跨节点** per-container + 聚合 CPU/Mem | 读 |
| `get_operation` | `operationId` | operation 状态 | 读 |
| `list_nodes` / `get_node` | `?id` | 节点状态/角色/可达性 | 读 |
| `get_self` | — | 本 worker 节点角色（nodeId/role/leader/swarmManager） | 读 |
| `get_events` | `?service`,`?type`,`?since`,`?limit` | 事件列表 | 读 |
| `exec_in_container` | `service`,`command[]`,`?slot`,`confirm` | exitCode + output（**跨节点路由**到任务所在节点） | 写（**confirm 确认**） |
| `exec_host_command` | `command`,`?node`,`confirm` | per-node exitCode + output（nsenter，**跨节点**，默认全部节点） | 写（**confirm 确认** + 黑白名单） |

**Tool 定义示例**（遵循规范字段）：
```json
{
  "name": "scale_service",
  "title": "Scale a Swarm service",
  "description": "Scale a service to the given replica count. replicas=0 will stop the service and requires user confirmation.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "name":     { "type": "string" },
      "replicas": { "type": "integer", "minimum": 0 }
    },
    "required": ["name", "replicas"]
  },
  "outputSchema": {
    "type": "object",
    "properties": {
      "operationId": { "type": "string" },
      "status":       { "type": "string", "enum": ["pending","healthy","failed"] }
    },
    "required": ["operationId", "status"]
  }
}
```

**危险操作经 Elicitation（MRTR）确认**：`remove_service` / `scale_service(replicas=0)` 执行中返回 `resultType:"input_required"` + `inputRequests:[{elicitation/create, form, requestedSchema:{...}}]`；Agent 侧用户 `accept` 后客户端带 `inputResponses`+新 `id` 重试，Worker 校验确认值后落地。

### 6.4 Resources（只读视图）

- `worker://services` — 服务清单
- `worker://services/{name}` — 服务详情（含 config 回显、当前 spec、tasks）
- `worker://services/{name}/logs?tail=200` — 最近日志
- `worker://nodes` / `worker://nodes/{id}` — 节点
- `worker://events?since=...&type=...` — 事件流

通过 `resources/list`、`resources/read` 访问；变更走 `subscriptions/listen`。

> **P3 实现说明（已落地，`internal/mcp/`，基于 go-sdk v1.7.0）**：
> - **传输**：Streamable HTTP（单 endpoint `POST /mcp`，**stateless**，`StreamableHTTPOptions.Stateless=true`，2026-07-28 无状态协议必需；真机发现并修复）；`-mcp-stdio` 模式走 stdio（newline-delimited JSON-RPC 自定义 transport，与 SDK custom-transport 同款）。
> - **Server**：`mcp.NewServer` + 泛型 `mcp.AddTool[In,Out]`（自动生成 input/output JSON Schema 2020-12、入参校验、`structuredContent` 输出）。
> - **Tools（20 个，已注册）**：编排类 list/get/deploy/update/scale/restart/remove_service、get_service_logs、get_events、get_operation、list/get_node、get_self；命令执行类 **exec_in_container**（跨节点路由）、**exec_host_command**（nsenter 宿主机，跨节点，黑白名单）；指标类 **get_resource_usage**（跨节点聚合）；节点探测类 **check_port**（任意 host:port TCP）、**check_http**（任意 URL，状态码+body 正则）、**list_host_processes**（宿主机进程发现，名称/cmdline 子串过滤）、**check_flow**（多步 HTTP 事务拨测）——探测类只读、无需 confirm，node 参数空=全部 ready 节点扇出（单点失败降级为结果条目），供排查 LLM 检查宿主机中间件/Java 进程。危险操作（remove/scale=0/两类 exec）要求 `confirm=true`，否则返回 `isError` 工具错误（e2e 已验证，含 `rm -rf /` 被黑名单拦截）。
> - **Resources（3 个）**：`worker://services`、`worker://services/{name}`（template）、`worker://events`。
> - **协议能力**：go-sdk 自动实现 `server/discover`、版本协商（2026-07-28/2025-11-25）、每请求 `_meta` 能力声明；客户端 e2e 确认 `InitializeResult().ProtocolVersion=2026-07-28`。2026-07-28 要求请求体 `_meta` 携带 `io.modelcontextprotocol/protocolVersion`（手写 JSON-RPC 需显式带，SDK 客户端自动带）。
> - **跨节点**：manager 按任务所在节点路由到 node worker 的 `/api/v1/local/*`，聚合 stats、代理 exec、广播 host 命令（真机双节点验证）。
> - **未做（记入风险）**：`subscriptions/listen` 长连接订阅（go-sdk 已内置能力，未启用）；Elicitation MRTR 用户确认（以 confirm 参数代替）；OAuth 2.1 完整授权码流（当前为 **bearer-token 鉴权**，`internal/authz`，已含 RFC 9728 Protected Resource Metadata 发现端点，见 §七）。

---

## 七、安全设计

1. **Docker socket**：Worker 挂载 `/var/run/docker.sock`，等同 root，必须：
   - Worker 容器以只读 root 挂载 + `--cap-drop=ALL` + 仅 `SETFCAP` 等最小能力；
   - 宿主机仅 root + docker 组可访问 socket；
   - 生产建议改 `tcp://manager:2376` + TLS 双向认证，避免每节点暴露 socket。
2. **宿主机命令执行（nsenter）**：`exec_host_command` 经 nsenter 进宿主机 PID1 命名空间执行。注：swarm 部署会忽略 `privileged`/`pid: host`（`deploy/stack.yml` 注记），完整宿主机可见需裸 `docker run`。这是整个系统攻击面最大的一环，安全基线：
   - 默认 `allowHostExec=false`（需 config 显式开启，见 `deploy/agent-config.yaml.example`）；
   - 命令黑白名单由 agent config 下发（见 §4.7）；
   - 高风险动作（`exec_host_command`/`exec_in_container`/`remove_service`/`scale=0`）在 MCP 层强制 `confirm=true`；
   - 生产应将该 API 置于仅内网/带鉴权的反向代理后，避免未授权访问特权 Worker。
3. **mTLS 双向认证**（管理端↔Worker）：`-tls-cert/-tls-key/-tls-ca` 三参齐备时强制客户端证书（`RequireAndVerifyClientCert`，TLS 1.2+）。`ca.pem`/`cert.pem`/`key.pem` 权限 `0444`/`0400`，证书 `extKeyUsage` 区分 `serverAuth`/`clientAuth`，`subjectAltName` 含所有节点。与 Bearer token 可叠加（证书鉴身份 + token 鉴权限）。
4. **Swarm 端口**：2377/TCP（manager 间）、7946/TCP+UDP（节点发现）、4789/UDP（VXLAN，仅可信网络，必要时 `--opt encrypted` 启用 IPsec ESP）。daemon 远程 API 走 2376/TLS，**禁用 2375 明文**。
5. **autolock**：`docker swarm update --autolock=true` 保护 Raft 密钥，manager 重启需 `swarm unlock`，防密钥落盘泄露。
6. **API/MCP 鉴权（Bearer Token）**：`auth.enabled` + `auth.tokens`（name→secret，"name@客户端IP" 作审计 actor）；HTTP 中间件（`internal/authz/authz.go`）用常量时间比较校验 `Authorization: Bearer`，包住 `/api/v1/local/*`（可执行宿主机命令）与 `/mcp`，auth 关闭时仍注入客户端 IP 作 actor（保证审计可溯源）；gRPC 侧由拦截器（`internal/authz/grpc.go`，unary + stream）从 metadata 取 `authorization: bearer` 同样校验，与 HTTP **共用同一 token 集**，包住 `:9080` 的 `ManagementService` 全部 RPC；`/.well-known/oauth-protected-resource`（RFC 9728）与 `/healthz` 公开。生产通过 `WORKER_TOKENS` env 集中分发 token。**完整 OAuth 2.1 授权码流（PKCE + AS）**：`/.well-known/oauth-protected-resource` 的 `authorization_servers` 字段现由 OpsGaurd IdP 填充（经 `OPSGUARD_IDP_ISSUER` env），MCP 客户端据此发现授权服务器走标准 PKCE 流；静态 bearer token 仍保留兜底，可平滑迁移。对接详见 `docs/IdP-接入指南.md`。
7. **配置脱敏**：config 中的 `env`/`secrets` 值、`registryAuth` 凭证不得进日志/事件；`slog` 统一脱敏过滤器（`internal/logging`）。
8. **审计日志**：所有编排写操作（deploy/update/scale/restart/remove）与命令执行（exec_in_container/exec_host_command）记入 `internal/audit`（**SQLite 持久化队列**，`audit.db`），含 actor（HTTP/MCP 为 "token名@客户端IP"，auth 关闭时为客户端 IP，stdio 会话为 "stdio"）、操作、目标、结果；gRPC `ListAudit` 点查、`SubscribeAudit` 双向流回推（管理端 ack seq 后 GC）。审计与业务日志相互独立。审计 actor 在 leader 写转发场景保持一致——非 leader 转发写 RPC 时携带原 bearer token，leader 据此记录原始调用方。

---

## 八、目录结构与模块划分

```
Worker/
├── cmd/worker/main.go                 # 入口：agent 配置 + 角色判断 + gRPC(管理)+HTTP(/mcp+/healthz+nodeagent)+MCP+Monitor
├── cmd/genpatch/                      # 离线 proto pb 补丁工具（无 protoc 时 gen.sh 降级调用）
├── internal/
│   ├── config/        config.go, validate.go, defaults.go, quantity.go, load.go（服务 config）
│   ├── agent/         agent.go, policy.go, hostexec.go（worker 自身 config：命令黑白名单/超时/开关）
│   ├── docker/        client.go, service.go, task.go, node.go, stats.go, logs.go,
│   │                  exec.go, image.go, transport.go, types.go, logdecoder.go
│   ├── orchestrator/  translator.go, lifecycle.go, operation.go, proxy.go（跨节点代理；管理 HTTP handler 已移除）
│   ├── grpcapi/       server.go, stream.go, tunnel.go（ManagementService + 双向流 + 反向隧道池）
│   │   └── pb/        protoc-gen-go 生成代码
│   ├── authz/         authz.go（HTTP bearer 中间件）, grpc.go（gRPC unary+stream 拦截器，同 token 集）
│   ├── monitor/       manager.go, portcheck.go, httpcheck.go, logcheck.go, rescheck.go, event.go（SQLite 队列）
│   ├── audit/         audit.go（SQLite 持久化审计队列：Add/Since/Ack/GC/WaitNew）
│   ├── nodeagent/     api.go, logs.go, util.go（每节点本地接口 /local/stats /local/exec /local/host /local/logs）
│   ├── idpproxy/      handler.go（/idp-proxy/ 经隧道访问管理端 IdP）
│   ├── registryproxy/ handler.go, cache.go（/v2/ 镜像中继 + blob LRU 缓存）
│   ├── mcp/           server.go, tools.go, resources.go, metrics.go, checks.go（exec/拨测/跨节点聚合）
│   ├── logging/       redact.go（slog 脱敏）
│   └── version/       version.go
├── proto/
│   └── opsguard.proto                  # gRPC 契约（ManagementService；仓库根）
├── deploy/
│   ├── Dockerfile
│   ├── stack.yml                      # global 部署（swarm 忽略 privileged/pid:host，宿主可见需裸 docker run）；暴露 :8080(HTTP) + :9080(gRPC)
│   └── agent-config.yaml.example      # 命令策略配置模板（黑白名单；含 listen/grpcListen/dataDir）
├── docs/                              # 已存在（本文件所在）
├── go.mod / go.sum
└── README.md
```

> **已移除文件**：`internal/orchestrator/api.go`（管理 HTTP handler）、`internal/monitor/webhook.go`（WebhookPusher）、`orchestrator/proxy.go` 中的 `ProxyWriteToLeader`（HTTP 写转发，改由 gRPC 内部客户端）。

---

## 九、分期实施计划

| 阶段 | 里程碑 | 交付物 | 状态 |
|---|---|---|---|
| **P0 骨架** | 项目脚手架 + Docker 客户端封装 + config schema | `go.mod`、`internal/docker/*`、`internal/config/*`、单测 | ✅ |
| **P1 编排** | config→spec 映射 + 生命周期轮询 + registry 鉴权/预拉取（原 HTTP POST API 在 P6 改为 gRPC） | `internal/orchestrator/*`、本地集成测试（swarm） | ✅ |
| **P2 监控** | 四类 checker + EventStore + 事件查询 + 探活修复（原内存 ring buffer 在 P6 改为 SQLite 队列） | `internal/monitor/*`、监控集成测试 | ✅ |
| **P3 MCP** | Tools/Resources + Streamable HTTP（go-sdk v1.7.0，协议 2026-07-28，stateless） | `internal/mcp/*`、MCP e2e（官方 SDK 客户端 + 真机双节点） | ✅ |
| **P4 HA** | 节点身份识别（/info NodeID）+ manager 写守卫 + `/self` + get_self + **per-node worker（global）** + **跨节点代理**（stats 聚合/exec 路由/host 广播） | `internal/nodeagent`、`orchestrator/proxy`、真机双节点验证 | ✅ |
| **P5 生产化** | 日志脱敏（`internal/logging`）+ TLS/（**mTLS 双向** `-tls-ca`）+ 部署 stack + README + **命令执行入口 + 黑白名单策略（agent config）** + **SSE 流式日志** + **鉴权（Bearer token + OAuth metadata）** + **审计日志** + **leader 写转发**（P6 起为 gRPC internal client）+ **env 集中分发** | `internal/agent`、`internal/authz`、`internal/audit`、`deploy/*`、真机验证 | ✅（OAuth 2.1 授权码流、autolock 未做，记入风险） |
| **P6 管理 API gRPC 化** | **管理端↔Worker 管理 API 从 HTTP 迁到 gRPC**：契约 `proto/opsguard.proto`（`ManagementService`）；事件/审计投递由 webhook 推送改为 **gRPC 双向流 `SubscribeEvents`/`SubscribeAudit`**（server 发起，顺网络策略方向）；`EventStore`/`audit.Store` 由内存 ring buffer 改为 **SQLite 持久化队列**（`modernc.org/sqlite`，`seq`/`Ack`/`GC`/`WaitNew`）；Worker **双端口**（`:8080` HTTP 仅 `/mcp`+`/healthz`+node 本地 API；`:9080` gRPC 全量 `ManagementService`）；鉴权由 HTTP 中间件扩展到 **gRPC 拦截器**（同 token 集）；删除 `internal/monitor/webhook.go`、`internal/orchestrator/api.go`、`ProxyWriteToLeader` 及 `/api/v1/events`/`/api/v1/audit`/`/api/v1/services*` HTTP 路由 | `internal/grpcapi`、`internal/grpcapi/pb`、`internal/authz/grpc.go`、`internal/audit`（SQLite）、`proto/opsguard.proto`、真机验证 | ✅ |

每个阶段配套：单元测试 + `docker testcontainers` 集成测试 + 文档更新。

**实现备注**：
- Docker 客户端为直接 HTTP（stdlib），非官方 SDK（模块结构损坏，见 §三）；MCP 用官方 go-sdk v1.7.0。
- P4 复用 swarm manager 的 Raft leader（`ControlAvailable`/`ManagerStatus.Leader`），未自研选举；每节点部署 node-role worker（global 服务），manager 经 `/api/v1/local/*` 跨节点代理，无需自研上报通道。
- P6 的 gRPC 双向流由 server 发起连接，契合网络策略（仅 server→worker 可达），Worker 无需主动外联管理端；事件用 SQLite 持久化队列，重启不丢、抗事件风暴，`seq`/`Ack`/`GC` 语义天然支撑流回推。
- 真机验证（双节点 swarm，服务器无外网，镜像离线传输）：跨节点多副本部署 healthy、任务分布两节点、routing mesh 跨节点 200、跨节点 stats 聚合（replicas=2）、宿主机命令执行（nsenter）+ 黑名单拦截、SSE 流式日志实时追加、gRPC 事件流回推 + ack + GC。

---

## 十、风险与待确认事项

1. **Docker Engine API 版本差异**：`WithAPIVersionNegotiation()` 自动协商，但 `StartInterval`（1.44+）、部分 log 字段依赖版本；translator 需做版本探测降级。
2. **日志驱动限制**：`ServiceLogs` 仅 `json-file`/`journald`；若集群用 `fluentd`/`gelf` 等远端驱动，logCheck 与流式日志接口不可用，需提示用户或改接集中日志（ELK）——v1 不做，记为已知限制。
3. **宿主机命令执行的安全暴露面**：`exec_host_command` 要求 worker 以 privileged + pid=host 运行（nsenter 进宿主机 PID1 命名空间），容器逃逸即宿主 root。缓解：`allowHostExec` 默认关、黑白名单、MCP `confirm`、生产置于仅内网/鉴权代理后；`/api/v1/local/*` 已由 bearer 中间件统一鉴权（PublicPaths 白名单除外，见 §七.6）。
4. **MCP 规范时效**：`2026-07-28` 为截至 2026-08-03 的最新版本；官方 SDK v1.7.0 已原生支持（stateless 需显式 `StreamableHTTPOptions.Stateless=true`，真机发现并修复）。后续版本升级需复核 SDK `CHANGELOG`。
5. **Leader 选举与写转发**：复用 swarm manager 的 Raft leader（`ManagerStatus.Leader`）而非自研选举，简化实现；多 manager 部署时仅 leader 执行写编排，其余 manager 实例上的写 RPC（Deploy/Update/Scale/Restart/Remove）经 **gRPC 内部客户端转发到 leader `:9080`**（携带原 bearer token，保持审计 actor 一致）。仍需处理 leader 切换时在途 operation 的接管（operation 状态落盘 + 新 leader 重放）与在途 gRPC 双向流的重连（管理端用 `lastSeen` 续传，重放未 ack 段）。
6. **config 兼容性**：config 是自定义 schema，未来若要兼容 Compose 文件或 K8s manifest，需在 `translator` 上加适配层，不污染核心模型。
7. **私有仓库凭证**：`registryAuth` 用 swarm secret 还是 `AuthConfig` 内联？默认 secret 引用（更安全），但 create 时需先 `secret create`；提供 `registryAuth.inline`（base64）作为便捷模式，标注不推荐用于生产。
8. **agent config 同步**：命令策略经每节点挂载的 `agent-config.yaml` 下发；配置变更需全节点重新挂载/重启 worker（尚无集中分发），生产可用配置中心或 swarm config 分发。
9. **跨节点 stats 采样时序**：`get_resource_usage` 的 CPU% 依赖两节点各自两次快照差值（1s 间隔），manager 聚合时各节点采样起点不一致，聚合值含 ±1s 偏差（读路径可接受）。
10. **SQLite 队列增长与重放**：事件/审计改 SQLite 持久化队列后，若管理端长期不连或未 ack，`Since(lastSeen)` 的未 GC 段会累积（上限=磁盘）。缓解：`GC(maxAge)` 只清理**已 ack 且超龄**的条目（未 ack 条目不清理、上限=磁盘；管理端游标已持久化，重连从游标续传）；生产应监控 `events.db`/`audit.db` 体积。`modernc.org/sqlite` 为纯 Go 无 CGO，跨平台编译友好，但高并发写性能低于 CGO 版 `mattn/go-sqlite3`——本场景写量来自事件探针，非热路径，可接受。
11. **gRPC 双向流与网络策略**：`SubscribeEvents`/`SubscribeAudit` 由 server 发起连接（仅 server→worker 可达的方向），Worker 侧不监听管理端可达性；连接断开后由 server 重建并续 `lastSeen`。若中间有 L4 代理/负载均衡，需确保长连接空闲不被过早回收（调大 idle timeout 或加心跳）。

---

## 附录 A：MCP 2026-07-28 关键要点

- **版本**：`2026-07-28`（最新，相对 `2025-11-25`）。
- **架构**：无状态；取消 `initialize` 握手与 `Mcp-Session-Id` 会话；每请求 `_meta` 携带 `protocolVersion`/`clientInfo`/`clientCapabilities`/`logLevel`。
- **传输**：标准仅 stdio + Streamable HTTP（单 POST endpoint，请求级 SSE 流式响应）；HTTP+SSE 已弃用；WebSocket 非标准（可作自定义传输）。
- **授权**：HTTP 配 OAuth 2.1（PKCE + `resource` 参数 + RFC9728 Protected Resource Metadata + RFC9207 `iss`）；stdio 从环境取凭证。
- **服务端发现**：`server/discover`（服务端必须实现，客户端可选）返回 `supportedVersions`/`capabilities`/`serverInfo`。
- **原语**：Tools/Resources/Prompts（Active）+ Elicitation（Active，MRTR 投递）；Roots/Sampling/Logging 已弃用（SEP-2577）。
- **MRTR**：服务端→客户端输入一律以 `InputRequiredResult`（`resultType:"input_required"`）返回，客户端带 `inputResponses`+新 `id` 重试。
- **订阅**：`subscriptions/listen` 单一长生命 SSE 流，通知带 `subscriptionId`。
- **结果契约**：所有 result 带 `resultType`（`"complete"`/`"input_required"`）。
- **错误码**：`-32020 HeaderMismatch`、`-32021 MissingRequiredClientCapability`、`-32022 UnsupportedProtocolVersion`。
- **SDK**：`github.com/modelcontextprotocol/go-sdk`（Tier-1，官方）。

## 附录 B：Docker Swarm 关键 API

- **服务**：`POST /services/create`、`POST /services/{id}/update`（带 `version` 乐观并发）、`DELETE /services/{id}`、`GET /services`、`GET /services/{id}`、`GET /services/{id}/logs`（follow/since/tail/timestamps/details，仅 json-file/journald）。
- **任务**：`GET /tasks`（filter `service`/`node`/`desired-state`）、`GET /tasks/{id}`；`Task.Status.State` 单调推进（new→…→running→complete，分支 rejected/failed/orphaned）。
- **节点**：`GET /nodes`、`GET /nodes/{id}`；健康判据：`Status.State==ready`，manager 额外 `ManagerStatus.Reachability==reachable`。
- **就绪**：`running` task 数 == `replicas` 且 `Task.Status.State==running` 且 `DesiredState==running`；配 healthcheck 时容器 `State.Health.Status==healthy`。
- **资源**：`TaskTemplate.Resources.Limits`/`Reservations`，`NanoCPUs`（CPU×1e9）、`MemoryBytes`（字节）；`GET /containers/{id}/stats?stream=false` 取 CPU/Mem/Net/Block。
- **端口探活**：swarm 无专用端口 API；取 `Endpoint.Ports` 后对节点 IP+PublishedPort 主动 TCP/HTTP 探活（routing mesh 保证任意节点可达）。
- **安全**：daemon socket 限本机或 2376/TLS 双向认证；swarm 端口 2377/TCP、7946/TCP+UDP、4789/UDP（VXLAN，限可信网络）；`autolock` 保护 Raft 密钥。
- **Go SDK**：`github.com/docker/docker/client`，`NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation(), client.WithTLSClientConfig(...))`；方法 `ServiceCreate/Update/Remove/List/InspectWithRaw/Logs`、`TaskList/InspectWithRaw/Logs`、`NodeList/InspectWithRaw/Update/Remove`、`ContainerStats`。
