# AiNexus Skill 与 MCP 管理设计方案

> 版本：v0.1（草案）
> 日期：2026-08-21
> 状态：待评审
> 关联：`OpsGaurdWeb/server/internal/ainexus/`（内嵌 AI 网关）、`internal/mlops/`（Prompt Hub）、`internal/ainexusrt/`（运行时热重载）、`Worker/internal/mcp/`（Worker MCP Server，20 工具）、《OpsGaurd-场景操作手册.md》（12 运维场景，内置技能内容来源）

---

## 一、背景与现状

### 1.1 现有能力链路

```
前端对话页(troubleshoot) ──POST /api/v1/ainexus/chat──▶ api/ainexus.go
    │ (alert_id 时注入证据种子消息；use_mcp 连接集群 Worker MCP)
    ▼
内嵌 AiNexus 网关(ainexus/server) ── ReAct Agent(ainexus/agent)
    │ system/首条消息 = mlops Prompt Hub 场景模板(4 内置场景)
    │ 工具 = registry(内置 command/http_request/file_read)
    │       + MCP Manager(用户 mcp_servers 配置 + cluster:* 自动连接)
    ▼
Provider(openai/anthropic 兼容) → 模型池；usage 计量(chat/investigate/...)
```

- **提示词**：Prompt Hub 管入口消息模板（investigate_system / investigate_user / compress_system / patrol_system + 自定义），版本化 + 激活回滚，经 `ScenarioTemplateSource` 最小接口注入引擎。
- **MCP**：`mcp.Manager` 支持 stdio / sse / streamable-http 三种传输；集群 Worker 的 MCP（`/mcp`，bearer token，20 工具）在网关启动与热重载后自动重连；工具命名 `mcp_{server}_{tool}` 清洗后注入 registry，每轮请求全量携带。

### 1.2 问题清单

**Skill 侧（知识无载体）**

| # | 问题 | 现状证据 |
|---|---|---|
| S1 | 领域排查知识无法沉淀给 Agent。《场景操作手册》12 个运维场景是纯人读文档，老运维的 playbook（现象→检查项→命令→判据）没有机器可消费的载体 | Agent 只有固定 system 模板 + 工具定义，无任何可扩展知识注入机制 |
| S2 | 知识只能往 system 模板里堆，随场景增长 token 膨胀且无按需加载 | investigate_system 模板单条消息，全量常驻每轮请求 |
| S3 | Prompt Hub 的自定义场景"不接入线上入口"，写了也没有消费端 | `mlops.ScenarioCustom` 注释明确不接线上 |

**MCP 侧（有连接、无管理）**

| # | 问题 | 现状证据 |
|---|---|---|
| M1 | 无前端编辑入口。mcp_servers 只能手改 config.yaml（文件为源时）或直接调 PUT /v1/ainexus/config | 前端 `ModelConfig.vue` 无任何 mcp 内容；GET 视图里 mcp_servers 仅只读展示 |
| M2 | 无单 server 生命周期：想停用一个 server 只能整包改配置热重载；无 `enabled` 字段；无连通性测试（provider 有 /config/test，MCP 没有） | `MCPServerConfig` 无 enabled；Update 只支持全量 |
| M3 | 无健康状态与自愈：连接失败只留进程日志；Worker 重启后工具列表不刷新（不处理 listChanged）；页面看不到"连上了没、有几个工具、最近错误" | `AddServer` 失败仅 log；无心跳、无状态查询 |
| M4 | 工具治理缺失：不同 server 同名工具注册失败被静默跳过；工具总数无上限（乱加 server 会撑爆每轮 tools 定义 token） | `RegisterAllTools` 冲突 continue；无 max 限制 |
| M5 | 任意 URL 可配置为 MCP server（http/sse），内网管理端存在 SSRF / 证据外发风险 | `Validate` 仅校验 URL 非空 |
| M6 | **部署形态未定义**：用户自定义 MCP 程序装在哪里没有答案——装 server（stdio）把管理节点变成通用运行时且访问集群资源要另打通链路；装 worker 深度绑定、每集群重复安装；部署为 swarm service 受 overlay 网络隔离约束 | `Manager` 支持 stdio 但无任何部署指引与边界定义 |

### 1.3 概念定位（三者正交）

| 机制 | 回答的问题 | 归属 | 生命周期 |
|---|---|---|---|
| **Prompt Hub（已有）** | 会话入口怎么问（消息模板） | mlops 运营层 | 版本化，激活生效 |
| **Skill（本方案新增）** | 排查过程中用什么知识（playbook/判据/流程） | mlops 运营层 | 独立管理，索引常驻 + 正文按需 |
| **MCP 工具（增强）** | Agent 能做什么动作 | 网关配置（runtime YAML） | 热重载/心跳自愈 |

---

## 二、目标与非目标

**目标**

1. Agent 获得可运营的技能机制：领域知识以技能形式沉淀、管理、按需加载，不膨胀常驻 token。
2. MCP server 具备完整管理面：页面增删改查、启停、连通性测试、状态可见、故障自愈。
3. 复用既有架构模式，不侵入引擎内核：Skill 走 `SkillSource` 最小接口（同 `PromptSource` 模式）；MCP 管理走 runtime YAML + 热重载。

**非目标**

1. 不做多 Agent / 子 Agent 编排（技能只是知识与流程，不改变 ReAct 单循环结构）。
2. 不做 skill 内嵌可执行代码/脚本（正文是纯 Markdown 指导，动作仍由现有工具执行，规避任意代码执行风险）。
3. 不做 MCP server 的 OAuth 客户端凭证管理（沿用 headers 明文 + 脱敏回显模式）。
4. 不改动 Worker 侧 MCP Server（本方案只做管理端/MCP 客户端侧）。

---

## 三、Skill 设计

### 3.1 数据模型

技能是 mlops 运营资产（同 Prompt Hub），**不进 ainexus runtime YAML**——热重载网关不重置技能，技能变更也无需重建网关。

```go
// internal/mlops/skill.go
type Skill struct {
    ID          int64  `json:"id"`           // LevelDB 自增
    Name        string `json:"name"`         // 唯一 ASCII slug，如 "svc-unhealthy"
    DisplayName string `json:"display_name"` // 页面显示名，如 "服务副本不健康排查"
    // Description 一句话触发条件。必须写"什么现象/什么任务时用"，
    // 它是 L1 索引里 Agent 判断是否调用 read_skill 的唯一依据。
    Description string   `json:"description"`
    Content     string   `json:"content"`    // Markdown 正文（流程/命令/判据/预期结果）
    Scenarios   []string `json:"scenarios"`  // 生效场景：chat / investigate；空 = 全部
    Enabled     bool     `json:"enabled"`
    Builtin     bool     `json:"builtin"`    // 内置技能（页面可改可禁用，不可删除）
    UpdatedAt   int64    `json:"updated_at"`
    UpdatedBy   string   `json:"updated_by"`
}
```

存储：LevelDB `mlops/skills` 前缀（沿用 store 包既有 bucket 模式）。P1 单版本 + 更新审计字段；P2 复用 Prompt Hub 的版本/激活机制（见 9. 分期）。

约束：`Content` ≤ 32KB；`Name` 匹配 `^[a-z0-9][a-z0-9-]{1,63}$`（与工具命名空间隔离，read_skill 参数直接透传）；启用技能数 ≤ 32（索引 token 预算，见 3.2）。

### 3.2 渐进式注入（三层披露）

**L1 索引（常驻）**：在会话 system 消息之后追加一条独立的技能目录 system 消息，每个启用技能一行（名称 + 触发条件），上限约 1KB：

```text
【可用技能】以下为领域排查技能。判断当前任务与某技能的触发条件匹配时，
先调用 read_skill 工具阅读全文，再按其流程排查；不匹配则忽略：
- svc-unhealthy: 服务副本不健康、持续重启或 running<desired 时使用
- port-down: 端口不通/TCP 拨测失败、跨节点连通性问题时使用
- resource-over: 容器 CPU/内存超限、OOMKilled、limit 逼近时使用
```

**L2 正文（按需）**：内置工具 `read_skill`，Agent 在 ReAct 循环中自主调用：

```go
// 注册名 read_skill；参数 {"name": "svc-unhealthy"}；返回 Markdown 全文。
// 超过 MaxSkillResultBytes(16KB) 时头尾保留、中部截断（复用 mcp_tool.go
// 的截断策略）；技能不存在/已禁用返回错误文本，Agent 可自行继续。
```

**L3 引用（P3，暂不实现）**：技能正文引用附属资源（示例 YAML、扩展文档），由 file_read 类工具按白名单读取。

**注入位置的关键约束**：Prompt Hub 内置模板与代码硬编码提示词有**逐字节一致性回归测试**守护（`TestInvestigateTemplateV1MatchesLegacy` 等）。技能索引必须作为**独立追加的 system 消息**注入（Conversation 组装完成后 append），绝不拼接进场景模板消息，否则破坏等价性测试与模板语义。

### 3.3 引擎接入（最小接口，同 PromptSource 模式）

引擎（ainexus 包）不依赖 mlops，仅依赖接口；mlops.Service 实现之：

```go
// internal/ainexus/agent/skills.go（或 server 包，与 ScenarioTemplateSource 同文件层级）
type SkillSource interface {
    // SkillIndex 返回指定场景下启用技能的元数据（L1 目录行素材）。
    SkillIndex(scenario string) []SkillMeta
    // SkillContent 按名称返回技能正文（read_skill 执行时调用）。
    SkillContent(name string) (content string, ok bool)
}

type SkillMeta struct {
    Name        string `json:"name"`
    Description string `json:"description"`
}
```

接线方式：

- `ainexusrt.Service` 增加 `SetSkillSource(src)`（须在 Init 前注入，热重载复用，同 `SetPromptSource`）。
- `server.New` 增加 `WithSkillSource(...)` Option；`Initialize` 时注册 `read_skill` 内置工具（实现里持有 SkillSource；nil source 时不注册该工具、不注入索引）。
- `handler.OpenAIHandler.ChatCompletions` / `AnthropicHandler.Messages` 在 conv 消息组装完成后调用 `appendSkillIndex(conv, scenario)`：scenario 由计量上下文（usage.Operation）或请求入口区分（chat / investigate / native_chat）。
- `Summarize`（巡检报告）不注入——巡检是结构化报告生成，不是排查过程。

`read_skill` 的调用同样计入工具事件流（tool_start/tool_end），前端对话页可看到"Agent 正在阅读技能 xxx"。

### 3.4 触发方式

| 方式 | 机制 | 适用 |
|---|---|---|
| 自动 | L1 索引 + Agent 自主调用 read_skill | 默认 |
| 手动挂载 | chat 请求体扩展 `skills: ["name", ...]`：服务端剥离该字段后，将对应技能正文直接注入为独立 system 消息（免 L2 一跳， guaranteed 生效） | 用户明确知道要用某技能；前端对话页提供"挂载技能"多选器 |
| 场景绑定 | mlops 场景→技能集绑定（P2） | 告警类型自动带技能 |

手动挂载的技能仍受 Enabled 检查（禁用技能挂载报 400）。

### 3.5 管理端（MLOps 技能 Hub）

MLOps 页新增"技能"tab（与"提示词"tab 并列，仅 admin 可写）：

- **列表**：显示名 / name / 触发条件 / 生效场景 / 启用开关 / 内置标记 / 近 30 天使用次数 / 更新时间。
- **编辑器**：Markdown 文本域（ displayName / description / scenarios / content ），保存即生效（无需任何重载——见 3.1）。
- **使用统计**：read_skill 每次执行 `s.st` 计数（skill 名 + 月度桶），列表页展示；P3 做趋势。

API（挂 `/api/v1/mlops`，读全员 / 写 admin，同 Prompt Hub 权限模式）：

```
GET    /v1/mlops/skills                 列表（含 enabled/builtin/usage）
GET    /v1/mlops/skills/:id             详情
POST   /v1/mlops/skills                 新建
PUT    /v1/mlops/skills/:id             更新（保存即生效）
DELETE /v1/mlops/skills/:id             删除（builtin 拒绝）
POST   /v1/mlops/skills/:id/render      预览 L1 索引行与 read_skill 返回效果
```

### 3.6 内置首批技能（从场景手册提炼）

| name | 来源手册场景 | 内容骨架 |
|---|---|---|
| `svc-unhealthy` | 服务异常/副本不健康 | get_service 看 desired/running 差异 → get_service_logs 找重启原因 → get_resource_usage 查 OOM → 处置（scale/restart/回滚 update） |
| `port-down` | 端口不通 | check_port 定位断点（服务内→节点间→入口）→ get_self 确认节点角色 → list_host_processes 确认监听 → 处置 |
| `resource-over` | 资源超限 | get_resource_usage 定位 task → 判据（CPU% 阈值 / mem vs limit）→ get_service_logs 找 OOMKilled → 调 limit 或查代码 |
| `http-5xx` | 流量层异常（P2） | check_http / check_flow 分层拨测 → get_events 看关联告警 → 上游/下游定位 |

内置技能在 `mlops/skills.go` 中硬编码（同 builtin templates 模式），首启落库 `builtin=true`；页面可编辑（编辑后 builtin 语义变为"基于内置修改"，提供"还原默认"）。

### 3.7 安全与成本

- 技能正文是纯文本进上下文，无执行语义；`read_skill` 是只读工具，无参数注入面（name 白名单校验 `^[a-z0-9-]+$`）。
- L1 索引 ≤ 32 技能 × ≤ 60 字描述 ≈ ≤ 1.5KB 常驻，对 1M 窗口与 800K 预算可忽略；P1 记录索引实际 token 估算到日志。
- 技能正文单次读取 ≤ 16KB 截断保护，防超长正文挤占窗口。

---

## 四、MCP 管理增强

### 4.1 部署形态决策：用户自定义 MCP 仅支持 HTTP 传输，程序用户自行部署

**决策**：管理端只承担 MCP **客户端**角色，不承载用户 MCP 程序的运行时职责。用户自定义 MCP 仅允许 `sse` / `streamable-http` 两种传输（页面表单、API、Validate 三处收口）；程序部署位置由用户按网络与目标资源自定，**唯一前提是管理端到 MCP URL 的出向网络可达**。

**候选部署位置对比**：

| 候选位置 | 通信链路 | 评估 |
|---|---|---|
| server 进程内（stdio 子进程） | Agent → 本机子进程 | ✗ 管理节点被污染成通用运行时（Python/Node 依赖、资源占用、升级维护）；用户代码运行在握有全部集群凭据的管理端进程环境，安全边界最差；管理端为容器化部署时子进程生命周期与文件挂载均不可靠；且 MCP 若要操作集群资源，链路退化为 Agent→本机子进程→（gRPC/API）→Worker，反而是最长路径 |
| worker 进程内（每集群） | Agent → Worker | ✗ Worker 是统一 Go 二进制，承载用户任意程序 = 深度绑定实现细节；每个集群重复安装一份；随 Worker 升级联动 |
| swarm service（集群内） | Agent → 集群发布地址 → overlay 内 service | △ 受 overlay 网络隔离约束：须经 ingress 发布端口 + 防火墙放通，每个 MCP 占用一个发布端口。但它是"需访问该集群内资源（数据库/中间件/服务）的 MCP"的自然形态——service 在 overlay 内直达目标，且系统已有部署编排能力可辅助落地（见推荐形态 B） |
| **独立 HTTP 服务（本方案采用）** | Agent → http(s) URL | ✓ 唯一前提是管理端出向可达；进程守护/升级/扩容归用户既有运维体系；与集群 Worker MCP（streamable-http + bearer）模式完全一致，客户端代码路径统一 |

**网络边界说明**：MCP 连接始终由管理端进程**出向发起**（streamable-http 为单 POST 端点，sse 为 GET+POST），不需要任何反向入站端口——防火墙只需放通管理端出向流量。这比任何"装在 server/worker 内"的进程内或 gRPC 双向方案都简单，也规避了管理端容器化后 stdio 子进程的宿主依赖。

**推荐部署形态**（页面"添加 MCP"处给出引导文案）：

- **A. 独立虚机/容器，与管理端同管理网**——最常见，零额外网络配置；
- **B. 部署为目标集群的 swarm service，经 ingress 发布**——当 MCP 需访问该集群内资源时（overlay 网络直达数据库/中间件）；系统已有 `deploy_service` 编排能力，P3 提供"一键生成部署 YAML（含端口发布与 token 环境变量）"的引导，降低 overlay 通信门槛。**部署进 ops-net 且经 Worker 代理（4.8）时可免发布端口**；
- **C. 内网既有平台（K8s/虚机/集群 r-nacos 等）上已运行的 remote MCP**——直接填 URL（含 r-nacos 暴露的 MCP 端点，见 4.7 边界说明）；公网 SaaS 类 MCP 须显式勾选 `allow_public_url`（见 4.6）。

**stdio 处置**：用户配置面禁用——`Validate` 对用户自定义 server 拒绝 stdio 传输（错误信息引导改造为 http）。`cluster:` 自动 MCP 与 mcp-go 客户端的 stdio 代码能力不受影响（本地调试仍可用）。存量 YAML 中的 stdio 条目：加载时跳过并标记 `transport_disabled`，页面提示迁移。原计划的"stdio 命令白名单"（P2）随之取消，管理面无 stdio 即无命令执行面。

### 4.2 配置模型扩展

```go
// internal/ainexus/config/config.go
type MCPServerConfig struct {
    // ... 现有字段不变 ...
    // Enabled 单 server 启停。旧 YAML 无此字段 = 启用（UnmarshalYAML 归一化，
    // 模式同 ModelConfig.Enabled，避免存量配置全部被判禁用）。
    Enabled bool `yaml:"enabled,omitempty"`
    // Description 用途备注（页面展示）。
    Description string `yaml:"description,omitempty"`
    // DisabledTools 按原始工具名排除（如高危 exec_in_container）。
    DisabledTools []string `yaml:"disabled_tools,omitempty"`
    // AllowPublicURL 允许非私网地址（默认 false，见 4.6 安全）。
    AllowPublicURL bool `yaml:"allow_public_url,omitempty"`
    // Command/Args/Env 仅为存量 YAML 兼容保留：用户配置面已禁用 stdio
    // （见 4.1），Validate 对自定义 server 拒绝该传输。
}
```

加载侧：`Manager.Initialize` 跳过 `Enabled=false` 的条目（保留配置，状态标记 disabled）；`refreshTools` 过滤 `DisabledTools`。

### 4.3 生命周期 API（单 server 语义）

新增路由组 `/api/v1/ainexus/mcp`（读全员 / 写 admin）：

```
GET    /v1/ainexus/mcp/servers                    列表（配置 + 运行状态，见 4.4）
GET    /v1/ainexus/mcp/servers/:name/tools        该 server 的工具定义清单
POST   /v1/ainexus/mcp/servers                    新增（仅 sse/streamable-http；
                                                 校验→连接→持久化）
PUT    /v1/ainexus/mcp/servers/:name              修改（重连；headers 空白沿用旧值）
DELETE /v1/ainexus/mcp/servers/:name              删除（断开 + 移除配置）
POST   /v1/ainexus/mcp/servers/:name/enable       启用（连接）
POST   /v1/ainexus/mcp/servers/:name/disable      停用（断开，保留配置）
POST   /v1/ainexus/mcp/servers/test               连通性测试（不落库：
                                                 initialize + listTools，返回
                                                 ok/latency_ms/tool_count/error，
                                                 语义对齐 provider 的 /config/test）
```

`cluster:*` 前缀条目只读（沿用现有约定：集群 MCP 由集群服务自动管理，页面仅展示）。连通性测试同时是 4.1"网络得通"前提的验收工具：添加流程默认先测试再保存（测试失败仍允许保存，状态标记 error 待自愈）。

**实现策略（分两步）**：

- **P1——复用全量热重载**：每个写操作 = 修改 runtime cfg 的 `MCPServers` → `ainexusrt.Update()`。实现量最小、原子性复用（构建失败旧网关继续服务）。代价：所有 MCP 连接重建（用户 MCP 与集群 MCP 均为 http 类，重连成本 ms 级，可接受；已无 stdio 子进程重启问题）。
- **P2——Manager 增量方法**：`RemoveServer(name)`（关连接 + 从 registry 注销工具）、`ReplaceServer(cfg)`（同名单server 重连 + 工具重注册），写操作仅对目标 server 增量执行，消除多集群场景的重连抖动。registry 增加 `Unregister(name)` 按前缀注销。

### 4.4 状态与健康自愈

`MCPServerClient` 增加状态结构，`Manager` 增加心跳循环：

```go
type ServerStatus struct {
    Name       string  `json:"name"`
    State      string  `json:"state"`       // connected | disabled | error | reconnecting | transport_disabled
    ToolCount  int     `json:"tool_count"`
    LastError  string  `json:"last_error,omitempty"`
    LastLatencyMS int64 `json:"last_latency_ms,omitempty"` // 最近一次成功 ping/listTools
    LastCheck  int64   `json:"last_check,omitempty"`       // unix 秒
    ConsecutiveFails int `json:"consecutive_fails,omitempty"`
}
```

- **心跳**：`Manager.StartHealthLoop(ctx, 60s)`，对每个 connected server 轮询 ping（超时 5s）。
- **自愈**：连续失败 ≥ 3 次 → state=reconnecting → 关闭旧 client 按原配置重建（指数退避 5s→10s→…→5min 封顶）；成功 → connected 并刷新状态。集群 MCP 覆盖 Worker 重启场景，替代"只能整体热重载"的恢复手段。
- **工具变更**：每次心跳成功后轻量 `listTools`，比对工具名集合 hash；变化 → `refreshTools` + 重新注册（处理 MCP `listChanged` 语义，覆盖 Worker 升级新增工具的场景）。
- 状态消费：`GET /v1/ainexus/mcp/servers` 聚合（配置脱敏视图 + ServerStatus）、`/ainexus/health`、前端列表页。

### 4.5 工具治理

- **冲突报告**：`RegisterAllTools` 返回被跳过的 `(server, tool, 原因)` 列表，`Server` 持有并经 API 暴露；前端列表页标黄提示"server X 的 mcp_x_y 因名称冲突未注册"。
- **总量上限**：`ToolsConfig.MaxTotalTools`（默认 64，0 = 不限）。注册前检查 registry 当前数量，超限时新 server 的工具整批不注册、状态标记 `tools_over_limit`，防多 server 场景下每轮 tools 定义 token 失控（当前 20 个 Worker 工具 schema ≈ 5–10K token，64 上限留足余量）。
- **工具清单**：`GET .../servers/:name/tools` 返回原始名、注入名（`mcp_{server}_{tool}` 清洗后）、描述、参数 schema、是否被 disabled_tools 排除——给运营可见性，也为排障"为什么模型没调某工具"提供依据。

### 4.6 安全

- **URL 内网限制（SSRF/证据外发）**：http/sse/streamable-http 传输的 URL，host 解析为 IP 后非私网段（RFC1918/回环/链路本地/已纳管集群地址）且 `AllowPublicURL != true` → Validate 拒绝。管理端本身部署于内网、出网走受控 internet-proxy（见提交 56703ae），默认拒绝公网目标与该边界一致。域名解析多 IP 时全部校验。
- **无命令执行面**：用户配置面禁用 stdio（见 4.1）后，管理端不因 MCP 配置执行任何本地命令；`cluster:` 自动 MCP 与 Worker 侧鉴权（bearer token）不变。
- 脱敏回显沿用现状（EnvKeys/HeaderKeys 仅列 key，空白值沿用旧值）。

### 4.7 与 r-nacos MCP 的关系（仅直连，不做深度集成）

r-nacos 是平台内置的注册/配置中心（见《注册配置中心-r-nacos-方案.md》，swarm stack 部署在集群侧，服务于**业务服务**），其 0.7.x+ 自带 MCP 注册中心能力：把注册在 r-nacos 的业务服务 HTTP 接口，经工具描述 + 转发规则声明式转成 MCP 工具，以 SSE / Streamable HTTP 端点暴露（GitHub issue #241）。

**结论：视为普通 http MCP 来源之一，L1 直连即可，不做聚合同步/声明式管理等深度集成。**

**架构边界分析（为什么不做深度集成）**：

1. **管理面与业务面独立**：OpsGaurd 管理端是控制面，r-nacos 属于集群业务面（业务服务的注册发现/配置）。ainexus 是 MCP **客户端**——它从不需要向任何注册中心注册自己，消费 r-nacos 的 MCP 端点也只是"添加一个 URL"的关系，不构成也不应构成集成依赖。
2. **多集群放大**：管理端纳管多个集群，r-nacos 按集群部署且并非每集群必有。若做聚合同步，N 套 r-nacos 的条目发现、命名冲突、状态聚合、集群增减联动都是管理端要背的复杂度，收益却不成比例。
3. **配置权责错位**：r-nacos 里的 tool spec / 转发规则是**业务方**的资产（描述业务接口语义），运维平台既无立场代管，也难对内容负责。
4. **网络边界**：管理端 → 集群内 r-nacos 需发布端口并跨网访问，回到了"通信受限"问题；且该链路可用性依赖业务面组件的健康，管理面不应引入此依赖。
5. **运维价值密度低**：运维 Agent 的工具需求主体是基础设施操作面——Worker MCP 的 20 工具已覆盖（服务/节点/容器/日志/拨测）。业务接口取证属于低频补充，且现有 Worker 工具 `check_http`（单次 HTTP 探测）、`check_flow`（多步事务拨测，支持变量提取）、`exec_in_container`（容器内执行）已可直接对业务接口取证，**无需业务方配置任何 MCP**。

**留给业务方的路径**（若确有给运维 Agent 供工具的需求）：业务方自行暴露网络可达的 http MCP（形态 A），或部署为集群 service（形态 B）；r-nacos 若已配好 MCP server 且端口对管理端可达，直接按形态 C 添加其 URL 即可（token-in-URL 凭据模型，回显打码）。这些路径全部复用本方案的统一管理面（连通性测试、心跳自愈、启停、状态可见），对 r-nacos 无任何特殊处理。

**更优的间接路径**：集群内 MCP（含 r-nacos）经 Worker 代理暴露，免跨网发布端口，见 4.8。

### 4.8 Worker 集群侧 MCP 代理（集群内 MCP 经 Worker 间接暴露）

4.7 指出管理端直连集群内组件的网络与角色边界问题。间接路径：**Worker 作为管理面在集群内的延伸，代理集群内网络可达的 MCP server**——r-nacos 的 mcp_server 端点是首个典型来源，形态 B 的集群内 MCP service 同样适用。

**链路**：

```
管理端 Agent ──MCP(streamable-http + bearer)──▶ Worker /mcp
                                                 ├─ 自有 20 工具（不变）
                                                 └─ 代理转发（MCP 客户端，overlay 内 DNS 直达）
                                                     ├─▶ r-nacos MCP 端点（业务接口转的工具）
                                                     └─▶ 集群内其他 MCP service（无需发布端口）
```

**机制**：

- Worker 增加 MCP **客户端**代理层：配置一组集群内 MCP 端点，握手 + listTools 拉取工具，以 `rn_<server>_<tool>` 前缀聚合进自身 /mcp 的工具列表；tools/call 命中代理前缀则转发到对应端点。Worker 的 MCP server 已基于 mcp-go，客户端能力同库复用，无新依赖。
- **管理端零改动**：仍只连 `cluster:<name>` 的 Worker /mcp；代理工具经既有 `mcp_{server}_{tool}` 清洗规则进 registry（Agent 最终看到 `mcp_cluster_prod_rn_<name>` 形态）。MCP 接入页在各集群条目下展示"经代理"的工具清单（只读，同集群 MCP 约定）。
- **网络收益**：被代理的集群内 MCP **无需 ingress 发布端口**——Worker 与 r-nacos 等平台组件同在 ops-net overlay，DNS 直达；业务 service 部署进同一 overlay 即可。这直接化解形态 B 的"通信受限"（发布端口、防火墙放通全部省掉），也消除了管理端对业务面组件的网络依赖。

**配置与治理**：

- **配置归属**：代理端点列表（URL/凭据）配置在 Worker 侧——P2 以 agent 本地配置静态声明（默认关闭，显式配置才启用）；P3 支持管理端集群配置经 gRPC 下发，MCP 接入页可管理。凭据不进管理端配置，回显打码。
- **工具膨胀治理**：代理工具计入 MaxTotalTools（4.5）；可按 `rn_<server>_*` 前缀在 disabled_tools 整组排除。
- **健康与刷新**：Worker 定期心跳被代理端点，失败标记并隔离——**不影响自有 20 工具的服务**（错误隔离是硬要求）；工具列表变更（如 r-nacos 新增 mcp_server）定期重拉生效。
- **安全与审计**：转发调用写入 Worker 既有操作审计（工具名含来源前缀，可追溯到端点）；每条代理可配 `require_confirm`（默认关，r-nacos 声明的多为查询类工具）；转发超时在 Worker→被代理端→业务实例链路上叠加，透传错误并在结果中标注来源端点，便于排障。

**动态发现与动态注册（r-nacos 作为发现源）——可行性评估**：

代理端点可静态配置，也可由 Worker 从 r-nacos 动态发现 mcp_server 列表并增删代理。结论：**可行，且管理端零新增概念**：

1. Worker 定时（60s）调 r-nacos 的 mcp_server 列表接口，diff 出新增/移除 → 增删代理端点（握手 + listTools）；Worker /mcp 为 stateless 模式，tools/list 实时反映当前工具集，无会话失效问题。
2. r-nacos 上的增删对管理端表现为**同一个 `cluster:<name>` server 的工具列表增减**——由 4.4 心跳的 tools hash 比对自动刷新。即"动态注册到 server"实际退化为"工具列表刷新"，不需要管理端动态增删 server 条目、不需要热重载。
3. registry 为运行时可变结构（RWMutex），新增工具下一轮请求即生效。

前提与风险：

| # | 项 | 说明 | 对策 |
|---|---|---|---|
| 1 | **r-nacos 发现 API 未文档化**（最大不确定点） | mcp_server 经 r-nacos 控制台（10848）管理，走其自有 API，**非 Nacos 标准协议**（且与 Nacos 3.x MCP Registry 不兼容）；列表接口的路径/鉴权需实测确认 | 发现源抽象为接口：静态配置（先行）/ r-nacos API（实测后启用）/ 未来 Nacos 3.x MCP Registry（若切回 Nacos）|
| 2 | **registry 尚无 Unregister**（现状） | 动态场景下 r-nacos 删除 mcp_server 后，管理端若残留 stale 工具定义，模型会尝试调用已不存在的工具 | P2 的 `Unregister`/前缀注销必须**先于**动态发现落地；心跳 diff 出工具消失时同步从 registry 注销 |
| 3 | mcp-go server 侧工具热增删 | Worker 工具集热更新的 API 支持需确认 | 必要时按当前代理集重建 handler（stateless 模式无会话迁移成本）|
| 4 | 工具集不可预知 | 动态发现会引入未经预审的工具 | MaxTotalTools、`rn_` 前缀整组排除、操作审计、页面工具清单可见性须与动态发现**同期**上线，不做裸发现 |
| 5 | Worker 新增凭据 | 发现接口的鉴权凭据存 Worker agent 配置 | 不进管理端配置，回显打码 |

**分期**：P2a 落地静态代理 + 4.4 工具变更刷新 + registry Unregister（验证转发链路与刷新机制）；P2b 在 r-nacos 列表 API 实测通过后启用动态发现（凭据与治理同期）。P3 的形态 B"一键部署引导"默认改为生成"不发布端口 + 进 ops-net + Worker 代理"形态。

---

## 五、前端设计

### 5.1 MLOps：新增两个 tab

- **技能（Skill Hub）**：见 3.5。
- **MCP 接入**：与"模型接入"对称的 server 管理页——
  - 列表：名称 / 传输 / 端点摘要 / 状态灯（connected/disabled/error/reconnecting/transport_disabled）/ 工具数 / 最近延迟 / 最近错误 / 集群标记（只读）/ 启停开关。
  - 操作：添加（弹窗：名称、传输**仅 sse/streamable-http 两选一**、URL、headers/env、描述，附部署引导文案——独立部署同管理网 / 部署为某集群 service / 既有平台直接填 URL）、编辑、删除（二次确认）、测试连接（展示延迟 + 工具数）、展开行内查看工具清单。
  - 状态灯轮询 30s 刷新（复用集群页的轻轮询模式）。

### 5.2 troubleshoot 对话页

- 输入区增加"挂载技能"多选器（数据源 GET /v1/mlops/skills?enabled=true；默认收起，显示已选 chip）。
- 选中后随消息体携带 `skills: [...]`；工具事件流中 read_skill 的 tool_start/tool_end 自然复用现有渲染（显示为"阅读技能"）。

---

## 六、计量与可观测

- **LLM 计量**：skill 索引注入与 read_skill 结果计入既有 scenario（chat/investigate）的 prompt token，无需新口径；不新增 scenario。
- **技能使用**：read_skill 执行计数（name + 月份桶，LevelDB `mlops/skill_usage`），Skill Hub 列表展示近 30 天次数。
- **MCP 可观测**：ServerStatus 进入 `/ainexus/health` 与列表 API；心跳失败/重连成功打结构化日志（现有 logger 前缀），供日志检索。P3 可选：工具调用次数/错误率按 server 统计（需在 MCPTool.Execute 埋点，本次不做）。

---

## 七、测试要点

| 类别 | 用例 |
|---|---|
| 注入隔离 | 技能索引追加后，investigate/patrol 内置模板消息**逐字节不变**（守住既有回归测试）；无技能/nil source 时不追加任何消息 |
| read_skill | 正常读取；不存在/禁用名返回错误文本；超长正文截断；name 非法字符拒绝；与热重载并发（SkillSource 每请求取快照） |
| 手动挂载 | skills 字段剥离后不透传网关；禁用技能 400；挂载后正文独立 system 消息注入 |
| Skill CRUD | name 唯一性/格式校验；builtin 不可删、可改可禁用；保存即生效（新会话可见，进行中会话不受影响） |
| MCP 配置 | 旧 YAML 无 enabled 字段归一化为启用；disabled 跳过加载且配置保留；Validate 覆盖新字段（公网 URL 拒绝/allow 逃生） |
| 传输收口 | 用户配置 stdio 被 Validate 拒绝（错误信息引导改造 http）；存量 stdio 条目加载跳过并标记 transport_disabled；`cluster:` 自动 MCP 与 mcp-go stdio 代码能力不受影响 |
| MCP 生命周期 | 新增失败不落库（连接失败返回错误）；删除断开连接并注销工具；enable/disable 幂等；cluster: 前缀写操作 403 |
| 健康自愈 | 连续失败 3 次触发重连；重连成功恢复 connected；工具列表变化触发重注册；心跳循环随网关热重载启停（不泄漏旧循环） |
| 工具治理 | 同名冲突进报告不再静默；超 MaxTotalTools 整批不注册且状态标记；disabled_tools 过滤生效 |

---

## 八、风险与对策

| 风险 | 对策 |
|---|---|
| LLM 不主动调用 read_skill，技能形同虚设 | description 强制写触发条件（CRUD 校验非空且含场景词）；L1 目录措辞明确"先读再用"；手动挂载作为确定性兜底；上线后看使用计数迭代措辞 |
| 每轮请求 token 增长（索引 + 工具清单） | 索引硬上限 32 条/1.5KB；MaxTotalTools=64；超限整批拒绝并状态可见 |
| 全量热重载抖动（P1 的 MCP 写操作） | 写操作低频（配置变更），用户/集群 MCP 均为 http stateless 重连 ms 级；P2 增量方法消除 |
| 用户 MCP 网络不可达（添加时通、运行中断） | 添加流程默认先连通性测试再保存；心跳自愈持续重连（指数退避封顶）；状态灯与最近错误页面可见，不静默失效 |
| Worker 代理引入 Worker 与集群内组件（r-nacos 等）的运行时耦合 | 代理默认关闭、显式配置才启用；失败标记并隔离（不影响自有 20 工具）；代理工具计入 MaxTotalTools 且可按前缀整组排除 |
| 公网 URL 限制误伤合法外网 MCP | AllowPublicURL 显式逃生 + 错误信息说明原因 |
| 技能内容质量参差误导排查 | builtin 技能给标准骨架；页面预览渲染（render 接口）所见即所得；使用统计反向发现低效技能 |

---

## 九、分期实施

| 阶段 | 范围 | 主要改动点 |
|---|---|---|
| **P1a Skill MVP** | SkillSource 接口 + read_skill 工具 + L1 注入（chat/investigate）+ 手动挂载字段 + Skill Hub CRUD（单版本）+ 3 个内置技能 + 使用计数 | `ainexus/agent|server`（接口+工具+注入）、`mlops/skill.go`、`store`、`api/mlops` 路由、前端 mlops 技能 tab + 对话页挂载器 |
| **P1b MCP 基础管理** | **传输收口（用户自定义仅 sse/streamable-http，见 4.1）** + enabled/description 字段 + 单 server CRUD/test API（全量热重载实现）+ 前端 MCP 接入 tab（含部署引导文案） | `ainexus/config`（Validate）、`mcp/manager`（状态字段）、`ainexusrt`（增量封装）、`api`、前端 |
| **P2 自愈与治理** | 心跳/重连/工具变更刷新；增量 RemoveServer/ReplaceServer；disabled_tools、冲突报告、MaxTotalTools、工具清单 API；skill 版本化 | `mcp/manager`、`tool/registry`（Unregister）、`mlops` |
| **P2a Worker 静态代理** | Worker 集群侧 MCP 代理（4.8：静态配置代理 r-nacos 等集群内端点）+ registry Unregister/前缀注销 + 工具变更刷新联调（管理端零改动） | `Worker/internal/mcp`（客户端代理层）、`tool/registry` |
| **P2b Worker 动态发现** | Worker 从 r-nacos 列表 API 动态发现 mcp_server 并增删代理（4.8 可行性评估：发现源抽象接口；r-nacos API 实测通过后启用；治理与凭据同期落地） | `Worker/internal/mcp`（发现源） |
| **P3 运营深化** | 场景→技能绑定（告警类型自动挂载）；技能使用趋势看板；L3 资源引用；http-5xx 等更多内置技能；MCP 工具调用埋点统计；"MCP 部署为集群 service"一键引导（默认生成"ops-net + Worker 代理"免发布端口形态）；Worker 代理配置管理端下发（gRPC） | `mlops`、前端 |

依赖关系：P1a 与 P1b 相互独立可并行；P2 依赖 P1b；P3 依赖 P1a。

---

## 十、附：关键时序（技能自动触发）

```
用户: "prod 的 gateway 服务一直重启，帮我看看"
  │ POST /api/v1/ainexus/chat {messages, use_mcp}
  ▼
api/ainexus.go: 计量(chat) → 模型解析 → OpenAIHandler
  ▼
conv 组装: [user 消息] + appendSkillIndex(conv,"chat") → [system:技能目录][user:...]
  ▼
ReAct 轮1: LLM 匹配 "svc-unhealthy" → tool_call read_skill{"name":"svc-unhealthy"}
  ▼
read_skill → 返回 playbook 全文（16KB 内）
  ▼
ReAct 轮2..n: 按 playbook 调 mcp_cluster_prod_get_service / get_service_logs /
              get_resource_usage …（集群 MCP 20 工具）
  ▼
结论输出（引用工具结果 + 技能判据）；read_skill 计数 +1
```
