# Worker 设计方案

> 版本：v0.1（草案）
> 日期：2026-08-03
> 状态：调研完成，待评审

## 一、背景与目标

OpsGaurd 定位为"智能运维全链路自动化平台"。`Worker` 是部署在被纳管 Docker 主机上的核心组件，承担三类职责：

| 模块 | 职责 | 对标 |
|---|---|---|
| ① 编排 | 提供 POST 接口，接收管理端下发的 `config`，自动拉取镜像并启动/更新/销毁容器 | 类 Rancher，但基于 **Docker Swarm** |
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
│  │  ├─ HTTP API (POST config)    │        │  ├─ HTTP API (只读/代理)       │  │
│  │  ├─ MCP Server (Stream HTTP)  │        │  └─ MCP Server (只读/代理)      │  │
│  │  └─ EventStore (内存+落盘)     │        │                               │  │
│  └───────────────────────────────┘        └───────────────────────────────┘  │
│                  ▲                                                             │
│                  │ docker.sock / 2376(TLS)                                      │
│                  ▼                                                             │
└─────────────────┬───────────────────────────────────────────────────────────┘
                  │
        ┌─────────┴─────────┐
        │  OpsGaurdWeb（管理端）│  ◀── 推送 config（POST） / 拉取事件
        └───────────────────┘
                  ▲
                  │ MCP（Streamable HTTP + OAuth2.1）
        ┌─────────┴─────────┐
        │  LLM Agent / Host  │
        └───────────────────┘
```

**角色判定**：Worker 启动时调用 `docker node inspect self`（通过 `node-self` 标签或本机 hostname 匹配 `NodeID`），读取 `Spec.Role`（`manager`/`worker`）与 `ManagerStatus.Leader`。

- **manager-role Worker**：独占编排能力；承担**服务级**监控（service logs、routing-mesh 端口/HTTP 探活）；聚合所有节点上报的本地指标；是事件总线中心。
- **worker-role Worker**：承担**本地**监控（本节点容器的 `stats`、本地 task 的日志 tail）；将本地指标/事件**上报**给 manager-role Worker（HTTP push，带退避重试）；其 HTTP API 与 MCP 工具中"写操作"**代理转发**到 leader manager。
- **Leader 选举**：swarm manager 集群本身有 Raft leader。manager-role Worker 中仅 `ManagerStatus.Leader==true` 者执行"写"编排与全局事件汇聚；其余 manager 实例做热备（API 代理到 leader）。

> **简化部署（小集群可选）**：仅在单台 manager 节点部署一个 Worker 实例，集中编排+监控+MCP，跳过 per-node agent。资源监控通过遍历 task→node 后远程 `tcp://node:2376`（TLS）拉 `stats`，节点数多时开销大，不推荐用于 >5 节点。

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
├── internal/orchestrator/  模块①：配置驱动编排
│   ├── api.go              POST/GET/DELETE HTTP handler（chi/gin）
│   ├── translator.go       Config → swarm.ServiceSpec 双向映射
│   ├── lifecycle.go        Create→轮询就绪→事件；Update→滚动；Rollback
│   └── registry.go         私有仓库认证（secret 引用）
├── internal/monitor/       模块②：监控
│   ├── manager.go          监控调度器（按 config 起停 checker）
│   ├── portcheck.go        TCP 连通性探针
│   ├── httpcheck.go        HTTP 健康探针（状态码/体匹配）
│   ├── logcheck.go         服务日志流 + 关键词/正则匹配
│   ├── rescheck.go         资源使用采集（阈值告警）
│   ├── reporter.go         本地→manager 上报（worker-role）
│   └── eventstore.go       事件存储（ring buffer + 落盘 JSON Lines）
├── internal/mcp/           模块③：MCP Server
│   ├── server.go           go-sdk 注册 tools/resources
│   ├── tools/              deploy/scale/logs/status/rollback/check…
│   ├── transport.go        Streamable HTTP（含 OAuth2.1）+ stdio（本地调试）
│   └── auth.go             OAuth2.1 资源服务器（PKCE + RFC9728）
├── internal/config/        Config schema + 校验（JSON Schema 2020-12）
├── internal/ha/           Leader 选举/代理（manager-role 热备）
├── cmd/worker/main.go      入口
└── deploy/                 Dockerfile、stack.yml、systemd unit
```

### 2.3 数据流

1. **部署流**：OpsGaurdWeb `POST /api/v1/services`（带 config）→ leader manager-role Worker → `translator` 转 `swarm.ServiceSpec` → `cli.ServiceCreate` → swarm 调度 task 到各节点 → 各节点 Worker 观察本地 task → 就绪/异常事件 → 上报 → manager 汇聚 → 回写部署结果。
2. **监控流**：各 checker 周期性探活/采集 → 命中阈值 → `EventStore` → 推送 OpsGaurdWeb（webhook）+ MCP `subscriptions/listen` 通知订阅方。
3. **Agent 流**：Agent `tools/call` → MCP Server → 调 orchestrator/monitor 能力 → 返回 `structuredContent`。

---

## 三、技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 语言 | **Go 1.22+** | .gitignore 已含 Go 模式；Docker/MCP 均 Tier-1 官方 Go SDK；单二进制便于全局服务部署 |
| Docker 客户端 | **直接 HTTP 调用 Engine REST API**（stdlib `net/http` + 自定义结构体） | 见下方说明 |
| HTTP 路由 | `github.com/go-chi/chi/v5` | 轻量、中间件友好、与 net/http 兼容 |
| MCP SDK | `github.com/modelcontextprotocol/go-sdk` v1.7.0 | 官方 Tier-1，**目标协议版本即 2026-07-28**（stateless + `server/discover` + MRTR + subscriptions + Streamable HTTP 全内置） |

> **实现期变更（P0）**：Docker 客户端从官方 Go SDK 改为**直接 HTTP 调用 Engine REST API**（stdlib `net/http` + 自定义请求/响应结构体）。原因：`github.com/docker/docker` SDK 的 Go 模块结构不稳定——新版把 `api`/`client` 拆成路径不匹配的嵌套模块（`github.com/moby/moby/api`），旧版（v24）又触发 `distribution/reference` 传递依赖损坏，跨版本都无法干净构建。直接走 REST API 零该依赖、二进制更小（10MB）、攻击面更少，且与本文档已枚举的 REST 端点一致。代价是自维护 Engine API JSON 结构体（仅子集，未知字段忽略）。transport 支持 `unix:///var/run/docker.sock`（默认/Linux）与 `tcp://host:2376`（+TLS）；Windows 本地开发用 Docker Desktop 的 `tcp://localhost:2375`（npipe 暂不支持，保持 stdlib-only）。
| 配置校验 | `github.com/santhosh-tekuri/jsonschema/v6` | JSON Schema 2020-12，与 MCP `inputSchema` 同源 |
| 日志 | `log/slog`（标准库结构化日志） | Go 1.21+ 内置，契合 MCP 弃用 `logging` 后走 stderr 的指引 |
| 探针 | `net.Dialer`（TCP）、`net/http`（HTTP）、`cli.ServiceLogs`（日志流） | 标准库 + Docker SDK |
| 存储 | 内存 ring buffer + JSON Lines 落盘 | 轻量；事件历史可后续接 OpsGaurdWeb DB |
| 部署 | Dockerfile + `deploy/stack.yml`（swarm stack） | Worker 自身也用 swarm 管理，自举 |

---

## 四、模块①：配置驱动的容器编排

### 4.1 POST API

管理端通过 HTTP 推送 config，Worker 调用 swarm API 落地。

| Method | Path | 说明 |
|---|---|---|
| `POST` | `/api/v1/services` | 创建服务（body=完整 config） |
| `POST` | `/api/v1/services/{name}` | 更新服务（body=新 config，全量替换；乐观并发用 `version`） |
| `DELETE` | `/api/v1/services/{name}` | 删除服务 |
| `GET` | `/api/v1/services` | 服务列表（支持 `?label=`、`?status=true`） |
| `GET` | `/api/v1/services/{name}` | 服务详情 + task 概览 + 健康 |
| `POST` | `/api/v1/services/{name}/scale` | 调整副本数 `{replicas:N}` |
| `POST` | `/api/v1/services/{name}/rollback` | 回滚到上一版本 |
| `POST` | `/api/v1/services/{name}/restart` | 强制重调度（`--force` 等价） |

**鉴权**：管理端→Worker 用 **mTLS**（Worker 侧加载 ca/cert/key，与 swarm manager 2376 同套体系）或共享 token；跨集群走 HTTPS。

**幂等**：`POST /services` 若同名已存在 → 返回 `409` 并提示用 `POST /services/{name}` 更新；`scale`/`rollback` 可重试。

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
POST /services  ──▶ ServiceCreate  ──▶ 返回 202 + serviceID + operationId
        │
        └─▶ 后台轮询（指数退避，上限 30s/次，总超时默认 5min）
              ├─ GET /services/{id}          → ServiceStatus.RunningTasks / DesiredTasks
              ├─ GET /tasks?service={id}     → 逐 task 检查
              └─ 就绪判据：
                    ① running task 数 == replicas（global：每节点一个 running）
                    ② 每个 task Status.State==running 且 DesiredState==running
                    ③ 容器 Health.Status==healthy（若配了 healthcheck）
              ├─ 命中就绪 → 写 EventStore(status=healthy) → 回写 OpsGaurdWeb
              └─ 超时/失败 → 写 Event(status=deploy_failed) + 错误摘要
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

**滚动更新**：`POST /services/{name}`（新 config）→ `ServiceUpdate`（带 `version`=上次 inspect 的 `Version.Index`，乐观并发）→ swarm 按 `UpdateConfig` 滚动。`failureAction=rollback` 时失败自动回滚；也可主动 `POST /rollback`。

**镜像拉取**：`imagePullPolicy=always` 在 create/update 前用 `cli.ImageList`+`cli.ImagePull` 预拉取（带 `RegistryAuth`），避免 task 在节点 `preparing` 阶段因拉取慢被误判超时。私有仓库凭证走 `registryAuth.secretRef`（swarm secret 存 `~/.docker/config.json`）。

### 4.5 编排事件流（给 OpsGaurdWeb / Agent）

每次 create/update/scale/rollback/force 产生一条 `Operation` 记录：`{operationId, type, service, status: pending|healthy|failed, startedAt, finishedAt, steps:[], error}`。通过：
- HTTP `GET /api/v1/operations/{id}` 查询；
- MCP 工具 `get_operation`；
- webhook 推送 OpsGaurdWeb。

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
- 主动回滚走 `POST /services/{name}/rollback`，或 `failureAction=rollback` 自动触发。

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
- **action**：`alert`（写事件）/ `restart`（触发 `POST /restart` 强制重调度）。
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

- `EventStore`：内存 ring buffer（默认 10000 条）+ 落盘 JSON Lines（`/var/lib/worker/events.jsonl`，轮转）。
- 事件结构：`{id, ts, service, type, level, msg, detail, labels}`。
- 外发：
  - **webhook**：POST 到 OpsGaurdWeb 配置的回调，带退避重试（指数，上限 5min）；
  - **MCP 订阅**：`subscriptions/listen` 的 SSE 流，按 `toolsListChanged`/`resourcesListChanged` 模式通知订阅方拉取新事件。

> **P2 实现说明（已落地，`internal/monitor/`）**：
> - **EventStore**：内存 ring buffer（默认 10000 条），线程安全，`GET /api/v1/events?service=&type=&limit=` 查询（新→旧）。
> - **四类探针**由 `monitor.Manager` 统一调度，按服务注册/注销（实现 `orchestrator.MonitorRegistrar`，Deploy/Update 自动注册、Remove 自动注销）。
> - **portCheck**：解析服务 task 所在节点 IP，对 `node:port` 做 TCP 探活（ingress 模式任一节点可达）；`retries` 次连续失败才产 `port_down`。
> - **httpCheck**：校验状态码（`expectedStatus`）与正则 body（`expectedBody`）。**URL 为 localhost/127.0.0.1 时自动改写为 task 节点 IP**（Worker 容器内 localhost 指向自身，否则误报）。
> - **logCheck**：`ServiceLogs(follow)` 流 + Docker 日志帧解码（8 字节头 + uint32 BE 长度），正则匹配 + ignore 排除 + 10s 去抖；断流自动重连（指数退避）。
> - **resCheck**：容器 stats 单次快照；**CPU% 用两次快照差值计算**（`docker stats` 算法），否则累计计数只能得"终身均值≈0"；内存按 `usage-inactive_file` 口径。多副本按均值聚合。
> - **已知限制（v1）**：资源按均值聚合（非 max/p95）；服务级探活默认 `retries=2` 固定（httpCheck）；`action=restart` 联动已接（log/resource 阈值可触发 `POST /restart`）但未在 e2e 验证；worker 重启后不会自动恢复既有服务的监控注册（需重新 Deploy/Update）。

### 5.6 监控与编排的联动

`logCheck.action=restart` 或 httpCheck 连续失败 `N` 次 → 自动触发 `POST /services/{name}/restart`（`--force` 重调度）。阈值与联动规则在 config 中声明，避免硬编码。

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
| `rollback_service` | `name` | `operationId` | 写 |
| `restart_service` | `name` | `operationId` | 写 |
| `remove_service` | `name` | `result` | 写（**必须 elicitation 确认**） |
| `get_service_logs` | `name`,`?tail`,`?since`,`?follow` | 日志行/订阅 | 读 |
| `check_port` | `name`,`port`（或自动取 service ports） | `reachable`, latency | 读 |
| `get_resource_usage` | `name`,`?metric`,`?percentile` | per-node + 聚合 CPU/Mem/Net | 读 |
| `get_operation` | `operationId` | operation 状态 | 读 |
| `list_nodes` / `get_node` | `?id` | 节点状态/角色/可达性 | 读 |
| `get_events` | `?service`,`?type`,`?since`,`?limit` | 事件列表 | 读 |

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
> - **传输**：Streamable HTTP（单 endpoint `POST /mcp`，stateless）；`-mcp-stdio` 模式走 stdio（newline-delimited JSON-RPC 自定义 transport，与 SDK custom-transport 同款）。
> - **Server**：`mcp.NewServer` + 泛型 `mcp.AddTool[In,Out]`（自动生成 input/output JSON Schema 2020-12、入参校验、`structuredContent` 输出）。
> - **Tools（12 个，已注册）**：list_services / get_service / deploy_service / update_service / scale_service / restart_service / remove_service / get_service_logs / get_events / get_operation / list_nodes / get_node。危险操作守卫：`remove_service` 与 `scale_service(replicas=0)` 要求 `confirm=true`，否则返回 `isError` 工具错误（e2e 已验证）。
> - **Resources（3 个）**：`worker://services`、`worker://services/{name}`（template）、`worker://events`。
> - **协议能力**：go-sdk 自动实现 `server/discover`、版本协商（2026-07-28/2025-11-25）、每请求 `_meta` 能力声明；客户端 e2e 确认 `InitializeResult().ProtocolVersion=2026-07-28`。
> - **已 e2e 验证**：discover → tools/list（12）→ resources（3）→ deploy_service → get_operation(healthy) → get_service → get_service_logs → get_events → remove_service(confirm)，以及 scale=0 无 confirm 的拒绝。
> - **未做（记入风险）**：OAuth 2.1（当前无鉴权，生产需在代理层加 mTLS/OAuth）；`subscriptions/listen` 长连接订阅（go-sdk 已内置能力，未启用）；Elicitation MRTR 用户确认（以 confirm 参数代替）。

---

## 七、安全设计

1. **Docker socket**：Worker 挂载 `/var/run/docker.sock`，等同 root，必须：
   - Worker 容器以只读 root 挂载 + `--cap-drop=ALL` + 仅 `SETFCAP` 等最小能力；
   - 宿主机仅 root + docker 组可访问 socket；
   - 生产建议改 `tcp://manager:2376` + TLS 双向认证，避免每节点暴露 socket。
2. **mTLS**（管理端↔Worker、Worker↔Docker daemon）：复用 swarm 2376 体系，`ca.pem`/`cert.pem`/`key.pem` 权限 `0444`/`0400`，证书 `extKeyUsage` 区分 `serverAuth`/`clientAuth`，`subjectAltName` 含所有节点。
3. **Swarm 端口**：2377/TCP（manager 间）、7946/TCP+UDP（节点发现）、4789/UDP（VXLAN，仅可信网络，必要时 `--opt encrypted` 启用 IPsec ESP）。daemon 远程 API 走 2376/TLS，**禁用 2375 明文**。
4. **autolock**：`docker swarm update --autolock=true` 保护 Raft 密钥，manager 重启需 `swarm unlock`，防密钥落盘泄露。
5. **MCP OAuth 2.1**：Worker 作 OAuth 2.1 资源服务器；token 走 `Authorization: Bearer`，按 RFC8707 校验 `resource` 受众；scope 细化 `worker:read` / `worker:write`。高危工具额外要 elicitation 确认。
6. **配置脱敏**：config 中的 `env`/`secrets` 值、`registryAuth` 凭证不得进日志/事件；`slog` 统一脱敏过滤器。

---

## 八、目录结构与模块划分

```
Worker/
├── cmd/worker/main.go                 # 入口：加载 config、启动 HTTP+MCP+Monitor
├── internal/
│   ├── config/        schema.go, validator.go, defaults.go
│   ├── docker/        client.go, service.go, task.go, node.go, stats.go, logs.go
│   ├── orchestrator/  api.go, translator.go, lifecycle.go, registry.go, operation.go
│   ├── monitor/       manager.go, portcheck.go, httpcheck.go, logcheck.go,
│   │                  rescheck.go, reporter.go, eventstore.go, webhook.go
│   ├── mcp/           server.go, transport.go, auth.go, subscriptions.go
│   │   └── tools/     list_services.go, deploy_service.go, scale_service.go,
│   │                  get_service.go, get_logs.go, check_port.go,
│   │                  get_resource_usage.go, get_events.go, ...
│   ├── ha/            leader.go, proxy.go
│   └── version/       version.go
├── deploy/
│   ├── Dockerfile
│   ├── stack.yml                      # Worker 自身用 swarm stack 部署（自举）
│   └── worker.service                 # 或 systemd unit（非 swarm 单机）
├── docs/                              # 已存在（本文件所在）
├── go.mod / go.sum
└── README.md
```

---

## 九、分期实施计划

| 阶段 | 里程碑 | 交付物 | 状态 |
|---|---|---|---|
| **P0 骨架** | 项目脚手架 + Docker 客户端封装 + config schema | `go.mod`、`internal/docker/*`、`internal/config/*`、单测 | ✅ |
| **P1 编排** | POST API + config→spec 映射 + 生命周期轮询 + registry 鉴权/预拉取 | `internal/orchestrator/*`、本地集成测试（swarm） | ✅ |
| **P2 监控** | 四类 checker + EventStore + 事件 API + 探活修复 | `internal/monitor/*`、监控集成测试 | ✅ |
| **P3 MCP** | Tools/Resources + Streamable HTTP（go-sdk v1.7.0，协议 2026-07-28） | `internal/mcp/*`、MCP e2e（官方 SDK 客户端） | ✅ |
| **P4 HA** | 节点身份识别（/info NodeID）+ manager 写守卫 + `/self` + MCP `get_self` | `internal/docker/info`、`orchestrator.Self` | ✅（多节点 per-node 上报未做，记入风险） |
| **P5 生产化** | 日志脱敏（`internal/logging`）+ TLS（`-tls-cert/-tls-key`）+ 部署 stack + README | `deploy/Dockerfile`、`deploy/stack.yml`、`internal/logging` | ✅（mTLS 双向认证、autolock 未做，记入风险） |

每个阶段配套：单元测试 + `docker testcontainers` 集成测试 + 文档更新。

**实现备注**：
- Docker 客户端为直接 HTTP（stdlib），非官方 SDK（模块结构损坏，见 §三）；MCP 用官方 go-sdk v1.7.0。
- P4 复用 swarm manager 的 Raft leader（`ControlAvailable`/`ManagerStatus.Leader`），未自研选举；多节点 per-node stats 上报、写操作跨节点代理未实现（v1 单 manager 部署可覆盖）。

---

## 十、风险与待确认事项

1. **Docker Engine API 版本差异**：`WithAPIVersionNegotiation()` 自动协商，但 `StartInterval`（1.44+）、部分 log 字段依赖版本；translator 需做版本探测降级。
2. **日志驱动限制**：`ServiceLogs` 仅 `json-file`/`journald`；若集群用 `fluentd`/`gelf` 等远端驱动，logCheck 不可用，需提示用户或改接集中日志（ELK）做 logCheck——v1 不做，记为已知限制。
3. **资源监控跨节点**：`stats` 是容器级，swarm 跨节点。v1 用 per-node agent 上报；若节点多（>20）考虑 cAdvisor sidecar，记为 v2 优化项。
4. **MCP 规范时效**：`2026-07-28` 为截至 2026-08-03 的最新版本；官方 SDK 对该版本的支持度需在 P3 启动时复核 `github.com/modelcontextprotocol/go-sdk` 的 `CHANGELOG`，必要时降级到 `2025-11-25`（有状态 initialize 模型）并按规范实现兼容探测。
5. **Leader 选举复杂度**：复用 swarm manager 的 Raft leader（`ManagerStatus.Leader`）而非自研选举，简化实现；但需处理 leader 切换时在途 operation 的接管（operation 状态落盘 + 新 leader 重放）。
6. **config 兼容性**：config 是自定义 schema，未来若要兼容 Compose 文件或 K8s manifest，需在 `translator` 上加适配层，不污染核心模型。
7. **私有仓库凭证**：`registryAuth` 用 swarm secret 还是 `AuthConfig` 内联？默认 secret 引用（更安全），但 create 时需先 `secret create`；提供 `registryAuth.inline`（base64）作为便捷模式，标注不推荐用于生产。

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
