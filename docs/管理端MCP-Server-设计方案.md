# 管理端 MCP Server 设计方案（自然语言运维入口）

> 版本：v0.5（终稿）
> 日期：2026-09-07（v0.1 设计；同日 P1 + P2 + P3 全部实现落地）
> 状态：**已完成**——60 工具 + 5 resources + RFC 9728 元数据 + IdP token 回退接入 + exec 三重防护 + CIDR 白名单 + 运营页；全链路冒烟测试守护
> 关联：`docs/管理端MCP-使用手册.md`（**使用/运维手册**）、`Worker/internal/mcp/`（Worker MCP Server 先例，20 工具）、《AiNexus-Skill与MCP-设计方案.md》（**客户端**侧 MCP 管理，本方案的镜像篇）、《OpsGaurd-系统技术总览.md》§5.4（MCP 双向使用）

### 附：P1 实现落点（与设计条目对照）

| 设计条目 | 代码 |
|---|---|
| MCP Server 主体 / stateless 传输 / Instructions 运行手册 | `mcpserver/server.go`（`New` / `HTTPHandler` / `serverInstructions`） |
| 命名 token + read/write scope + 端点中间件 | `mcpserver/authz.go`（SHA-256 摘要 + 常数时间比较；base URL 注入） |
| 写工具审计 + 结果截断 | `mcpserver/audit.go`、`server.go truncStr`（128KB 头尾保留）、`store/mcpaudit.go`（schema v4） |
| 上传票据 + curl 直传端点 | `mcpserver/upload.go`（一次性/TTL 1h/大小必等） |
| 委托排查（双 agent 协同） | `mcpserver/tools_investigation.go`（信号量/后台执行/状态机 running|done|error）；网关侧 `ainexus/server.RunInvestigation`（事件流转写证据轨迹） |
| 六组工具 | `tools_project.go`(5) / `tools_cluster.go`(6) / `tools_service.go`(10) / `tools_build.go`(6) / `tools_alert.go`(4) / `tools_diag.go`(4) |
| 路由挂载（`/mcp`、`/api/v1/mcp/build-upload`，均在平台认证组之外） | `router/router.go` + `api/response.go SetMCPServer` |
| 装配与 fail-fast（无可用 token 启动即退出） | `cmd/server/main.go` |
| 配置（`mcp:` 段） | `config/config.go` + `mcpserver/config.go`；样例见 `configs/config.yaml` 尾部 |
| 测试 | `mcpserver/mcpserver_test.go`（鉴权/票据/截断/状态机）+ `smoke_test.go`（go-sdk 客户端真实走 initialize→tools/list，41 工具清单断言） |

### 附：P2 实现落点（与设计条目对照）

| 设计条目 | 代码 |
|---|---|
| token 运行时表（LevelDB）+ 静态种子镜像/禁用/热生效 | `mcpserver/tokens.go`（syncTokens 原子换表）、`store/mcptoken.go`、`authz.go`（static+runtime 双层表） |
| 管理 API（admin）：tokens CRUD/启停、审计查询、用量、接入信息 | `api/mcp.go`（`/api/v1/mcp/*`，router 挂 AdminMiddleware） |
| 调用统计：/mcp 中间件计数（tools/call 报文解析，全工具零埋点）+ 30s flush 日聚合 | `mcpserver/usage.go` |
| probe_flow / node_processes（诊断 6 工具，总 41） | `tools_diag.go` |
| 前端「系统设置 → MCP 接入」tab：端点/接入模板复制、token 管理（secret 一次性展示）、用量表、审计表 | `web/src/views/system/McpAccess.vue` + `api/index.ts mcpApi` |
| secret 生命周期 | 创建时随机 64 位 hex 仅返回一次；落库 SHA-256；禁用热生效；静态种子可禁不可删 |

### 附：P3 实现落点（与设计条目对照）

| 设计条目 | 代码 |
|---|---|
| exec 透传（开关 + exec scope + 经 Worker MCP 通道 + 审计） | `tools_exec.go`（execGuard/auditedExec/callWorkerTool）、`authz.go` ScopeExec、ainexus `mcp/manager.go CallServerTool` + `server/server.go CallClusterTool` |
| 巡检域 8 工具（含 YAML 骨架内嵌工具描述，助手可代写巡检流程） | `tools_patrol.go` |
| 告警规则域 5 工具（save 默认 Apply 下发；rule 体复用 store.AlertRule 同构 schema） | `tools_alertrule.go` |
| 通知域 4 工具（channel 更新时空 config 沿用旧值防凭据抹丢） | `tools_notify.go` + `notify.Service.GetChannel` |
| worker_url CIDR 白名单（add/update 校验；非法网段配置报错不放行） | `allowlist.go checkClusterURLAllowed` |
| MCP resources（只读视图：集群/活跃告警/巡检/镜像 + 集群服务模板） | `tools_resources.go`（go-sdk AddResource/AddResourceTemplate，与 Worker resources 同构） |
| OAuth 2.1 / RFC 9728 | `oauth.go`（Metadata 元数据端点 + authenticateAny 回退链 + IdPTokenValidator 接口）；`idp/endpoints.go ValidateAccessToken`（导出校验：验签+iss/exp+吊销）；`cmd/server/main.go` idpValidatorAdapter 装配；`router.go` 挂 `/.well-known/oauth-protected-resource`（公开） |

---

## 一、背景与定位

### 1.1 现状：MCP 只有「下半身」

系统当前的 MCP 能力是单向的：

```
                    ┌────────────────────────────────────────────┐
  外部 AI 助手      │            opsguard-server                 │
  (ZCode/Claude/    │  REST API ──▶ service 层 ──▶ workerproxy ─┼──▶ Worker /mcp (MCP Server, 20 工具)
  Cursor)           │  AiNexus 网关 ── MCP 客户端 ───────────────┘
   ✗ 无法接入       │                （消费 MCP，不生产 MCP）       （server 自己也不暴露 MCP）
```

- **Worker 是 MCP 服务端**（`/mcp`，20 个集群内工具），但只有 server 内嵌的 AiNexus 网关会去连它；
- **server 是 MCP 客户端**（ainexus/mcp manager），但没有把自身管理能力暴露为 MCP；
- 结果：外部 AI 助手（ZCode、Claude Desktop、Cursor 等）想操作平台，只能靠「人翻译成页面点击」，平台对自然语言世界是封闭的。

### 1.2 目标场景

外部助手配置一个 MCP server 指向管理端后，一句自然语言即可完成完整运维闭环：

> "新建项目 demo，把 10.0.1.5:9080 那套集群纳管到这个项目，把我桌面的 gw.zip 构建成镜像 gw:v1，
>  然后在集群上部署 gateway 服务跑 2 个副本，8080 对外。"
>
> "prod 集群昨晚有什么告警？gateway 一直重启，帮我看看日志和资源占用，必要时重启它。"
>
> "gateway 报空指针了，我怀疑是刚改的连接池代码——你去集群拉日志确认一下，我这边对着代码看。"

覆盖能力域：**项目**、**集群纳管**、**服务部署/扩缩容**、**zip 构建镜像**、**故障排查**（告警/事件/日志/节点/主动拨测）、**双 agent 协同排障**（本机 agent 与内嵌 AiNexus 联合定位，见 7.3）。

### 1.3 概念定位：service 层的第四个消费者

| 消费者 | 协议 | 权限主体 | 破坏面 |
|---|---|---|---|
| 前端 SPA | REST + SSE | 登录用户（admin/viewer） | 页面确认框 |
| 内嵌 AiNexus 网关 | 进程内直调 | 网关自身 | confirm 工具参数 |
| Worker（被调用方） | gRPC | 集群 token | — |
| **外部 AI 助手（本方案）** | **MCP（Streamable HTTP）** | **命名 MCP token（read/write scope）** | **confirm=true 工具参数（镜像 Worker MCP 模式）** |

关键架构事实：**MCP 工具层不实现任何业务逻辑**，全部薄封装到既有 service 层（`cluster.Service`、`workerproxy.Client`、`registry.Service`、`store` 告警/事件/排查会话），与 REST handler 平级。这与 Worker MCP 的做法完全一致（工具 → orchestrator/monitor，不旁路）。

---

## 二、目标与非目标

**目标**

1. 管理端进程内新增 MCP Server，单端口复用（`/mcp`，与页面/REST 同 `:8090`），外部助手零额外网络配置即可接入。
2. 自然语言可完成：项目 CRUD、集群纳管/移除、服务部署/更新/扩缩/重启/删除/日志、zip 构建镜像与构建跟踪、告警/事件查询处置、节点与资源观测、主动拨测诊断。
3. **双 agent 协同排障**：本机 agent（有代码上下文、能改代码发版）与内嵌 AiNexus（有集群实时证据与领域技能，无代码上下文）经 MCP 工具互通，形成「代码侧假设 ↔ 集群侧取证」闭环——这是本 MCP 区别于普通 REST 皮肤的核心价值（见 7.3）。
4. 安全对齐既有平台：独立命名 token + 读写 scope、破坏性操作 `confirm=true` 双保险、全量操作审计、结果体积上限、敏感值不回显。
5. 实现模式与 Worker MCP 完全同构（同库、同 stateless 传输、同 typed struct schema、同 confirm/审计惯例），降低维护与认知成本。

**非目标**

1. 不做 MCP Resources / Prompts 的完整能力面（外部助手以 tools 为主，resources 仅 P3 点缀）。
2. 不做 OAuth 2.1 动态客户端流程（P1 静态 token；内嵌 IdP 的 OAuth 路径留 P3，与 Worker 的 RFC 9728 演进对齐）。
3. 不暴露容器/宿主机 exec（P1 明确排除，见 7.4；P2 以独立 `exec` scope 受控引入）。
4. 不做工具级细粒度授权（scope 只到 read/write/exec 三档；按工具开白名单是 P3 以后的事）。
5. 不替代 REST API——REST 仍是唯一权威接口面，MCP 是它的自然语言皮肤。

---

## 三、总体架构

### 3.1 链路

```
外部 AI 助手（ZCode / Claude Desktop / Cursor / 自研 Agent）
    │ MCP Streamable HTTP（单 POST 端点，stateless，2026-07-28 规范）
    │ Authorization: Bearer <mcp token>
    ▼
opsguard-server :8090  ┌─ r.Any("/mcp")  → mcpAuth 中间件（命名 token 校验 + actor 注入）
   （gin 同端口）       │                  → gin.WrapH(StreamableHTTPHandler)
                       │
                       │  internal/mcpserver/
                       │    ├─ tools_project.go   ──▶ cluster.Service (CreateProject/…)
                       │    ├─ tools_cluster.go   ──▶ cluster.Service (Add/Update/Remove，含 Worker 探活)
                       │    ├─ tools_service.go   ──▶ cluster.WorkerClient(name) → workerproxy（Deploy/Scale/…）
                       │    ├─ tools_build.go     ──▶ registry.Service (SubmitBuild/GetBuild/ListImages)
                       │    ├─ tools_alert.go     ──▶ cluster.Service.Alerts / store（告警、事件）+ ainexusrt（委托排查）
                       │    ├─ tools_diag.go      ──▶ workerproxy（CheckPort/CheckHTTP/CheckFlow/NodeStats/ListNodes）
                       │    ├─ authz.go           ── token 表（常数时间比较）+ scope 检查
                       │    └─ audit.go           ── LevelDB mcp_audit bucket + slog
                       ▼
                 既有 service 层（REST handler 同款依赖，零业务逻辑复制）
```

### 3.2 技术选型

| 决策 | 选择 | 理由 |
|---|---|---|
| MCP 库 | **官方 `github.com/modelcontextprotocol/go-sdk` v1.7.0**（与 Worker 相同） | Worker 已验证：typed struct 自动生成 JSON Schema（`description` struct tag）、stateless Streamable HTTP、confirm 惯例。管理端 go.mod 现仅有 `mark3labs/mcp-go`（**客户端**，ainexus 用）；双库并存各司其职，不做迁移 |
| 传输 | Streamable HTTP，`Stateless: true` | 2026-07-28 规范要求；无会话状态，gin 后面随便挂；助手侧兼容性最好（ZCode/Claude/Cursor 全支持） |
| 端口 | 复用 `:8090`，路径 `/mcp` | 单二进制哲学；不开新端口、不动防火墙。与 Worker 的 `/mcp` 路径约定一致 |
| 会话语义 | 无状态，每请求独立 | MCP 工具全部是无副作用查询或幂等写，无需会话；重试安全 |

### 3.3 代码骨架

```go
// internal/mcpserver/server.go
package mcpserver

func New(deps Deps, log *slog.Logger) (*Handler, error) {
    h := &Handler{deps: deps, log: log}
    srv := mcp.NewServer(&mcp.Implementation{
        Name: "opsguard", Title: "OpsGaurd", Version: "1.0.0", // P1 硬编码，后续随构建注入
    }, &mcp.ServerOptions{
        Instructions: "OpsGaurd 平台管理 MCP。所有集群类工具都要求 cluster 参数（先用 cluster_list 查名称）。" +
            "破坏性操作（cluster_remove / project_delete / service_remove / service_scale 到 0 / image_delete）" +
            "要求 confirm=true，助手必须先向用户复述影响并得到确认后再传 confirm=true。",
        Logger: log,
    })
    h.registerProjectTools(srv)   // 按域分文件注册，镜像 Worker/internal/mcp 的分文件模式
    h.registerClusterTools(srv)
    h.registerServiceTools(srv)
    h.registerBuildTools(srv)
    h.registerAlertTools(srv)
    h.registerDiagTools(srv)
    h.srv = srv
    return h, nil
}

// HTTPHandler 与 Worker 相同：stateless Streamable HTTP。
func (h *Handler) HTTPHandler() http.Handler {
    return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return h.srv },
        &mcp.StreamableHTTPOptions{Stateless: true})
}

// router.go 挂载（伪码）：
//   public = append(public, "/mcp")          // 豁免平台登录 token 中间件
//   r.Any("/mcp", h.MCPAuth(), gin.WrapH(mcpH.HTTPHandler()))
//   r.POST("/api/v1/mcp/build-upload", h.MCPUploadTicket(), buildUpload)   // 大包直传（见 6.2）
```

工具定义方式完全沿用 Worker：入参 typed struct + `json`/`description` tag 自动出 schema，handler 一行校验、一行调 service、一行返回 typed 结果——不手写 JSON Schema。

---

## 四、认证与授权

### 4.1 方案对比与决策

| 候选 | 评估 | 结论 |
|---|---|---|
| 复用登录 token（HMAC 用户 token） | TTL 24h 到期助手频繁掉线；绑定用户密码生命周期；无法单独吊销；角色只有 admin/viewer 两档 | ✗ |
| **独立命名 MCP token（静态，P1）** | 与 Worker `auth.tokens`（name→secret）完全同构；长期有效、可命名、可吊销、可按 read/write 分权；助手配置一次永久用 | ✓ P1 |
| 内嵌 IdP 签发 OAuth token | 能力具备（idp 包），但外部助手接入 OAuth 流程重，内网静态 token 已满足 | P3（对齐 Worker 的 RFC 9728 演进） |

**硬性规则：只要 `mcp.enabled=true`，MCP 端点永远要求 token——即使平台认证整体关闭（内网 bootstrap 模式）也不例外**。平台无认证时 REST 敞开是既定形态，但 MCP 是给「AI 自主调用」的口子，必须默认上锁，避免零鉴权的写操作面。

### 4.2 Token 模型

```go
// internal/mcpserver/authz.go
type Token struct {
    Name      string `json:"name"`       // 如 "zcode"、"patrol-bot"；即审计 actor
    Secret    string `json:"-"`          // 仅 SHA-256 摘要入库，原文不落盘
    SecretSHA string `json:"secret_sha"`
    Scope     string `json:"scope"`      // read | write（write 含 read；P2 增 exec）
    Enabled   bool   `json:"enabled"`
    CreatedAt int64  `json:"created_at"`
    LastUsed  int64  `json:"last_used"`  // 展示用，懒更新
}
```

- **校验**：`Authorization: Bearer <secret>`，对全部启用 token 做 SHA-256 后常数时间比较（`subtle.ConstantTimeCompare`，镜像 Worker authz）；命中后把 `actor = name@clientIP` 注入 context（审计与告警 ack 的 `AckedBy` 都用它）。
- **scope 执行**：工具注册时声明所需 scope（读工具 `read`、写工具 `write`），中间件在 `tools/call` 前检查；read token 调写工具返回明确错误文本（"token 'viewer' is read-only; tool 'service_deploy' requires write scope"），助手能向用户解释原因。
- **来源**：P1 从 `config.yaml` 静态加载（种子），P2 增加管理 API + LevelDB（`settings/mcp_tokens`）运行时增删改，静态种子与运行时表合并（静态为底、运行时可覆盖/禁用）。

```yaml
# configs/config.yaml
mcp:
  enabled: true
  max_investigations: 4      # 委托排查并发上限（见 7.3）
  tokens:                    # 静态种子；secret 建议生成 32 字节 hex
    - {name: zcode,  secret: "9f86d0...", scope: write}
    - {name: viewer, secret: "08f3a1...", scope: read}
```

---

## 五、工具清单（P1 全集 39 个）

命名规则：`<域>_<动作>`，与 Worker 的平铺名错开（助手同时挂两个 server 时不混淆）。除标注外全部需要 `read` scope；**粗体**为 `write` scope；` confirm` 列标注 confirm=true 防护。

### 5.1 项目（project_*，5 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `project_list` | — | 列表（id/name/desc/cluster_count） |
| `project_get` | `id` | 详情 |
| **`project_create`** | `name, desc?` | 同名拒绝（复用 service 校验） |
| **`project_update`** | `id, name?, desc?` | |
| **`project_delete`** `confirm` | `id` | 仅解绑集群不级联删除（与 REST 语义一致）；confirm 错误文案列出该名字下集群数 |

### 5.2 集群纳管（cluster_*，6 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `cluster_list` | `project?` | 名称/状态(online/offline)/项目/描述/LastSeen；提示下一步常用 cluster 名 |
| `cluster_get` | `name` | 详情 + 健康探测结果 + 纳管清单(inventory)摘要 |
| **`cluster_add`** | `name, worker_url, worker_http_url?, token?, project?, desc?` | **纳管**：复用 `cluster.Service.Add`——先 gRPC `Self` 探活（5s 超时，必须 swarm manager），给了 `worker_http_url` 再探 HTTP `/healthz`（MCP 同端口，默认 8080）；全部通过才落库，失败把探活错误原文返回给助手。MCP 地址推导优先级：显式 `mcp_url` > `{worker_http_url}/mcp` > 旧兜底 `{worker_url}/mcp` |
| **`cluster_update`** | `name, worker_url?, worker_http_url?, token?, project?, desc?` | token 传空沿用旧值（脱敏惯例）；HTTP 端点变化时自动推导的 `mcp_url` 跟随更新（显式覆盖过的保持不动） |
| **`cluster_remove`** `confirm` | `name` | 级联语义同 REST：停事件订阅、清告警规则、断集群 MCP；confirm 错误文案提示"将停止该集群全部监控与告警" |
| `cluster_events` | `name, service?, type?, limit?`(≤100, 默认 20) | 读 server 侧 ingest 落库的聚合事件（含恢复事件），非 Worker 原始队列 |

### 5.3 服务部署（service_*，10 个）

全部要求 `cluster` 参数路由到目标集群（`cluster.WorkerClient(name)`）。

| 工具 | 参数 | 说明 |
|---|---|---|
| `service_list` | `cluster, label?` | 名称/镜像/副本(running/desired)/端口 |
| `service_get` | `cluster, service` | 详情 + tasks + **当前 config**（来自 svccfg 快照；助手据此修改后交给 service_update） |
| **`service_deploy`** | `cluster, config` | config 为 Worker 配置 YAML/JSON 全文（与 Worker `deploy_service` 同一格式）；返回异步 operation id |
| **`service_update`** | `cluster, service, config` | 滚动更新 |
| **`service_scale`** `confirm→0` | `cluster, service, replicas` | replicas=0 必须 confirm（停服务），镜像 Worker 语义 |
| **`service_restart`** | `cluster, service` | 强制重建 tasks |
| **`service_remove`** `confirm` | `cluster, service` | |
| `service_logs` | `cluster, service, tail?`(默认 200，上限 1000), `since?` | 最近日志（一次性快照，非 SSE 流） |
| `service_operation` | `cluster, op_id` | 轮询 deploy/update/scale/remove 的收敛状态（status/replicas/error） |
| `service_audit` | `cluster, limit?` | 该集群 Worker 侧操作审计（workerproxy.Audit） |

部署闭环引导：`service_deploy` 返回体固定附一句 hint——"operation pending; poll service_operation until status=done|failed"。构建镜像后部署的联动见 §6.3。

### 5.4 镜像与构建（image_* / build_*，6 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `image_list` | `name?` | 仓库内 repo/tag 清单 |
| **`image_delete`** `confirm` | `name, tag` | |
| **`build_upload_begin`** | `filename, size_bytes` | 签发一次性上传票据 + 生成 curl 直传命令（见 §6.2） |
| **`build_submit`** | `upload_id, name, tag, dockerfile?`（zip 须先经 6.2 票据直传） | zip → server 本机 docker build → push 内嵌仓库；返回 build id |
| `build_get` | `id, logs_tail?`(默认 40 行) | 状态机 PENDING→EXTRACTING→BUILDING→PUSHING→CLEANING→SUCCESS/FAILED + 进度 + 日志尾部（构建失败时助手靠日志向用户解释原因） |
| `build_list` | `limit?` | 最近构建 |

### 5.5 告警（alert_*，4 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `alert_list` | `status?`(active/acked/recovered), `cluster?, level?, limit?` | 活跃告警是排查入口，返回体含 cluster/service/type/count/first/last |
| `alert_get` | `id` | 详情 + 关联事件时间线摘要 + 历史排查会话 |
| **`alert_ack`** | `id` | AckedBy = token 名 |
| **`alert_recover`** | `id` | 人工关闭 |

### 5.6 排查会话 / 双 agent 协同（investigation_*，4 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `investigation_list` | `alert_id?` | 排查会话列表（含页面发起与 MCP 委托两类） |
| **`investigation_start`** | `alert_id?` 或 `question, cluster?` | **委托内嵌 AiNexus 排查**：question 模式鼓励携带代码侧假设与关键上下文（≤8KB）；返回会话 id，异步执行（见 7.3） |
| `investigation_get` | `id` | 状态 running/done + 结论 + 工具调用证据尾部（供本机 agent 引用回代码分析） |
| **`investigation_continue`** | `id, message` | 同会话多轮追问，支撑「取证→对代码→再取证」的协同循环 |

### 5.7 诊断拨测（node_* / probe_*，4 个）

| 工具 | 参数 | 说明 |
|---|---|---|
| `node_list` | `cluster` | swarm 节点/角色/健康 |
| `node_stats` | `cluster, node?` | 节点维度容器 CPU/内存（node 空取全部聚合；实现复用 NodeStats + ListNodes 解析 hostname→nodeID） |
| `probe_port` | `cluster, host, port, node?` | 从指定节点（默认 manager leader）发起 TCP 拨测，返回各节点连通矩阵 |
| `probe_http` | `cluster, url, node?, expect_status?` | HTTP 拨测（复用 workerproxy.CheckHTTP，同节点解析） |

> `probe_flow`（多步事务拨测）与宿主机进程清单 `node_processes` **已随 P2 落地**（工具总数 41）。

### 5.8 工具分组小结

项目 5 / 集群 6 / 服务 10 / 镜像构建 6 / 告警 4 / 排查会话 4 / 诊断 6 / 巡检 8 / 告警规则 5 / 通知 4 / exec 透传 2，共 **60 个**；write scope 25 个、exec 专属 2 个、带 confirm 防护 7 个（patrol_delete / alertrule_delete 新增）。

---

## 六、镜像构建的文件上传设计（重点）

### 6.1 问题

MCP 工具入参是 JSON，zip 是二进制；平台 `registry.max_upload_mb` 默认 500MB，而实际构建包普遍数十 MB 起步（基本没有小包），base64 内联进工具调用只会同时撑爆助手上下文与服务端请求体。**结论：不做内联通道，统一走「上传票据 + curl 直传」**。

### 6.2 上传票据 + curl 直传（唯一通道）

1. `build_upload_begin{filename, size_bytes}` → 返回 `{upload_id, ticket}`；票据随机 32 字节 hex，**一次性、TTL 1 小时、上传体大小必须与声明的 size_bytes 一致才接受**（声明值即限额，封顶 `registry.max_upload_mb`）。
2. 助手用 shell 直传（不经 MCP 协议、不暴露长效 MCP token，票据泄露也仅能完成这一次已声明的上传）：

```bash
curl -sS -X POST "https://opsguard:8090/api/v1/mcp/build-upload?upload_id=<id>&ticket=<ticket>" \
     -H "Content-Type: application/octet-stream" --data-binary @gw.zip
```

3. `build_submit{upload_id, name, tag, dockerfile?}` 提交构建：直传体落临时 zip（zip-slip 校验路径与 REST 上传完全复用）→ `registry.Service.SubmitBuild(zipPath, name, tag, dockerfile)`，同一构建流水线，`build_get` 跟踪状态。

票据端点独立于 `/mcp`（挂 REST 路由组），自带票据校验中间件；未带票据/票据过期/重放/大小不符均返回明确错误。`build_submit` 收到的 `upload_id` 未完成直传时同样报错并附 curl 命令模板，助手可自助重试。

### 6.3 构建→部署的自然语言闭环

镜像引用规则直接写进 `service_deploy` 的工具描述（"本集群拉取镜像用 `registry.opsguard/<name>:<tag>` 或 `127.0.0.1:<port>/...`，经 Worker 中继免配置"），助手即可自主完成「构建完成 → 取 SUCCESS 返回的 image ref → 生成部署 config → deploy → 轮询 operation」的链路，无需人肉中转。附录 14.2 给出完整时序。

---

## 七、故障排查路径设计

### 7.1 分层诊断面（P1 全部只读 + 两个处置动作）

```
alert_list ──▶ alert_get（事件时间线）
                 │
                 ├─▶ service_logs ──▶ （日志证据）
                 ├─▶ node_stats / node_list ──▶ （资源/节点证据）
                 ├─▶ probe_port / probe_http ──▶ （主动复现/定位断点）
                 └─▶ 处置：service_restart / service_scale / alert_ack / alert_recover
```

设计意图：证据收集全只读（read scope token 也能完整排查），处置动作最小集（restart/scale/ack/recover），**删除类处置（remove）保留但 confirm 拦截**。只读快查与 7.3 的委托深挖是互补两档：前者秒级、零 LLM 成本；后者带领域技能与多轮集群取证。

### 7.2 与 Worker MCP 的分工

外部助手经由管理端工具排查时**不需要直连 Worker /mcp**（也连不通——Worker 在隔离集群内）。管理端工具是 Worker 能力的**路由聚合视图**（带 cluster 参数），exec 类高危能力刻意不透传（见 7.4）。若用户把内嵌 AiNexus 排查对话作为入口，那是另一条既有链路（网关 MCP 客户端 → Worker），与本方案互不干扰。

### 7.3 双 agent 协同排障（本 MCP 的核心价值，P1）

两个 agent 的上下文天然互补：**本机 agent 有代码上下文**（读仓库、定位嫌疑代码、改完直接构建镜像发版），**AiNexus 有集群实时证据与领域技能**（Worker MCP 20 工具 + 技能 playbook，但看不到代码）。此前这两侧没有通路——本 MCP 把它们接成「代码侧假设 ↔ 集群侧取证」的闭环：

```
本机 agent (ZCode/Claude…)                  AiNexus（内嵌专家代理）
  有代码上下文，能改代码发版          ▶    有集群证据与技能，无代码上下文
      │ 读代码形成假设                      │
      │ investigation_start{               │
      │   question:"怀疑新加的重试逻辑导致  │
      │   连接池耗尽，去 prod 确认 gateway  │
      │   当前连接数与最近日志/重启记录",   │
      │   cluster:"prod"}                  │
      ├────────────────────────────────────┤ 按技能流程调集群工具取证
      │ investigation_get{id}              │
      │   ◀── 结论 + 工具调用证据尾部       │
      │ 证据对上/推翻假设 → 改代码           │
      │ investigation_continue{id, 追问}   │（多轮往返）
      │ 改完代码 → build_upload_begin +     │
      │   curl 直传 + build_submit          │
      │ service_update 滚动更新 ────────────▶ 验证恢复
```

- **工具面**（P1 即具备，见 5.6）：`investigation_start`（alert_id 或自由 question——question 模式就是为携带代码侧假设与关键上下文设计的，≤8KB，建议传「现象 + 嫌疑代码摘要 + 请确认什么」三段式）/ `investigation_get`（结论 + 证据尾部，供本机 agent 引用回代码分析）/ `investigation_continue`（同会话追问）。
- **异步执行**：AI 排查分钟级，同步等会撞助手客户端超时。start 立即返回会话 id；后台 goroutine 消费网关 SSE 流，消息逐条写入 `store.Investigation`，完成时落 Conclusion；get 轮询（进行中返回 running + 已产出的中间证据）。
- **实现复用**：直接走 `ainexusrt` 网关（与 `POST /ainexus/chat|investigate` 同一引擎、同一技能注入与用量计量口径），MCP 层只做「发起—落库—轮询」编排，不新建推理链路；委托会话打 `source=mcp` 标记，前端排查会话列表可见。
- **依赖与降级**：需网关已配置可用模型；未配置时三个工具返回引导性错误（指向 MLOps 模型接入），不影响其余工具。
- **并发护栏**：委托排查占网关推理资源，信号量限并发（默认 4，`mcp.max_investigations` 可配），超限返回"排队中请稍后"而非静默堆积。
- **分工边界**：AiNexus 只做集群侧取证与结论，不改代码不发版；处置动作（restart/scale/update）仍由本机 agent 经管理端工具显式执行——职责清晰、全程可审计。

### 7.4 exec 透传（**已随 P3 落地**，三重防护）

Worker 的 `exec_in_container` / `exec_host_command` 是最高危能力。管理端透传方案（`tools_exec.go` 的 `service_exec` / `node_exec`）：

1. **配置开关**：`mcp.exec_enabled`（默认 false——关着时连 exec token 也被拒，错误信息指向配置项）；
2. **独立 exec scope**：token scope 三档化 `read < write < exec`，exec 工具要求 scope=exec（write/read 拒绝并给出解释性错误）；
3. **链路与约束不变**：管理端 →（进程内 MCP 客户端，复用网关的集群连接池 `Manager.CallServerTool`）→ 目标集群 Worker /mcp → 节点执行，**Worker 侧 commandPolicy 黑白名单与超时仍然生效**；命令原文入审计（`service_exec`/`node_exec` 审计摘要含命令）；`node_exec` 强制显式传 node（杜绝误 fan-out 全部节点）；结果 128KB 截断。

---

## 八、安全设计

| 面 | 措施 |
|---|---|
| 鉴权 | 命名 token、SHA-256 摘要入库、常数时间比较；平台无认证时 MCP 仍强制鉴权（4.1 硬性规则） |
| 授权 | read/write scope 工具级声明 + 中间件统一执行；错误文本可解释 |
| 破坏性操作 | `confirm=true` 双保险（cluster_remove / project_delete / service_remove / service_scale→0 / image_delete），confirm 缺失时错误文案**必须向助手说清影响面**（如"该集群下有 N 个服务"），由助手转述用户确认 |
| 审计 | 写工具全部落 `mcp_audit`（LevelDB，结构见 §九）+ slog；读工具仅 slog（对齐 Worker"只读探测不审计"的口径） |
| SSRF 面 | `cluster_add.worker_url`/`worker_http_url` 任意内网 URL 的探测（gRPC Self + HTTP /healthz）可被用作内网端口扫描——缓解：write scope 门槛 + 审计记录 + （P3 可选）worker_url/worker_http_url CIDR 白名单（两个端点都校验）；残余风险记录在案 |
| 体积防线 | 工具结果 128KB 头尾保留截断（复用 ainexus mcp_tool 截断策略）；logs tail≤1000、events limit≤100、build 日志尾部≤200 行；investigation question ≤8KB；构建包不经 MCP 协议传输（票据直传，见 §六） |
| 敏感值 | cluster token/secret 一律不回显（`has_token` 布尔）；`service_get` 的 config 原样返回（运维语义需要），但 config 中 env 密钥字段由 Worker 配置模型负责，不额外打码（与页面行为一致）；audit 记录入参摘要时对 `config/question` 大字段只记长度 |
| 传输 | 生产建议前置 TLS（现有部署形态不变，`/mcp` 与 REST 同域同证书） |

---

## 九、可观测

```go
// 审计记录（LevelDB bucket: mcp_audit/<seq>，seq 复用 store.Seq）
type MCPAudit struct {
    Seq     int64  `json:"seq"`
    TS      int64  `json:"ts"`
    Actor   string `json:"actor"`     // token 名
    Tool    string `json:"tool"`
    Args    string `json:"args"`      // 摘要（大字段只记长度），≤512 字符
    OK      bool   `json:"ok"`
    Error   string `json:"error,omitempty"`
    CostMS  int64  `json:"cost_ms"`
}
```

- **查询**：P2 admin API `GET /api/v1/mcp/audit?actor=&tool=&limit=`；P1 先 slog 检索。
- **计数**：token×tool 调用次数与错误率，内存聚合 + 定期落盘（`mcp_usage` 前缀），前端 P2 展示。
- **健康**：`GET /ainexus/health` 与 `/healthz` 附加 mcp 段（enabled/token 数/今日调用数）。
- **委托排查计量**：investigation_* 触发的推理照常进 MLOps 用量计量（scenario=chat/investigate），不新增口径；会话打 `source=mcp`，报表可区分助手委托与页面发起。

---

## 十、前端设计（**已随 P2 落地**）

系统设置新增 **「MCP 接入」** tab（admin 可见，与「AI 排查网关」「身份提供者」并列）：

- **总开关与端点信息**：`https://<host>/mcp` 一键复制；内置 ZCode / Claude Desktop / Cursor 三种客户端的接入配置 JSON 模板（含 Bearer header 占位），复制即用。
- **Token 管理**：列表（名称/scope/来源(config|页面)/启用/最近使用/创建时间）、新建（生成 secret 仅展示一次）、启停（热生效）、删除（静态种子提示改 config）。运行时表落 LevelDB（4.2）。
- **调用情况**：近 7 天 token×tool 调用次数表 + 最近 30 条写操作审计（审计数据源）。

实现：`web/src/views/system/McpAccess.vue`。

---

## 十一、测试要点

| 类别 | 用例 |
|---|---|
| 鉴权 | 无 token/错 token 401；read token 调写工具报可解释错误；平台认证关闭时 /mcp 仍 401；token 禁用即时生效 |
| scope/confirm | 五个 confirm 工具缺 confirm 全部拒绝且错误文案含影响面；scale→0 拦截；write token 全通 |
| 项目/集群 | project 重名拒绝；cluster_add 探活失败不落库且错误透传；cluster_remove 级联回调触发（订阅停止/规则清理）；token 空白沿用旧值 |
| 服务 | service_deploy 非法 config 报错定位行号；operation 轮询五种终态；service_logs tail/since 边界；read snapshot 与 svccfg 一致 |
| 构建 | 票据一次性（重放拒绝）/TTL 过期/大小与声明不符/超 max_upload_mb 全部拒绝；未带票据直传 401；upload_id 未完成直传时 build_submit 报错并附 curl 模板；build_get FAILED 时日志尾部可见；zip-slip 恶意包拒绝（复用既有校验用例） |
| 委托排查 | question 模式落库 source=mcp；>8KB 拒绝；get 进行中返回 running + 中间证据、完成后有结论与证据尾部；continue 复用既有会话上下文；网关未配置模型时三工具返回引导性错误且不影响其他工具；并发超限返回排队提示；后台执行不阻塞 /mcp 其他调用 |
| 告警/诊断 | alert_list 过滤组合；probe_http 非 2xx 判定；node 缺省聚合与指定节点一致 |
| 截断 | 128KB 截断头尾保留；logs/events/build 日志各自上限生效 |
| 审计 | 写工具全落库且 actor/tool/cost 正确；大字段只记长度；读工具无审计记录但有 slog |
| 回归 | /mcp 不影响既有路由（SPA NoRoute、REST、/v2、/ainexus）；工具 schema 快照测试（工具名/参数集变更显式更新快照，防止无意破坏助手侧提示词） |

---

## 十二、风险与对策

| 风险 | 对策 |
|---|---|
| 助手在 confirm 语义上「自作主张」传 true | Instructions + 工具描述双重声明"必须转述用户确认"；审计可追责到 token；高价值 token 建议发 read scope |
| 委托排查并发失控（多助手同时 start 挤占网关） | 信号量上限（`mcp.max_investigations`，默认 4）+ 超限排队提示；用量照常计量，成本可见 |
| AiNexus 未配置/模型故障导致协同排障不可用 | 三工具显式降级报错并引导配置；其余工具零影响 |
| 工具描述质量差导致助手选错工具/漏 cluster 参数 | 每工具描述写清"何时用/前置工具/下一步"；上线后按实际对话失败样本迭代描述（低成本，改字符串即可） |
| MCP 工具面与 REST 演进不同步（新 API 忘了加工具） | 工具层薄封装 + 文档声明"MCP 面只覆盖 P1 清单"；新增域（patrol/alertrule/notify）按 P2 表推进，不在本方案外私加 |
| server 进程暴露面扩大（外部助手网络可达） | token 鉴权强制 + 建议 TLS；/mcp 路径纳入既有安全公告；必要时配 `server.addr` 绑定与前置反代限源 |
| 双 MCP 库（mcp-go 客户端 + go-sdk 服务端）依赖漂移 | 各自锁版本；升级跟随 Worker 的 go-sdk 版本走，同升同测 |

---

## 十三、分期实施

| 阶段 | 范围 | 主要改动点 |
|---|---|---|
| **P1 核心 39 工具** | `/mcp` 端点 + 静态 token 鉴权/scope + 七组 39 工具（项目 5、集群 6、服务 10、镜像构建 6、告警 4、排查会话 4 含双 agent 委托排查、诊断 4）+ confirm/审计/截断 + 构建票据直传（唯一通道） | `internal/mcpserver/`（新包）、`router.go`（挂载+public 豁免+票据直传端点）、`config`（mcp 段）、go.mod 增 go-sdk、`ainexusrt`/`api`（SSE 消费落库编排） |
| **P2 运营与增强** | **已落地**：token 管理 API + 前端「MCP 接入」tab（LevelDB 运行时表，静态种子镜像/禁用/热生效）+ 用量统计（/mcp 中间件计数，30s 落盘日聚合）+ 审计查询 + probe_flow / node_processes。exec 透传顺延 P3（需 workerproxy/proto 扩展，且默认关） | `mcpserver/tokens.go`、`usage.go`、`store/mcptoken.go`、`api/mcp.go`、前端 `McpAccess.vue` |
| **P3 深化** | **已全部落地**：patrol 域 8 工具（list/get/create/update/delete/run/runs/report，自然语言建巡检）+ alertrule 域 5 工具（save 默认带 apply 下发）+ notify 域 4 工具（channels/channel_save/policies/records）+ exec 透传 2 工具（`mcp.exec_enabled` 默认关 + scope=exec 三档化 + Worker commandPolicy 仍生效 + 全量审计）+ worker_url CIDR 白名单（`mcp.cluster_url_allow_cidrs`）+ **MCP resources**（opsguard://clusters、alerts/active、patrols、images + clusters/{name}/services 模板）+ **OAuth 2.1**（`/.well-known/oauth-protected-resource` RFC 9728 元数据公开端点；静态 token 未命中时回退校验内嵌 IdP access token——验签+iss/exp+吊销与 Introspect 同口径，scope 按 mcp:read/write/exec 映射，默认 read 最小授权） | `tools_patrol.go`、`tools_alertrule.go`、`tools_notify.go`、`tools_exec.go`、`tools_resources.go`、`oauth.go`、`allowlist.go`、ainexus `Manager.CallServerTool`/`Server.CallClusterTool`、`idp.Service.ValidateAccessToken` |

依赖关系：P2 依赖 P1；P3 的 OAuth 依赖 P2 的 token 体系收敛。

---

## 十四、附录

### 14.1 助手接入配置示例

ZCode（`.zcode/mcp.json` 或等价配置）：

```json
{
  "mcpServers": {
    "opsguard": {
      "type": "streamableHttp",
      "url": "http://10.0.0.10:8090/mcp",
      "headers": { "Authorization": "Bearer 9f86d081884c7d65..." }
    }
  }
}
```

Claude Desktop / Cursor 同构（`mcpServers.opsguard` + url/headers），模板随 P2 前端一键复制。

### 14.2 关键时序：一句自然语言完成「纳管→构建→部署」

```
用户: "把 10.0.1.5 纳管为 prod 集群（gRPC 9080 / HTTP 8080），用 gw.zip 构建镜像 gw:v1，部署 2 副本对外 8080"
  │
  ├─ cluster_add{name:prod, worker_url:10.0.1.5:9080, worker_http_url:http://10.0.1.5:8080, token:…}
  │     └─ service 探活 gRPC Self ✅ + HTTP /healthz ✅ → 落库（审计: cluster_add by zcode）
  ├─ build_upload_begin{filename:gw.zip, size} → ticket
  │     └─ curl 票据直传（6.2，唯一通道）→ upload ok
  ├─ build_submit{upload_id, name:gw, tag:v1}
  │     └─ EXTRACTING→BUILDING→PUSHING→SUCCESS，image=registry.opsguard/gw:v1
  ├─ service_deploy{cluster:prod, config:|
  │     image: registry.opsguard/gw:v1
  │     service: {name: gateway, replicas: 2, ports: ["8080:8080"]}…}
  │     └─ operation pending
  └─ service_operation{op_id} ×N → done(healthy)
        └─ 回复用户：集群已纳管、镜像已构建、服务已上线（全程 4 工具 + 1 直传，均带审计）
```

### 14.3 关键时序：双 agent 协同排障（7.3 的价值落地）

```
用户对本机 agent: "prod 的 gateway 一直 OOM 重启，是不是我们上周改的缓存代码引起的？"
  │
  ├─ 本机 agent：读仓库定位上周改动（连接池/缓存模块），形成假设
  ├─ investigation_start{cluster:"prod", question:"现象：gateway 周期性 OOMKilled 重启。
  │    嫌疑：上周 v1.4.2 新增的本地缓存未设上限（代码摘要…）。请确认：容器内存曲线
  │    与 limit 差值、OOM 前日志尾部、是否 v1.4.2 起才出现"}
  │     └─ AiNexus 按技能流程取证：get_resource_usage / get_service_logs / get_events
  ├─ investigation_get{id} → running（返回中间证据）→ done
  │     └─ 结论：v1.4.2 起内存爬升曲线吻合，OOM 前日志有 cache miss 风暴
  ├─ 本机 agent：对代码确认缓存无淘汰策略 → 修复 + code review
  ├─ investigation_continue{id, "已修复为 LRU+上限 1024，请继续观察 prod 内存趋势"}
  │     └─ AiNexus 持续观测（本机 agent 同时改代码）
  ├─ build_upload_begin + curl 直传 gw-v1.4.3.zip → build_submit → SUCCESS
  └─ service_update{cluster:"prod", config(镜像 v1.4.3)} → service_operation done
        └─ 回复用户：根因（代码+集群证据双确认）、已修复发版、内存曲线回落
```

两侧各司其职：代码上下文与修复在本机，集群取证与验证在 AiNexus——这条通路此前只能靠人肉在两边搬运信息。
```
