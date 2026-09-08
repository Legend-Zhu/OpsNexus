# OpsGaurdWeb 集群管理端设计方案

> 版本：v0.1（草案）
> 日期：2026-08-04
> 状态：原型分析完成，方案待评审（历史方案文档；2026-08-21 已按当前代码复核修正明显漂移，实现现状以《OpsGaurd-系统技术总览.md》为准）
> 关联：`Worker/`（集群侧 Agent，swarm 编排+监控+MCP）、`AiNexus/`（AI 对话网关/Agent）、`OpsGaurdWeb/Prototype/OpsGuard-prototype-v2.html`（UI 原型）

## 一、原型分析

### 1.1 原型是什么

`OpsGuard-prototype-v2.html` 是一个**单文件纯前端原型**（无后端，全内存 mock 数据），完整演示了"智能运维监控平台"的目标 UI 与交互。深色主题、左侧导航 + 顶栏面包屑 + 内容区布局。

### 1.2 信息架构（侧边栏 8 组 16 页）

| 分组 | 页面 | 核心内容 |
|---|---|---|
| **概览** | 运维态势 | 统计卡（项目/在线服务器/应用/活跃告警）+ 服务器健康概览 + 最近告警时间线 + 今日巡检概览 |
| **资源管理** | 项目管理 | 项目 CRUD（项目→服务器→应用→中间件的层级组织） |
| | 服务器管理 | 列表（状态/IP/类型/所属项目/CPU/内存/磁盘/Agent）+ 搜索筛选 + 添加服务器（SSH 端口/认证）→ 引导装 Agent |
| | 服务器详情 | 资源卡 + 部署应用 + 运行中间件 + 本机告警 + 基本信息 + **SSH 终端入口** + Agent 安装向导 |
| | 应用服务管理 | JAR 包注册（类型/端口/路径/JVM 参数）+ 运行/停止/异常 + 详情（堆内存/GC/线程/HTTP 5xx） |
| | 中间件管理 | Redis/MySQL/Kafka/Nginx 卡片（连接数/内存/可用率） |
| **监控告警** | 实时监控 | CPU/内存柱状图 + JAR 进程状态表 |
| | 告警中心 | 告警列表（级别/来源/服务器/次数）+ 认领/屏蔽 + **AI 深度排查**（证据链 MCP 采集 + LLM 根因分析 + 建议）+ 告警规则 |
| **智能巡检** | 巡检编排 | **YAML 声明式流程**（probe 阶段不调 LLM → condition 异常才调 LLM → 严重告警深度排查）+ 执行流程可视化 |
| | 定时调度 | 内置调度引擎（不依赖 XXL-Job），cron/频率/立即执行/执行记录 |
| | 巡检报告 | AI 摘要报告（LLM 生成） |
| **AI 智能体** | 智能助手 | 对话界面（MCP 工具调用展示、根因分析、操作执行、通知闭环） |
| | LLM 策略 | **分层调 LLM**（探测不调/告警 qwen-plus/深度排查 gpt-4o+MCP/报告 qwen-plus）+ 模型配置表 |
| **通知&系统** | 通知管理 | 渠道（飞书/短信/钉钉/企微/邮件）+ 策略（级别→渠道→接收人→升级）+ 发送记录 |
| | 系统设置 | 用户管理（超管/运维/研发负责人）+ Agent 配置（采集间隔/上报地址/日志保留/SSH 超时） |

### 1.3 核心交互流程

1. **登录** → 工作台（原型 admin/admin123）
2. **纳管一台服务器**：添加服务器（IP/SSH/认证）→ 弹 Agent 安装向导（复制一键安装命令 `curl ... | bash`）→ Agent 注册（WebSocket）→ 验证"已安装·在线"→ 指标开始上报
3. **告警 → AI 深度排查**：告警列表点"深度排查" → MCP 工具采集证据链（top/jstack/GC 日志/日志检索）→ LLM 根因分析 + 因果链 + 分层建议 → 通知
4. **巡检闭环**：YAML 编排（探测→告警→深度排查→报告）→ 内置定时调度 → 异常调 LLM 生成告警/报告 → 飞书/短信通知
5. **智能助手**：自然语言提问 → Agent 调 MCP 工具排查 → 展示证据 → 根因 → 建议 → 可执行操作 → 通知

### 1.4 原型数据模型

```
Project(项目) 1—* Server(服务器: IP/类型/CPU/内存/磁盘/Agent状态) 1—* App(JAR: 端口/路径/堆/GC/线程/5xx)
                                          └—* Middleware(Redis/MySQL/Kafka/Nginx: 连接数/内存/可用率)
Alert(告警: 级别/来源/服务器/状态/次数) · User(角色) · Schedule(cron) · LLMConfig(场景→模型)
```

### 1.5 关键设计亮点（方案必须继承）

1. **分层调 LLM 策略**：探测阶段纯工具+规则**不调 LLM**，异常才调轻量模型（qwen-plus 生成告警），严重告警才调强模型（gpt-4o + MCP 深度排查），汇总才调 LLM——成本可控，是原型的核心思想。
2. **AI 深度排查 = MCP 证据链 + LLM 根因分析**：Agent 的 MCP 工具（top/jstack/GC/日志检索）采集证据 → LLM 分析因果链 → 给出分层建议。
3. **Agent 安装向导**：一键安装命令 + 步骤化引导 + 自检方法 + 失败排查，降低纳管门槛。
4. **YAML 声明式巡检**：流程定义与执行分离，探测/告警/排查/报告分阶段。
5. **内置调度引擎**：不依赖外部 XXL-Job。
6. **通知升级策略**：级别→渠道→接收人→未处理升级。

### 1.6 与现有资产的关系（关键差异）

| 维度 | 原型视角 | 现有 Worker 视角 | 统一方式 |
|---|---|---|---|
| 纳管对象 | 裸机服务器 + JAR 进程 + 中间件 | **Docker Swarm 集群**（节点 + 服务/容器） | 管理端顶层为**集群**（类 Rancher），集群内提供**容器视图**（Worker 提供）|
| Agent | 裸机采集程序（9100 端口，采集 CPU/内存/磁盘/进程） | Worker 容器（挂 docker.sock：容器 stats/端口/日志/命令执行/MCP） | **v1 以 Worker 为 Agent**；裸机 Agent 作为后续扩展（Worker 已有 nsenter 宿主机能力可覆盖部分）|
| 资源指标 | 服务器 CPU/内存/磁盘 | 容器 stats + 节点资源（Worker `/local/stats`） | 节点维度指标由 Worker 聚合提供 |
| AI 排查 | 假设有 Agent 提供 MCP 工具 | **Worker 自带 /mcp（20 工具）** | **闭环：内嵌 AiNexus 作为 MCP 客户端连接 Worker 的 /mcp**，对话/排查端点由管理端进程内直调 |
| LLM 网关 | 原型假设直连各模型 | — | **AiNexus 已是现成的多模型路由网关**（OpenAI/Anthropic 双格式 + 多 Provider） |

> **结论**：原型给出的是**产品形态与信息架构**；底层资源模型以**集群（swarm）+ Worker** 实现，AI 能力由 **AiNexus** 承载（vendor 进后端单进程）。两者通过"前端 → 管理端（内嵌 AiNexus）→ MCP → Worker（采集/操作）"形成闭环。

---

## 二、管理端定位与技术架构

### 2.1 定位

OpsGaurdWeb 是**多集群管理控制台**（类 Rancher）：纳管多个 swarm 集群（每个集群一个 Worker），提供资源管理、监控告警、智能巡检、AI 排查、通知与系统管理。AiNexus 的代码 **vendor 进后端进程**（单进程承载管理 API + AI 网关），负责全部 LLM 交互。

### 2.2 总体架构

```
┌──────────────────────── OpsGaurdWeb 管理端（单进程） ─────────────────┐
│  前端（Vue3 + Element Plus）                                           │
│  仪表盘│集群管理│工作负载│监控告警│智能巡检│AI 助手│通知│系统             │
│        │                    │                     │                  │
│   axios/SSE ────────────────┼─────────────────────┘                  │
└────────┼────────────────────┼────────────────────────────────────────┘
         │ /api/v1/clusters   │ /api/v1/ainexus/chat (SSE，进程内)
┌────────▼────────────────────▼──────────────────────────────┐
│ 后端 Go+Gin                                                 │
│ ├─ 管理面：集群注册表 / Worker 代理客户端 / 告警 ingest        │
│ └─ AiNexus 内嵌网关（vendor 自 ./AiNexus，同进程）              │
│    多模型路由 + ReAct Agent + MCP 客户端 ──MCP(stdio/SSE/HTTP)─┼──▶ Worker /mcp
│    （无独立端口、无独立鉴权，原生端点挂载 /ainexus/*）            │
└─────────────────────────────────────────────────────────────┘
         │ 管理（Worker gRPC ManagementService） + MCP（HTTP /mcp，AiNexus 调用）
┌────────▼──────────────────────────────┐
│ 集群 1..N：Worker（swarm manager）       │
│  ├─ 编排/管理 RPC（Self/Deploy/...）    │
│  ├─ 监控 SubscribeEvents / SubscribeAudit 双向流
│  └─ MCP /mcp（供内嵌 AiNexus 调用）       │
└─────────────────────────────────────────┘
```

### 2.3 技术栈（骨架已搭，`OpsGaurdWeb/`）

| 层 | 选型 | 状态 |
|---|---|---|
| 前端 | Vue3 + TypeScript + Vite + Element Plus + Pinia + Vue Router | ✅ 骨架（路由/布局/4 占位页/api 骨架） |
| 后端 | Go + Gin | ✅ 骨架（clusters/workloads/events/audit/ainexus 路由占位 + response 封装） |
| AI | AiNexus（Go+gin，多模型路由 + MCP 客户端） | 代码 vendor 进后端单进程，待整合 |
| 集群侧 | Worker（swarm 编排/监控/MCP） | ✅ 已投产（真机双节点验证） |

---

## 三、前端信息架构（原型 → 管理端映射）

| 原型页面 | 管理端模块 | 路由 | 数据来源 |
|---|---|---|---|
| 运维态势 | 仪表盘 | `/dashboard` | 各集群 Worker 聚合（节点/服务/事件/审计） |
| 项目管理 | 项目（管理层级第一层） | `/projects` | 管理端项目表 |
| 项目管理 | 集群管理（类 Rancher） | `/clusters` | 后端集群注册表 |
| 项目-服务器 | 集群详情（节点/服务/进程） | `/clusters/:name` | gRPC `ListServices`/`ListNodes`/`NodeStats`/`NodeProcesses`（经 workerproxy） |
| 服务器详情 | 集群资源详情（节点+容器+操作） | `/clusters/:name`（详情抽屉） | Worker 详情/exec/日志 |
| 应用服务管理 | 工作负载（swarm 服务） | `/clusters/:name`（收编） | gRPC `ListServices` 等（部署/缩放/重启；主动回滚未实现） |
| 中间件管理 | 节点探测覆盖（端口/HTTP/进程 filter） | `/clusters/:name`（节点抽屉） | gRPC `CheckPort`/`CheckHTTP`/`NodeProcesses` |
| 实时监控 | 集群监控（节点资源/服务健康） | `/clusters/:name`（收编） | gRPC `NodeStats`/`WatchNodeStats` + 事件 |
| 告警中心 | 告警中心（已排查标记 + 一键跳转排查） | `/alerts` | gRPC SubscribeEvents 已订阅入管理端的事件 + investigation 回写 |
| 巡检编排/调度/报告 | 智能巡检（YAML 六类检查 + 内置调度 + 报告 + 异常转告警） | `/patrol` | 管理端存储 + 调度引擎 + AiNexus 生成 |
| 智能助手/LLM 策略 | 对话式排查（选告警/自由提问/多轮/落库） | `/troubleshoot` | AiNexus `/chat`（SSE + alert_id 证据注入） |
| —（新增） | 镜像仓库（内嵌 registry + 上传构建 + 镜像列表） | `/registry` | 管理端 registry（/v2 + 构建任务） |
| 通知管理 | 通知中心（渠道/策略/记录） | `/notify` | 管理端（对接 Worker webhook 出口 + 飞书/短信） |
| —（新增） | MLOps（模型接入/提示词/模型/用量费用） | `/mlops` | 管理端（内嵌网关运营层；模型接入为网关基础能力，不受 mlops 开关影响） |
| 系统设置 | 系统设置（用户/SSO/身份提供者/AI 排查网关/密钥） | `/system` | 管理端 |

> 注：工作负载/监控已收编进集群详情页（/workloads、/monitor 重定向 /clusters）。

---

## 四、后端模块与 API

### 4.1 模块划分

```
server/internal/
├── api/            # handlers（clusters/workloads/events/audit/patrol/notify/settings/registry/investigations）
├── config/         # 集群注册表 + AiNexus 内嵌配置 + registry 配置 + store 路径
├── router/         # 路由（含 ainexus 组、/v2 registry 协议端点）
├── ainexus/        # 【内嵌】AiNexus 网关代码（providers/tools/mcp/agent/handler/server 全套，
│                   #        单进程运行；AddMCPCluster 动态连接集群 Worker MCP 采证；
│                   #        MCP headers 透传 + 工具名清洗为合法 function name，✅ P4/P7）
├── cluster/        # 集群管理：注册表 CRUD + Worker 连接状态探测 + 告警查询 + 排查会话（✅ P1/P3）
├── ingest/         # gRPC 事件订阅入口：subscriber.go（每集群一个 SubscribeEvents 订阅者，drain
│                   #   pb.MonitorEvent → store.IngestEvent → HandleEvent 落库+告警聚合 → 按 seq ACK）
│                   #   + manager.go（订阅者 add/remove 生命周期）+ store/cursor.go（游标持久化，断线续传）（✅ P3）
├── workerproxy/    # Worker gRPC 代理客户端：拨号 Worker 的 ManagementService，调用 Self/Events/Deploy/
│                   #   NodeStats/CheckPort/CheckHTTP/进程 filter + SubscribeEvents/SubscribeAudit 双向流
│                   #   （公开方法签名不变，上层 api/patrol/alertrule/investigate 无感）（✅ P1/P2/P3/P8）
├── patrol/         # 巡检：YAML 流程（resource/health/port/http/process/flow 六类检查）+ 内置调度
│                   #   + AI 报告 + 报告渠道投递 + 异常转告警闭环（✅ P5/P8）
├── notify/         # 通知：渠道（可配置不预设）/策略/记录 + 互联网代理转发 + 通用 Send（✅ P6/P8）
├── alertrule/      # 告警规则：管理 Worker monitoring config（决策⑦）+ 下发（✅ P6）
│                   #   + 纳管对象下发（写入 InventoryItem.Monitoring）+ 清单↔规则双向同步
├── invmonitor/     # 纳管对象监控调度器：对 inventory 条目的 monitoring 做 server 侧周期探测
│                   #   （CheckPort/CheckHTTP/NodeContainers，状态翻转 → ingest 聚合成告警）
├── auth/           # 认证：本地用户（加盐哈希 + HMAC token）+ OIDC/SSO 抽象 + 中间件（✅ P6）
├── registry/       # 内嵌镜像仓库：OCI /v2（pull/push/catalog/tags/delete，basic auth）
│                   #   + 页面传包构建（docker CLI，任务状态机）+ 保留策略/GC（✅ P9）
└── store/          # 持久化（LevelDB/goleveldb 嵌入式 KV：集群/事件/告警/巡检/通知/用户
                    #   + settings/investigation/ainexus 桶 + 索引 + 序列 + 迁移）
```

> 注：以上为 v0.1 模块划分快照；后续新增 `idp/`、`idptunnel/`、`mlops/`、`api/project.go`/`inventory.go`/`secret.go` 及 store 多个桶文件未逐一列入，全量见《OpsGaurd-系统技术总览.md》§4.1。

### 4.2 API 规划（骨架基础上扩展）

```
# 集群管理（已有骨架，补业务）
GET/POST    /api/v1/clusters                    # 列表 / 接入（Worker URL+token+名称）
GET/DELETE  /api/v1/clusters/:name              # 详情（健康探测）/ 移除
GET         /api/v1/clusters/:name/nodes        # 节点列表（gRPC ListNodes）
GET         /api/v1/clusters/:name/nodes/:id/processes   # 节点进程（top/limit/filter）

# 工作负载（经 Worker 代理）
GET         /api/v1/clusters/:name/workloads            # gRPC ListServices
GET         /api/v1/clusters/:name/workloads/:service   # 详情+tasks+健康
POST        /api/v1/clusters/:name/workloads            # 部署（config → Worker）
POST        /api/v1/clusters/:name/workloads/:service/scale|restart   # rollback 未实现
DELETE      /api/v1/clusters/:name/workloads/:service
GET         /api/v1/clusters/:name/workloads/:service/logs   # SSE 流式（gRPC StreamLogs 透传）

# 监控告警（经 Worker）
GET         /api/v1/clusters/:name/events       # 查询已入管理端的事件（由 gRPC SubscribeEvents 订阅落库）
GET         /api/v1/clusters/:name/audit        # gRPC ListAudit
GET         /api/v1/clusters/:name/metrics      # 节点资源聚合（gRPC NodeStats）

# 告警规则（集群级监控配置管理；service = swarm 服务名或纳管对象名）
GET         /api/v1/alertrules[?cluster=]       # 规则列表（可按集群过滤，集群详情页「告警规则」tab）
PUT         /api/v1/alertrules                  # 保存规则（校验 monitoring）
POST        /api/v1/alertrules/apply            # 下发：swarm → 合并进服务 config 推 Worker Update；
                                                #       纳管对象 → 写入 InventoryItem.Monitoring，server 侧
                                                #       invmonitor 调度器周期探测（CheckPort/CheckHTTP/容器存活，
                                                #       状态翻转经 ingest 聚合成告警，恢复自动关闭）
DELETE      /api/v1/alertrules/:cluster/:service

# AiNexus 整合（内嵌网关，不单独起服务）
GET         /api/v1/ainexus/health                # 内嵌网关健康（providers/models/tools/mcp）
GET         /api/v1/ainexus/models                # 模型列表（模型选择器）
POST        /api/v1/ainexus/chat                  # 对话式排查（SSE；扩展 alert_id/use_mcp：证据前缀注入+多轮追问）
POST        /api/v1/ainexus/investigate           # 深度排查：告警 → 拼上下文 → 内嵌 Agent（一次性）
GET/PUT     /api/v1/ainexus/config                # 网关配置（页面管理，保存即热重载）
# 内嵌网关原生端点（挂载 /ainexus，兼容 AiNexus 自身 URL 契约）
GET         /ainexus/health | /ainexus/api/models | /ainexus/api/tools | /ainexus/api/mcp
POST        /ainexus/v1/chat/completions | /ainexus/v1/messages
GET         /ainexus/v1/models

# 排查会话（对话式 troubleshoot 落库，关联告警回写「已排查」）
POST        /api/v1/investigations                # 保存（带 alert_id 回写告警标记）
PUT         /api/v1/investigations/:id            # 追问后更新同一条
GET         /api/v1/investigations/:id            # 详情
GET         /api/v1/alerts/:id/investigations     # 按告警列出历史排查

# 巡检（YAML 流程：resource/health/port/http/process/flow 六类检查）
GET/POST    /api/v1/patrols                     # YAML 流程 CRUD + 校验
POST        /api/v1/patrols/:id/run             # 立即执行
GET         /api/v1/patrols/:id/runs            # 执行记录
GET         /api/v1/patrols/:id/reports         # 报告
GET/PUT     /api/v1/settings/patrol-report      # 报告投递策略（每次/仅异常/关闭 + 渠道）

# 内嵌镜像仓库（管理 API；/v2 协议端点见下）
GET         /api/v1/registry/info               # 服务信息（hostname/docker 可用性/保留策略）
POST        /api/v1/registry/builds             # 上传 zip 构建（multipart/octet-stream）
GET         /api/v1/registry/builds[/:id]       # 构建任务轮询（状态/进度/步骤日志）
GET         /api/v1/registry/images             # 镜像列表（repo+tags+大小+更新时间）
DELETE      /api/v1/registry/images/*ref        # 删除 tag（<name>/tags/<tag>）

# OCI 镜像仓库协议端点（docker CLI 直连，独立 basic auth，不走会话认证）
*           /v2/*                               # pull/push/uploads/catalog/tags/delete

# 通知/系统（新增）
GET/POST    /api/v1/notify/channels             # 渠道（飞书/短信/...）
GET         /api/v1/notify/policies             # 策略（按级别）
PUT         /api/v1/notify/policies/:level      # 策略保存
GET         /api/v1/notify/records
GET/POST    /api/v1/users                       # 用户管理
```

---

## 五、数据来源与集成（核心闭环）

### 5.1 集群接入（类 Rancher）

管理端 `clusters` 注册表保存：集群名、Worker 的 gRPC 地址（swarm manager 节点）、可选 MCP URL + Worker token。接入时经 `workerproxy` 拨号 Worker 的 gRPC `ManagementService` 并调 `Self`（确认 manager 角色 + 连通性），保存后即可代理其全部能力。

### 5.2 Worker 代理（gRPC）

后端 `workerproxy` 封装对 Worker 的调用（拨号 Worker 的 gRPC `ManagementService`，带 token、超时、错误归一），为前端提供统一数据。公开方法签名（Self/Events/Deploy/NodeStats/CheckPort/...）保持不变，上层 `api/`、`patrol/`、`alertrule/`、`investigate/` 无感——内部由原 REST 调用切换为 `pb.ManagementServiceClient` RPC。**事件入口也已由 webhook 切换为 gRPC 订阅**：`ingest.Subscriber`（每集群一个）通过 `SubscribeEvents` 双向流主动拉取 Worker 的监控事件（`SubscribeAudit` 同理取审计），drain `pb.MonitorEvent` → 转 `store.IngestEvent` → 复用 `ingest.Service.HandleEvent` 落库 + 告警聚合 → 按 seq ACK，并把消费游标持久化到 LevelDB（`store.PutCursor`/`GetCursor`），断线后按游标续传。`ingest.Manager` 负责按集群 add/remove 订阅者并托管其生命周期，重连采用有界退避。

> **网络策略背景**：部署环境仅放行 **server→worker** 方向。gRPC `SubscribeEvents` 流由管理端（允许方向）主动建立，Worker 在同一条连接上把事件回送——在不放宽网络策略的前提下实现了事件推送语义。原 webhook 入口（`POST /api/v1/ingest/events`、`ingest.ParseEvent`/`ValidateToken`/`LimitReader`、`IngestToken` 配置项、Worker 侧 `WebhookPusher`）已随迁移移除。

### 5.3 AiNexus 整合（内嵌进后端，单进程）

- **形态**：AiNexus 的 Go 代码作为 `server/internal/ainexus/` **vendor 进管理端后端模块**（Go `internal` 可见性规则下不可跨模块 import，故为代码级集成而非 `replace` 依赖）。后端进程内同时持有管理 API 与 AI 网关：AiNexus 的 providers/tools/MCP Manager/ReAct Agent 全部在同一进程，`/ainexus/*` 原生端点直接挂载进 gin 路由，不再有独立监听端口、独立鉴权、进程间 HTTP。
- **对话/排查**：前端 AI 助手 → 后端 `/ainexus/chat` → **进程内调用** AiNexus OpenAI handler（gin Context 直接传入，SSE 写回同一响应流），无中间 HTTP 跳转。
- **深度排查**：管理端把告警上下文（相关事件、服务日志、审计记录）注入 prompt → 内嵌 ReAct Agent 调 MCP 工具 → **AiNexus 的 MCP 客户端连接该集群 Worker 的 `/mcp`**，由 Worker 的 20 工具（get_service_logs/get_events/exec_host_command/...）实际采集证据 → LLM 根因分析。**MCP 集成（stdio/SSE/Streamable HTTP 三种传输）与 Worker /mcp 的对接逻辑原样复用，仅传输 URL 走内网。**
- **分层调 LLM**：AiNexus 多模型路由天然支持"告警用轻量模型、深度排查用强模型"——管理端在请求时按场景指定 model。
- **鉴权收敛**：AiNexus 网关自身的 APIKey 校验在嵌入后**关闭**（由管理端统一鉴权中间件覆盖 `/api/v1/ainexus/*`），避免双重认证。
- **上下文管理（长排查防爆）**：预算默认对齐深度排查模型 **1M 窗口**（`agent.max_context_tokens`），超预算时**摘要式压缩**（类 Trae Memory）：旧 ReAct 轮次经内嵌 LLM 压缩成一条 `[历史摘要]`（按 根因/关键操作/关键观测/待办 分类聚合），保留最近 `keep_tool_rounds` 个工具轮次完整原文；压缩失败/未配置 LLM 退化为按完整轮次硬删（tool/assistant 配对原子，防 400）。硬限大幅放宽：工具结果单次 128KB、流式 chunk 32K rune、investigate 证据 256KB——1M 窗口下以信息密度而非省空间为目标。

### 5.4 通知渠道与互联网代理（内网部署约束）

管理端部署在**内网**，无法直连公网渠道 API（飞书 Open API、短信服务商）。通知链路：

```
┌─ 内网 ─────────────────┐        ┌─ 互联网 ─────────────────────┐
│ 管理端 notify 模块       │        │ 转发代理（独立部署在公网可达   │
│  └─ 渠道(可配置)         │──HTTPS─▶│  的互联网服务器上，如某跳板机)  │
│     · 飞书 Webhook      │ 经代理  │  └─▶ 飞书 Open API          │
│     · 短信(阿里云/腾讯云) │        │  └─▶ 短信服务商 API           │
│     · 钉钉/企微/邮件(扩展) │        │  代理凭据(飞书 token/短信密钥) │
└─────────────────────────┘        │  仅存于代理侧，不入内网库      │
                                   └──────────────────────────────┘
```

- **渠道可配置不预设**：管理端 `NotifyChannel` 表保存渠道定义（type/name/config/via_proxy/enabled），用户自建飞书或短信渠道。
- **via_proxy 标记**：需访问公网的渠道一律经代理转发；代理是独立小服务（HTTP 转发，接收内网请求后带自身凭据调公网 API），**公网凭据不进入内网管理端**。
- **v1 必支持**：飞书（机器人 Webhook）与短信（至少一家服务商），经代理链路。

### 5.5 闭环链路（2026-08-05 全部打通）

```
① Worker 监控事件 ──gRPC SubscribeEvents（server 主动建流，Worker 回送）──▶ ingest(订阅+游标+ACK) ──▶ 告警聚合 ──▶ notify 按级别策略投递渠道
② 巡检 cron/手动 ──▶ 六类检查(resource/health/port/http/process/flow)──▶ 异常
     ├──▶ patrol_failed 告警(按 检查项|节点 细分,新增/复发才通知,恢复自动关闭)
     └──▶ AI 报告(每次 Run 结束生成)──▶ 按 settings/patrol-report 投递渠道(每次/仅异常/关闭)
③ 告警 ──▶ 对话式排查(troubleshoot 页:选告警 or 自由提问,多轮追问,
     服务端注入事件/日志/审计证据前缀,可选 MCP 采证)
     └──▶ 会话自动落库(investigation)──▶ 回写告警「已排查×N」──▶ 告警页一键跳转
④ 页面传 zip ──▶ 管理端 docker build ──▶ push 内嵌 registry(127.0.0.1 loopback)
     ──▶ 集群节点 docker pull(统一主机名 + insecure-registries)──▶ 部署上线
```

### 5.6 内嵌镜像仓库与构建（P9，双网段统一寻址）

- **形态**：OCI registry 以 `/v2` 协议端点内嵌进管理端进程（与管理台同端口），文件存储（blobs content-addressable + manifests/tags link，原子写入），不经 LevelDB；支持 pull/push/catalog/tags/delete，manifest content-type 原样存取（docker schema2 / OCI / image index 透明，多架构 manifest 可存可拉）。
- **认证**：/v2 走独立 HTTP Basic（`registry.users`，bcrypt 或 `{PLAIN}`），不走管理端会话认证；管理端构建 push 用 `registry.builder` 账号（启动时 docker login 一次）。空用户表 = 内网全信放行。
- **构建（参考 CI build-api 模式）**：页面上传 zip（Dockerfile + 编译产物，≤500MB，名称白名单防注入）→ 管理端解压（zip-slip 防护、.sh 自动 +x、Dockerfile 目录为 context）→ `docker build` → `docker push 127.0.0.1:<port>`（**loopback 免 HTTPS，管理端 dockerd 零配置**）→ 清理。任务状态机 PENDING→EXTRACTING→BUILDING→PUSHING→CLEANING→SUCCESS/FAILED + 进度 + 步骤日志，页面轮询。
- **双网段统一寻址（10 内网 / 172 政务外网）**：镜像名内嵌 registry 地址，网段割裂会导致"外推的镜像内网拉不了"。解法=镜像引用统一用**主机名**（`registry.hostname`，默认 `registry.opsguard`）：集群节点（10 段）与构建机（172 段）各自 /etc/hosts 解析到本网段的管理端地址；节点 `daemon.json` 的 `insecure-registries` 配主机名条目（双网段通用）；部署 YAML 的 image 永远写 `registry.opsguard:<port>/<name>:<tag>`。页面上传构建天然规避寻址（构建机只需能开管理台页面）。
- **保留策略 + GC**：每仓库保留最近 N 个 tag（`retention_per_repo` 默认 10），manifest 写入/删除后自动裁剪 + mark-sweep 清无引用 blob。
- **部署前提**：管理端服务器双网卡（或两网段路由可达）+ 装 docker CLI（仅构建需要）。

---

## 六、关键设计决策

> **已确认（2026-08-04）**：① 资源模型按集群+swarm 服务，裸机 Agent 留扩展；② **AiNexus 整合进后端**（单进程，非独立服务）；③ 内置调度 **单实例**；④ 通知渠道**可配置不预设**，**必须支持飞书或短信**，且**服务部署在内网、短信/飞书等外呼需经互联网服务器代理**；⑤ 用户认证走 **SSO**；⑥ 存储用 **LevelDB**（嵌入式 KV，goleveldb）；⑦ 告警规则**管理 Worker 的 monitoring config**（单一事实来源）。

1. **资源模型：集群 + swarm 服务为第一公民**（类 Rancher），原型的"服务器/应用/中间件"视图在 v1 以"节点/服务/端口服务"呈现；裸机 Agent（JAR 进程/中间件专项）列为扩展，复用 Worker 的 nsenter 宿主机能力与容器 stats。✅ 已确认
2. **AiNexus 整合进后端**：AiNexus 的 Go 代码 **vendor 进 `server/internal/ainexus/`**（Go `internal` 可见性规则下不能跨模块 import，故代码级集成），与管理端同进程运行——无独立服务、无独立端口、无进程间 HTTP；`/ainexus/*` 原生端点挂载进管理端路由，网关自身 APIKey 鉴权关闭（统一走管理端鉴权）。✅ 已确认
3. **告警数据入口 = gRPC 事件订阅**：管理端 `ingest.Subscriber`（每集群一个）主动建立 `SubscribeEvents` gRPC 流，drain Worker 的 `pb.MonitorEvent` → 复用 `ingest.Service.HandleEvent` 落库 + 告警聚合 → 按 seq ACK + 游标持久化（断线续传），天然获得多集群告警汇聚。该方向（server→worker）由部署环境的网络策略约束所驱动——gRPC 流由管理端发起，Worker 借同一条连接回送事件。
4. **巡检编排 v1 简化**：YAML 流程存储 + **内置调度引擎（单实例，Go cron，不做分布式/并发控制）**，按流程调 AiNexus 生成告警/报告；与 Worker 实时探针互补。✅ 已确认
5. **SSE 透传**：AI 对话与日志流均以 SSE 从前端直连体验，后端只做代理不做缓冲。
6. **存储 LevelDB**：管理端自身数据（集群注册表、告警、巡检、通知、用户）落 **LevelDB（goleveldb，嵌入式 KV）**——单文件目录、零外部服务、零运维，契合单实例 + 内网离线部署；数据模式（追加时序写 + 按 key 有序范围扫）正是 KV 强项；`meta/version` + 迁移函数自管版本，数据目录拷走即备份。✅ 已确认（原 PostgreSQL，改用 LevelDB）
7. **SSO 认证**：管理端用户认证对接企业 SSO（OIDC/SAML 网关），本地账号仅作 fallback；用户/角色与 SSO 目录同步。✅ 已确认
8. **通知渠道可配置 + 互联网代理**：渠道（飞书/短信/钉钉/企微/邮件）均为可配置项，不预设默认；**内网部署约束**——外呼类渠道（短信、飞书等需访问公网）一律经**互联网转发代理**（独立部署在可访问公网的服务器上，内网管理端 → 代理 → 公网渠道 API），代理凭据不入内网库。✅ 已确认
9. **告警规则管理 Worker monitoring config**：管理端告警规则页 = 编辑目标集群 Worker 的服务 monitoring 配置（单一事实来源），不在管理端做独立规则引擎。✅ 已确认
10. **巡检闭环**：巡检报告按全局设置（`settings/patrol-report`：每次/仅异常/关闭 + 渠道）投递通知渠道；巡检异常按 `检查项|节点` 细分为 `patrol_failed` 告警（确定键 ID、原地重激活、新增/复发才通知、恢复自动关闭）。✅ 已实现（2026-08-05）
11. **对话式排查**：troubleshoot 为对话界面（选告警 or 自由提问、多轮追问）；`/ainexus/chat` 扩展 `alert_id`/`use_mcp`——服务端注入证据前缀（每轮前置，追问不丢上下文）+ 按需挂集群 MCP；会话自动落库 `investigation/` 并回写告警「已排查」标记。✅ 已实现（2026-08-05）
12. **内嵌镜像仓库**：OCI registry 内嵌管理端进程（/v2，basic auth），镜像构建在**管理端本机**执行（docker CLI，职责清晰），push 走 127.0.0.1 loopback 免 HTTPS；**双网段（10 内网/172 政务外网）统一主机名寻址**，规避"外推内拉"割裂。✅ 已实现（2026-08-05）

---

## 七、数据模型（管理端存储，LevelDB KV）

LevelDB 无表/SQL：数据按 **桶（bucket）前缀 + 主键** 组织，值为 JSON 编码；按 key 字节序有序，`seq`/`ts` 前缀天然支持时间序分页；过滤靠复合索引 key。

```
key 结构: <bucket>/<pk>  → JSON 值

meta/version                      -> 数据版本号（迁移用，整数递增）

cluster/<name>                    -> Cluster{name, project_id, worker_url(gRPC), worker_http_url(HTTP),
                                       mcp_url, token, desc, status(online/offline), last_seen, inventory}
cluster/idx/status/<status>       -> ""  （按状态枚举，可选）

event/<seq>                       -> IngestEvent{id, ts, cluster_id, service, type,
                                       level, msg, detail}   // 来自 gRPC SubscribeEvents 订阅
event/idx/cluster/<cluster>/<seq> -> ""  （按集群过滤，可选）

alert/<id>                        -> Alert{id, cluster_id, service, level, title,
                                       status(active/acked/recovered), count, first_ts, last_ts,
                                       investigations, last_investigation_id}  // 排查回写
alert/idx/<status>/<cluster>/<ts>/<id> -> ""  （告警列表过滤/排序索引）
// 告警 id = sha256(cluster|service|type) 前 6 字节；巡检告警用 AlertIDWithKey
// （附加 检查项|节点 标识），复发原地重激活（不换 id），见 syncAlerts

patrol/<id>                       -> Patrol{id, name, description, cron, enabled, yaml}
patrolrun/<id>                    -> PatrolRun{id, patrol_id, started_at, finished_at,
                                       result, anomalies}   // Anomaly 含 node 字段（节点级检查）
report/<id>                       -> Report{id, patrol_run_id, ai_summary}

investigation/<seq>               -> Investigation{id, alert_id, cluster, title,
                                       messages(对话 JSON), conclusion, model, created_at}
                                       // 对话式排查落库；关联告警时回写 alert.investigations

settings/<key>                    -> JSON 值（全局设置，如 patrol-report 投递策略）

ainexus/runtime                   -> 网关运行时配置（YAML 原文，页面保存即热重载）

notify/channel/<id>               -> NotifyChannel{id, type, name, config, via_proxy, enabled}
notify/policy/<level>             -> NotifyPolicy{level, channel_ids, receivers, escalate}
notify/record/<seq>               -> NotifyRecord{id, ts, channel_id, alert_id, target, status, error}

user/<id>                         -> User{id, sso_sub, username, name, role, phone,
                                       feishu_id, notify_channels, enabled}

seq/<kind>                        -> 自增序列（告警 id、事件 seq 等）
```

> 写入模式：单实例 + goleveldb **WriteBatch 原子批量**；读-改-写（如告警 count 累加）用进程内互斥锁串行化。备份 = 停写后拷贝数据目录；升级 = `meta/version` 比对 + 顺序执行迁移函数。

---

## 八、分期实施计划

| 阶段 | 里程碑 | 交付物 |
|---|---|---|
| **P0 骨架** ✅ | 前后端骨架 + 路由占位 | `OpsGaurdWeb/web` + `server`（已提交 f2900f2） |
| **P1 集群接入** ✅ | 集群注册表 CRUD + Worker 健康探测 + Worker 代理客户端 + **LevelDB 存储层** | `cluster/`、`workerproxy/`、`store/`、前端集群页接真数据（已提交，真机双集群验证通过（2026-08-05）） |
| **P2 工作负载** ✅ | 服务列表/详情/部署/缩放/重启/移除 + 异步操作轮询 + SSE 日志 | 前端 workloads 页 + 后端代理（已提交，真机双集群验证通过（2026-08-05）） |
| **P3 监控告警** ✅ | gRPC 事件订阅（SubscribeEvents 订阅 + 游标持久化 + seq ACK）+ 告警落库/列表/认领/恢复（事件驱动）+ 节点资源视图 + 告警规则（管理 Worker monitoring config，**P6 待做**） | `ingest`（subscriber/manager/cursor）、`Alert`、前端 alerts/monitor 页（已提交，真机双集群验证通过（2026-08-05）） |
| **P4 AiNexus 整合** ✅ | **vendor AiNexus 进后端** + `/ainexus/*` 原生端点 + /ainexus/chat 进程内 SSE + **深度排查闭环**（告警 → 事件/日志/审计上下文注入 → 内嵌 Agent + **动态连接集群 Worker MCP 采证**） | `ainexus/` 内嵌网关、前端 troubleshoot 页（已提交，真机 LLM 联调待做） |
| **P5 智能巡检** ✅ | YAML 流程 CRUD + 内置调度引擎（单实例 Go cron）+ 执行记录 + AI 报告 | `patrol/`、前端 patrol/schedule/report 页（已提交，真机验证通过（2026-08-05）） |
| **P6 通知/系统** ✅ | 渠道（可配置）+ 互联网代理对接 + 策略/记录 + **SSO 认证**（OIDC + 本地 fallback）+ 用户管理 + 告警规则（管理 Worker monitoring config） | `notify/`、`alertrule/`、`auth/`、`users`、前端 notify/system 页（已提交，真机验证通过（2026-08-05）） |
| **P7 网关页面化** ✅ | AiNexus 网关配置页面管理（保存即热重载）+ 模型统一配置（default_model + 集群 MCP 自动合并）+ MCP headers 透传/工具名清洗修复 | `ainexusrt/`、系统设置「模型配置/AI 排查网关」tab（2026-08-05） |
| **P8 巡检闭环+节点探测** ✅ | 巡检 port/http/process 检查类型（Worker `/local/check/*` + 进程 filter）+ 报告渠道投递 + 异常转告警（patrol_failed）+ 对话式排查页（多轮/落库/告警回写）+ MCP 探测三工具（check_port/check_http/list_host_processes） | `patrol/`、`api/investigation.go`、troubleshoot 对话页（2026-08-05） |
| **P9 内嵌镜像仓库** ✅ | OCI /v2（pull/push/catalog/delete，basic auth）+ 页面传包构建（docker CLI 状态机）+ 保留策略/GC + 双网段统一主机名寻址 | `registry/`、前端镜像仓库页（2026-08-05） |
| **P10 菜单重组 + 账号安全** ✅ | 模型配置移入 MLOps「模型接入」（mlops 关闭时仍可用）+ 巡检报告投递移入智能巡检「报告投递」+ 系统设置收敛为账号/身份/AI 网关/密钥 + 修改密码（自助 + admin 重置）+ 用户管理与网关配置写操作 admin 收敛 | `auth/`（ChangePassword/ResetPassword）、前端 mlops/patrol/system 页（2026-08-21） |

每阶段：单元测试 + 真机（双节点 swarm）联调 + 文档更新。

---

## 九、风险与待确认

> **已确认（2026-08-04）**：资源模型口径 ✅、AiNexus 整合进后端 ✅、巡检单实例 ✅、通知可配置+飞书/短信+互联网代理 ✅、SSO ✅、存储 LevelDB ✅、告警规则管理 Worker config ✅。以下为剩余实施期关注点：

1. **SSO 落地方式**：~~对接哪种 SSO（OIDC? 企业网关? 自建?）；本地账号 fallback 的边界（v1 是否保留本地登录入口）。~~ **已落地（2026-08-06）**：OpsGaurd 自身作为 OIDC 身份提供者（IdP），其他系统（企业内业务应用、Worker/MCP 工具）跳转 `/api/v1/idp/authorize` 过来认证；对外提供标准 Discovery/JWKS/Authorize/Token/UserInfo/Introspect 端点，授权码 + PKCE，RS256 签名。管理台「系统设置 → 身份提供者」注册 client。本地账号保留为 fallback 与引导管理员。对接指南见 `docs/IdP-接入指南.md`。同时 `auth.sso.oidc` 仍保留作为 RP 对接上游企业 SSO 的能力（方向相反，两者可并存）。
2. **通知互联网代理**：代理服务的形态（独立小服务 HTTP 转发？还是复用现有网关？）；短信服务商（阿里云/腾讯云）与飞书 Webhook 的凭据存放（代理侧）。
3. **LevelDB 数据量/备份**：单实例 KV 在告警/事件量级增长后的压缩与归档策略（`alert/idx/*` 索引修剪、数据目录备份窗口）；是否需要定期 compact 与冷备。
4. **AiNexus 内嵌边界**：vendor 时保留 AiNexus 的 providers/tools/MCP/Agent 全部能力；管理端与内嵌网关共享一个 gin 引擎后，需确认 `/ainexus/*` 原生端点与 `/api/v1/ainexus/*` 的业务化包装不冲突；内嵌后关闭网关自身 APIKey 校验，鉴权收敛到管理端。
5. **告警规则管理范围**：管理端编辑 Worker monitoring config 时的校验/下发链路（复用 Worker deploy/update 的 config 通道）。
6. **巡检调度**：单实例 Go cron 的持久化（进程重启后 cron 恢复）与执行记录保留策略。

---

## 附录：与 Worker/AiNexus 的能力对照

| 原型功能 | 落点 | 状态 |
|---|---|---|
| 资源采集（CPU/内存/磁盘） | Worker `/local/stats` + 节点 stats | ✅ 已实现 |
| 进程/服务健康 | Worker 服务健康（task+healthcheck）+ `/local/processes`（filter） | ✅ 已实现 |
| 命令执行/SSH | Worker `exec_host_command`/`exec_in_container`（黑白名单） | ✅ 已实现 |
| MCP 工具（AI 排查证据） | Worker `/mcp` 20 工具（含 check_port/check_http/check_flow/list_host_processes） | ✅ 已实现 |
| 事件/告警 | Worker 事件 + gRPC SubscribeEvents 订阅 + 管理端 ingest 聚合（游标 + ACK） | ✅ 已实现 |
| LLM 对话/多模型路由 | AiNexus（已 vendor 进后端，`server/internal/ainexus`） | ✅ 已实现 |
| 分层调 LLM 策略 | AiNexus 多模型路由 + default_model 统一配置（页面管理，热重载） | ✅ 已实现 |
| 巡检编排/调度/报告 | 管理端 `patrol/`（六类检查 + 报告投递 + 异常转告警） | ✅ 已实现 |
| 通知（飞书/短信） | 管理端 `notify/`（渠道/策略/记录 + 通用 Send） | ✅ 已实现 |
| 用户/系统设置 | 管理端（用户/SSO/身份提供者/AI 网关/密钥 tab + 修改密码/重置密码） | ✅ 已实现 |
| 镜像仓库/构建 | 管理端 `registry/`（内嵌 OCI /v2 + 页面传包构建） | ✅ 已实现（P9） |
