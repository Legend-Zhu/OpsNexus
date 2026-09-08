# OpsGaurd 系统技术总览

> 伞形文档：给出平台整体架构、组件构成、通信与存储设计、安全模型、部署形态的全景图。
> 各子系统的深入设计与实施细节见 `docs/` 目录下的专项方案文档，本文不重复展开。
>
> 版本快照：2026-08-21，对应管理端（server）1.2.12 / Worker 1.2.7。

## 一、平台概述

### 1.1 定位

OpsGaurd（智能运维全链路自动化平台）用于纳管多个**网络隔离**的 Docker Swarm 集群，在管理端统一提供以下能力：

| 能力域 | 内容 |
| --- | --- |
| 容器编排 | 服务部署 / 更新 / 扩缩容 / 重启 / 删除，异步 operation 就绪判定，滚动更新 |
| 监控告警 | 端口 / HTTP / 日志 / 资源四类周期探针，事件聚合为告警，规则管理与自动通知 |
| 智能巡检 | YAML 巡检任务 + cron 调度 + AI 巡检报告 + 报告投递 + 异常转告警 |
| AI 异常排查 | 内嵌 AiNexus 网关（OpenAI / Anthropic 兼容端点），ReAct agent + MCP 工具直连各集群 |
| MLOps | Prompt Hub（版本化提示词）、模型接入与启停、call 级用量计量与费用报表、月度预算 |
| 镜像仓库 | 管理端内嵌 OCI registry（/v2 协议），经 gRPC 隧道中继到隔离集群 pull/push |
| 身份认证 | 本地用户 + SSO（OIDC RP）+ OpsGaurd 自身作为 OIDC IdP 向集群内系统发牌 |
| 纳管清单 | 非 swarm 的 standalone 容器 / 宿主机服务的清单化注册与周期探测 |

### 1.2 四个贯穿全系统的设计决策

理解下面四条，就理解了 OpsGaurd 的大部分代码形态：

1. **单向网络策略**：平台通信只依赖「管理端 → 集群 Worker」的单向通路，不需要集群反向访问管理端，隔离集群因此无需开放反向防火墙洞。一切通信由 server 发起——包括看似"推送"的事件回传（靠 server 发起的 `SubscribeEvents`/`SubscribeAudit` 双向流承载）和集群内系统访问管理端 IdP / 镜像仓库（靠 server 发起的 `Tunnel` 反向隧道承载）。
2. **零外部依赖**：无 MySQL / Redis / 消息队列。管理端用嵌入式 LevelDB，Worker 用纯 Go SQLite，部署一套 OpsGaurd 只需要 Docker 本身。
3. **离线优先**：目标环境为无外网内网，安装包、镜像、甚至 proto 代码生成（`genpatch`）都有离线路径。
4. **单二进制管理端**：server 进程同时托管 REST API、内嵌 AI 网关、内嵌 OCI 仓库、内嵌 OIDC IdP 和前端 SPA 静态文件，一个容器跑完管理面。

## 二、总体架构

```
                         ┌────────────────────────────────────────────────┐
                         │              管理机（单节点）                    │
                         │                                                │
   浏览器 ──HTTP/SSE──▶  │  opsguard-server (Go 单二进制, :8080)           │
                         │  ├─ REST API  /api/v1/*                        │
                         │  ├─ SPA 静态托管 ./web (Vue3 dist)              │
                         │  ├─ AiNexus AI 网关 /ainexus/*                  │
                         │  ├─ 内嵌 OCI 仓库 /v2/*                         │
                         │  ├─ 内嵌 OIDC IdP /api/v1/idp/*                 │
                         │  └─ LevelDB (./data)                            │
                         └───────────┬────────────────────────────────────┘
                                     │  gRPC（server 主动发起）
                                     │  bearer: worker token
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
 ┌──────────────┐            ┌──────────────┐            ┌──────────────┐
 │ 集群 A Worker │            │ 集群 B Worker │   ……       │ 集群 N Worker │
 │ (swarm       │            │              │            │              │
 │  global，    │            │  每集群一个   │            │              │
 │  每节点一个) │            │  leader 执行  │            │              │
 │              │            │  写操作       │            │              │
 │ HTTP :8080   │            │              │            │              │
 │ /mcp /healthz│            │              │            │              │
 │ gRPC :9080   │            │              │            │              │
 │ SQLite 队列  │            │              │            │              │
 │ docker.sock  │            │              │            │              │
 └──────────────┘            └──────────────┘            └──────────────┘

 集群内消费方（经 Worker 本地入口，免反向洞）：
   ├─ /idp-proxy/*   → r-nacos 等访问管理端 IdP（走 Tunnel 反向隧道）
   └─ /v2/*          → docker daemon pull/push 管理端内嵌仓库（走 Tunnel 反向隧道）
```

组件与代码位置：

| 组件 | 代码位置 | 形态 |
| --- | --- | --- |
| 管理端 server | `OpsGaurdWeb/server/` | Go 单二进制容器（含前端产物），生产 `docker run` |
| 前端 web | `OpsGaurdWeb/web/` | Vue3 SPA，构建产物由 server 托管 |
| Worker | `Worker/` | Go 静态二进制，swarm `global` 模式每节点一个 |
| 管理面协议 | `proto/opsguard.proto` | server ↔ Worker 的 gRPC 契约，双端各自生成 stub |
| 离线部署 | `deploy/offline/`（runbook 入仓） | docker 静态包 + swarm init + 离线镜像 load |

## 三、技术栈

| 组件 | 语言/运行时 | 核心依赖 | 存储 |
| --- | --- | --- | --- |
| server | Go 1.25 | gin v1.12、grpc v1.83 + protobuf、goleveldb、coreos/go-oidc v3、mark3labs/mcp-go（MCP 客户端）、robfig/cron v3、log/slog | LevelDB（`./data`，schemaVersion=3） |
| Worker | Go 1.25 | grpc v1.83、modelcontextprotocol/go-sdk v1.7（MCP 服务端）、modernc.org/sqlite（纯 Go）、自研 Docker Engine/Swarm REST 客户端（无官方 SDK） | SQLite（事件/审计持久队列） |
| web | TypeScript 5.7 | Vue 3.5、Vite 6、Element Plus 2.9、Pinia 2.3、vue-router 4.5、axios 1.7、js-yaml | — |

## 四、组件详解

### 4.1 管理端 server（`OpsGaurdWeb/server/`）

入口 `cmd/server/main.go` 装配全部服务。internal 包职责：

| 包 | 职责 |
| --- | --- |
| `api` / `router` | 全部 HTTP handler 与 gin 路由注册、SPA 静态托管 |
| `config` | `config.yaml` 加载与默认值 |
| `store` | LevelDB 门面 + 各领域 bucket（JSON 值 + key 前缀，WriteBatch 原子提交，meta/version 自增迁移） |
| `cluster` | 集群注册表 CRUD + Worker 健康探测 |
| `workerproxy` | 对 Worker 的 gRPC 客户端封装（编排/探测/日志流/事件订阅/反向隧道），内含 pb 生成代码 |
| `ingest` | 事件拉取落库 + 告警聚合 + 自动通知异步分发 |
| `alertrule` | 告警规则管理，apply 下发到 Worker 侧 monitoring 配置 |
| `notify` | 通知渠道（飞书 / webhook / 短信——短信仅支持经外网代理发送）与策略、记录 |
| `patrol` | 智能巡检：YAML flow + cron 调度 + AI 报告 + 投递 + 异常转告警 |
| `invmonitor` | 纳管清单对象的服务端周期探测（经 Worker 的 Check* RPC 指定节点发起） |
| `ainexus` | 内嵌 AI 排查网关（agent/ReAct、provider、MCP 客户端、工具、SSE、用量计量） |
| `ainexusrt` | 网关运行时配置管理（页面保存热重载，LevelDB 持久化） |
| `mlops` | MLOps 运营层（Prompt Hub、用量计量、费用报表、模型启停/绑定、月度预算） |
| `registry` | 内嵌 OCI 镜像仓库（/v2 协议 + basic auth + 页面传包构建 + retention） |
| `auth` | 本地用户 + SSO(OIDC RP) 认证、Bearer token 中间件、admin 角色 |
| `idp` | OpsGaurd 自身作为 OIDC 身份提供者（authorize/token/jwks/userinfo/introspect/discovery） |
| `idptunnel` | 每集群反向隧道管理器（流池、白名单路径、registry 凭据代持） |

对外接口面（默认监听 `:8090`，生产容器经配置覆盖为 `:8080`）：

- `/healthz`——健康检查。
- `/api/v1/*`——主 API 分组：`auth`（登录/SSO/me/改密）、`idp`（OIDC 端点 + client 管理）、`projects`、`clusters`（CRUD + workloads/events/audit/metrics/nodes/inventory）、`alerts`、`investigations`、`patrols`、`notify`、`alertrules`、`secrets`、`registry`、`users`（admin）、`ainexus`（chat/investigate/config）、`mlops`、`settings`。
- `/ainexus/*`——AI 网关原生端点（`/v1/chat/completions`、`/v1/messages`，OpenAI/Anthropic 兼容）。
- `/v2/*`——内嵌 OCI 仓库（独立 basic auth，不走会话认证）。
- SSE：`GET /api/v1/clusters/:name/nodes/stream`（节点列表 + 各节点指标流）、`GET .../workloads/:service/logs`（日志流透传）、AI 排查对话 SSE。无 WebSocket。
- NoRoute 回退 `index.html`（SPA history 路由）。

### 4.2 Worker（`Worker/`）

入口 `cmd/worker/main.go`：角色检测（swarm manager/node）→ 装配 HTTP(:8080) + gRPC(:9080) + MCP + 监控 + 节点本地 API + 隧道中继 → TLS/mTLS、优雅停机。internal 包职责：

| 包 | 职责 |
| --- | --- |
| `grpcapi` | `ManagementService` gRPC 服务端 + SubscribeEvents/Audit 双向流 + Tunnel 反向隧道池 |
| `orchestrator` | swarm 服务全生命周期（异步 operation、滚动更新、跨节点 NodeClient 代理、非 leader 写转发 leader） |
| `monitor` | 四类周期探针（端口/HTTP/日志/资源）+ SQLite 事件持久队列（单调 seq、ack、GC） |
| `nodeagent` | 每节点本地 HTTP API：stats、容器/宿主机 exec、SSE 日志、按需探测、容器健康批量查询 |
| `registryproxy` | `/v2/` 镜像中继（pull/push 经隧道流式转发）+ 按内容寻址的 blob 磁盘 LRU 缓存 |
| `idpproxy` | `/idp-proxy/` 本地入口：集群内 IdP 请求经反向隧道转发到管理端并改写发现文档 |
| `mcp` | MCP 服务器（Streamable HTTP `/mcp` + stdio，20 个工具：编排、日志、事件、节点、exec、拨测等） |
| `agent` | Worker 自身配置（角色、命令黑白名单、auth tokens）+ nsenter 宿主机命令执行 |
| `authz` | Bearer token 鉴权中间件（HTTP 与 gRPC 拦截器共用 token 集） |
| `audit` | 敏感操作审计的 SQLite 持久存储，经 gRPC 流回推 |
| `docker` | 自研 Docker Engine/Swarm REST 客户端（unix socket / TCP+TLS） |
| `logging` | slog 脱敏 Handler（token/secret 等值 → `[REDACTED]`） |
| `config` | 管理端下发的 `service:` + `monitoring:` 配置 schema 与校验 |

部署形态：swarm `global` 模式每节点一个，挂载 `docker.sock`、`agent-config.yaml`、`/var/lib/opsguard`（SQLite 队列）。注意 swarm 会忽略 `privileged`/`pid:host`，需要宿主机完整可见的场景用裸 `docker run`（见 `Worker/deploy/stack.yml` 注释）。

### 4.3 前端 web（`OpsGaurdWeb/web/`）

- 交互约定：axios 单例（`baseURL=/api`，15s 超时，请求注入 `Bearer`，401 统一清 token 跳登录，登录接口本身除外）；统一响应体 `{code,message,data}`。
- 实时：两种 SSE——集群详情页 `EventSource` 订阅节点流（token 经 query 传，因 EventSource 不能带 header）；AI 排查对话用 `fetch` + ReadableStream 手动消费。
- 信息架构（侧边菜单）：**总览 / 项目 / 集群 / 告警中心 / 镜像仓库 / 智能巡检 / 异常排查 / MLOps / 通知中心 / 系统设置**。工作负载与监控已收编进集群详情页。
  - MLOps 四个 tab：模型接入（原系统设置-模型配置移入）/ 提示词 / 模型 / 用量费用。
  - 智能巡检内嵌巡检报告投递配置。
  - 系统设置 tab：用户（admin 可见）/ SSO / 身份提供者 / AI 排查网关 / 密钥。

## 五、通信设计（核心）

契约文件：`proto/opsguard.proto`（`package opsguard.v1`，service `ManagementService`，25 个 RPC）。同一份 proto 编译进两个独立模块（`proto/gen.sh`，`go_package` 由各模块自备 `M` 参数注入）：Worker 侧 `Worker/internal/grpcapi/pb`（服务端），server 侧 `OpsGaurdWeb/server/internal/workerproxy/pb`（客户端），互不交叉 import。

### 5.1 方向性

server 始终是 gRPC **客户端**，Worker 是**服务端**（gRPC `:9080`）。事件不靠 Worker 主动推送，而由 server 发起 `SubscribeEvents`/`SubscribeAudit` 双向流：首帧携带续传游标 `after_seq`，Worker 按序回推事件，server 回 `ack_seq` 确认后 Worker 才 GC 已确认条目；断线重连从最后持久化游标续传，重启不丢（SQLite 持久队列兜底）。

### 5.2 五类流式 RPC 语义

| RPC | 类型 | 用途 |
| --- | --- | --- |
| `WatchNodeStats` | 服务端流 | 节点列表 + 每节点指标；首帧（kind=init）只带基础列表立即返回，后续各节点指标完成即推（慢节点不阻塞他人）；驱动 SSE `/nodes/stream` |
| `StreamLogs` | 服务端流 | 工作负载日志流（替代旧 SSE /local/logs） |
| `SubscribeEvents` / `SubscribeAudit` | 双向流 | 事件/审计回推，游标 + ack + GC（见上） |
| `Tunnel` | 双向流 | 多路字节隧道：集群内 HTTP 请求以 `TunnelFrame`（2MiB 分帧，`chunk_seq`/`chunk_eof`/`req_chunked`）请求帧发往 server，server 回源本机 IdP / 内嵌仓库后按 id 回响应帧 |

写操作（Deploy/Update/…）只在 swarm Raft leader 上执行，非 leader manager 经内部 gRPC 客户端转发 leader，对 server 透明。

### 5.3 反向隧道与镜像中继

- **流池**：server 预开 N 条（默认 16，`OPSGUARD_TUNNEL_POOL` 两端须一致）Tunnel 双向流，一请求独占一流（独立 HTTP/2 流控窗口，消除单流队头阻塞）；borrow 跳过死流，keepalive 判死回收。
- **白名单**：隧道仅放行 `/api/v1/idp/*`、`/.well-known/openid-configuration`（仅此一条）、`/v2` 前缀；registry 的 basic auth 由 server 侧代持，集群节点无感。
- **镜像中继**：Worker 本地 `/v2/` 把集群内 docker daemon 的 pull/push 流式中继到管理端内嵌仓库；pull 侧带按内容寻址的磁盘 LRU 缓存（digest 校验防路径逃逸，临时文件原子 publish，半截 blob 永不入缓存）。
- **超时教训（重要）**：push 数百 MB 大镜像层时，任何固定 HTTP ReadTimeout/WriteTimeout 都会中途掐断连接（曾表现为 30s 502）。修复原则：Worker HTTP server `ReadTimeout=WriteTimeout=0`，用 `IdleTimeout=120s` 兜底回收空闲连接（`Worker/cmd/worker/main.go`，worker 1.2.7）。gRPC 层消息上限 16MiB，流控窗口 32MiB/流 + 64MiB/连接（否则大 blob 拉取吞吐塌缩到几十 KB/s）。

### 5.4 MCP 双向使用

- Worker 是 **MCP 服务端**：`/mcp`（Streamable HTTP）+ stdio，暴露 20 个运维工具。
- server 是 **MCP 客户端**：AiNexus 网关经各集群 Worker 的 `/mcp` 调工具，实现 AI 排查对集群的真实操作能力；worker 与 MCP 工具均以 OpsGaurd IdP 签发的 token 鉴权（OAuth 资源元数据端点 RFC 9728）。

## 六、存储设计

**管理端 LevelDB**（`store.path`，默认 `./data`）：值以 JSON 为主（`meta/version` 为二进制、ainexus 运行时配置为 YAML），key 前缀 bucket，进程内 mutex 串行化 + WriteBatch 原子提交，meta/version 自增迁移（当前 schemaVersion=3）。主要 bucket：

- 事件/告警：`event/<seq>`、`alert/<id>` + 索引（含「已通知」标记）；`seq/<kind>` 发号器。
- 巡检：`patrol`、`patrolrun`、`report`。
- 项目/集群：`project`、`cluster`（纳管清单 InventoryConfig 嵌在 Cluster 内）+ `cache`（集群资源缓存）。
- 用户/通知/规则：`user/*`、`notify/channel|policy|record`、`alertrule`。
- IdP：idpclient / idpcode / idpatoken / idprtoken / idpkey / idpsession。
- MLOps：prompt（版本化）、mlusage（call 级明细）、mlusage_call（call 幂等索引）、mlusage_day（日聚合）、mlpricing、mlbinding、mlbudget、mlops_audit。
- 其他：ainexus（网关运行时配置）、investigation、secret、cursor（事件订阅游标）、svccfg、settings。

**Worker SQLite**（`dataDir`，容器内 `/var/lib/opsguard`）：monitor 事件与 audit 的持久队列——单调 seq、按 `after_seq` 续传、ack 后 GC，是事件链路「不丢不重」的基石。

## 七、业务子系统速览

| 子系统 | 链路 | 核心代码 |
| --- | --- | --- |
| 告警闭环 | Worker 探针产事件 → SQLite 队列 → server SubscribeEvents 拉流 → ingest 聚合为告警（含恢复事件，防幽灵告警）→ notify 异步分发（产生/恢复/里程碑） | `server/internal/ingest`、`notify`、`alertrule`；`Worker/internal/monitor` |
| 智能巡检 | YAML flow 定义检查项 → cron 调度 → 经 Worker 执行探测 → AI 生成报告 → 投递（通知渠道）→ 异常项转告警 | `server/internal/patrol` |
| AI 排查（AiNexus） | 前端 SSE 对话 → 内嵌网关（OpenAI/Anthropic 兼容）→ ReAct agent 调工具（含各集群 Worker MCP）→ 用量计量入 MLOps | `server/internal/ainexus`、`ainexusrt` |
| MLOps | provider call 级计量 → 日聚合费用报表 → 模型单价快照 / 启停热重载 / 场景绑定 / 月度预算档位通知；Prompt Hub 版本化管理提示词 | `server/internal/mlops` |
| 纳管清单（invmonitor） | 页面登记 standalone 容器/宿主机服务 → server 周期经 Worker `Check*`/`NodeContainers` 从指定节点探测 → 状态翻转进 ingest 聚合告警；与集群告警规则双向同步 | `server/internal/invmonitor` |
| IdP / SSO | OpsGaurd 双角色：对外是 OIDC RP（SSO 登录），对内是 OIDC IdP（向 r-nacos 等集群内系统发牌，经反向隧道免开洞） | `server/internal/auth`、`idp`、`idptunnel`；`Worker/internal/idpproxy` |
| 镜像仓库 | 页面 zip 上传 → server 内嵌 docker build → /v2 OCI 存储 → 集群经 Worker 中继 pull；retention 清理 | `server/internal/registry`；`Worker/internal/registryproxy` |

## 八、安全模型

- **用户认证**：本地用户（口令为加盐单轮 SHA-256；registry `/v2` 账号另用 bcrypt htpasswd）+ SSO（OIDC RP）；会话为 Bearer token（HMAC-SHA256 签名，`auth.token_secret`，`token_ttl` 控制）。配了 secret / SSO / IdP 之一即启用认证并播种默认 admin（密码可用环境变量 `OPSGUARD_ADMIN_PASSWORD` 覆盖）。
- **角色**：admin 与普通用户两级。写操作按角色收敛（用户管理、IdP clients、MLOps 写、AI 网关配置等 admin 限定，server 1.2.12 起）。
- **server ↔ Worker**：每集群独立 worker token（`clusters.<name>.token`），gRPC metadata / HTTP header 双面生效（Worker `authz` 包共用 token 集）；CA 证书齐备时强制 mTLS。
- **命令执行**：容器 exec 与 nsenter 宿主机 exec 受每节点 `commandPolicy` 黑/白名单与超时约束（`WORKER_ALLOW_HOST_EXEC` 等开关默认收紧）。
- **隧道白名单**：Tunnel 仅放行 IdP 与 /v2 路径；registry basic auth 由 server 代持。
- **审计与脱敏**：敏感操作入 SQLite 审计队列回推管理端；Worker 日志对 token/secret 类值统一 `[REDACTED]`。
- **密钥文件不进仓**：`agent-config*.yaml`、`stack*.yml`、install 脚本、镜像 tar 均已 gitignore（含 worker token 与集群地址），仓库只保留 runbook。

## 九、可靠性与性能要点

| 机制 | 设计 |
| --- | --- |
| 事件不丢不重 | SQLite 持久队列 + 单调 seq + `after_seq` 游标续传 + ack 后 GC；事件 ID 加序号防低分辨率时钟同 tick 去重误伤 |
| 大传输吞吐 | gRPC 16MiB 消息上限、32MiB/流 + 64MiB/连接流控窗口；隧道 2MiB 分帧流式；blob 磁盘 LRU 缓存 |
| 大层 push | HTTP 读写超时置 0、`IdleTimeout=120s` 兜底（见 §5.3 教训） |
| 死流回收 | gRPC keepalive 15s ping / 5s 无 ack 判死；server 重启后半开流 ~20s 内 revoke，隧道池跳过/重建死流 |
| 并发化 | 节点指标按节点并发（慢节点不阻塞首屏）；容器健康批量并发 inspect；stats 5s 缓存 |
| 超时预算 | NodeClient 总预算 15s、单次探测 10s、命令默认 30s；API axios 15s |
| 优雅停机 | SIGTERM → HTTP 10s shutdown + gRPC GracefulStop + 关闭 leader 转发连接（`TunnelManager.Detach` 已实现但未接入停机路径，隧道的流随 gRPC 关闭自然断开） |
| 幽灵告警治理 | 检查配置变更/移除时补发恢复事件 |

## 十、配置参考

### 10.1 管理端（`configs/config.yaml`，`-config` 指定）

| 配置 | 说明 |
| --- | --- |
| `server.addr` | 监听地址，默认 `:8090`（生产容器覆盖为 `:8080`） |
| `store.path` | LevelDB 目录，默认 `./data` |
| `auth.token_secret` / `token_ttl` / `sso.oidc.*` | 认证开关与参数 |
| `idp.enabled` / `issuer` / `*_token_ttl` | 启用内嵌 IdP（自动开启认证与 idptunnel） |
| `clusters.<name>.worker_url` / `token` / `mcp_url` / `desc` | 每集群 Worker gRPC 地址与凭据 |
| `registry.*` | 内嵌仓库：enabled/hostname/storage/retention/max_upload/users/builder/relay 账号 |
| `ainexus.*` | AI 网关初始配置（仅当从未在页面保存过运行时配置时作为回退，保存后以 LevelDB 运行时配置为准） |
| `mlops.*` | enabled/usage_retain_days/usage_queue_size/usage_gc_interval/timezone/currency（当前仅 CNY） |

环境变量：`OPSGUARD_ADMIN_PASSWORD`、`OPSGUARD_TUNNEL_POOL`（默认 16，须与 Worker 侧一致）。

### 10.2 Worker（`agent-config.yaml`，模板 `deploy/agent-config.yaml.example`）

`worker.role / listen / grpcListen / dataDir`、`commandPolicy`（黑白名单）、`auth.tokens`。环境变量可覆盖：`WORKER_ROLE`、`WORKER_TOKENS`（`name=secret,...`，设置即启用鉴权）、`WORKER_ALLOW_HOST_EXEC`、`WORKER_COMMAND_TIMEOUT`、`OPSGUARD_TUNNEL_BASE`、`OPSGUARD_TUNNEL_POOL`、`OPSGUARD_IDP_ISSUER`、`OPSGUARD_REGISTRY_CACHE_DIR`、`OPSGUARD_REGISTRY_CACHE_MB` 等。flags：`-addr -grpc-addr -data-dir -agent-config -tls-cert/-tls-key/-tls-ca` 等。

## 十一、部署形态

- **生产形态是 Docker Swarm**（管理端自身 `docker run` 单容器 + 各被管集群 swarm global Worker）。
- **离线部署**：docker 静态二进制包（systemd + daemon.json）→ swarm init/join → 镜像离线 load 或经内嵌仓库中继拉取。
- **端口矩阵**（默认值，均可在部署时自定义；各集群实际端口以 runbook 为准，被管集群常用 `mode: host` 的 6060/6061）：

| 进程 | 端口 | 用途 |
| --- | --- | --- |
| server | 8090（默认）/ 8080（生产容器） | 页面 + 全部 API + 内嵌仓库/IdP/AI 网关 |
| Worker HTTP | 8080（默认） | `/mcp`、`/healthz`、`/idp-proxy/*`、`/v2/*` 中继、节点本地 API |
| Worker gRPC | 9080（默认） | ManagementService（server 连入） |

> 接入集群时，管理端需**同时可达**被管集群 manager 的 Worker gRPC 与 HTTP 两个端口
> （gRPC 探活 + HTTP `/healthz` 校验），接入表单分别填写两个端点，MCP 地址自动取
> `{HTTP 地址}/mcp`——详见 `docs/Worker部署手册.md` §5。

- **版本现状**：server 1.2.12、Worker 1.2.7。

## 十二、开发与构建  

- proto 变更：`proto/gen.sh` 双端生成；环境无 protoc / 无 module proxy 时，回退 `Worker/cmd/genpatch`（`genpatch.exe` 即其产物）离线给两端 pb.go 打 TunnelFrame 字段增量补丁，幂等且带 verify 往返校验。
- Worker 构建：`Worker/deploy/Dockerfile`（golang:1.25-alpine 多阶段，CGO_ENABLED=0 静态编译，alpine 3.20 运行时）；版本号经 `-ldflags` 注入 `internal/version`。
- server 构建：`OpsGaurdWeb/deploy/Dockerfile`，产物内含 `web/dist`；前端独立 `npm run build`（vue-tsc 校验）。
- 前端开发：`web/` 下 Vite dev 5173，`/api` 代理到 `http://localhost:8090`。
