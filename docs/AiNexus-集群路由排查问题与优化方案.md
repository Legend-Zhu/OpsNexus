# AiNexus 排查任务不路由到目标集群——全面排查与优化方案

## 1. 问题现象

在「异常排查（AiNexus）」输入如下自由提问：

> 自然灾害集群external-server服务surveilService/api/video/getPreviewURL?cameraIndexCode=…&protocol=wss接口报500，排查一下

Agent **不会去"自然灾害集群"的 Worker 采证排查**：要么纯文字回答，要么在错误/默认的集群上取证，甚至可能拿着"名不副实"的工具做跨集群操作。本文还原链路、定位根因（含同类问题），并给出分阶段优化方案。

## 2. 链路还原与断点

排查对话有两条入口，问题主要出在自由提问链路，但告警链路同样被根因 R1 击穿。

### 2.1 自由提问链路（本次场景）

```
web/src/views/troubleshoot/Index.vue (send)
  → POST /api/v1/ainexus/chat  { stream, use_mcp, messages }   ← 注意：无 alert_id、无 cluster、无 system
  → api/ainexus.go AINexusChat
      alert_id 为空 → 只剥离扩展字段后透传，不做任何集群动作
  → ainexus/handler/openai_handler.go ChatCompletions
      conv := NewConversation(model) + 客户端 messages 原样入栈   ← 全程没有 system 消息
  → agent.RunStream：ReAct 循环，工具 = registry 全量
```

断点：

1. **没有 system 消息**：模型不知道平台纳管了哪些集群、不知道工具按集群隔离、"自然灾害集群"五个字没有任何解析动作。
2. **不连接目标集群 MCP**：`use_mcp=true` 只在告警路径（`alert_id` 非空）生效（`api/ainexus.go:64-85`），自由提问直接跳过 `connectClusterMCP`。
3. registry 里的集群工具来自网关启动时全量重连（`ainexusrt/service.go:309-326 reconnectClustersLocked`），多集群 + 中文名场景下**只剩第一个注册集群的工具**（根因 R1）。

### 2.2 告警排查链路

```
alert_id → alertAndClient（workerproxy 拉事件/审计/日志做证据前缀）
         → connectClusterMCP(alert.Cluster)               ← "已连接"可能名不副实（R1）
         → investigate 模板（system 一句话 + 告警/证据 user 消息）
```

证据注入与模板本身是好的，但若该集群的工具没抢到注册名额，agent 调用的仍是第一个注册集群的 Worker——**采证和变更操作都会落到错误集群**。

## 3. 根因清单（按严重程度）

### R1（P0）中文集群名清洗后工具名碰撞，多集群只剩第一家的工具

- 工具名规则：`mcp_<server名>_<工具名>`，经 `[^a-zA-Z0-9_-]+ → "_"` 清洗（`ainexus/mcp/mcp_tool.go:46-65`）。集群 server 名为 `cluster:<集群名>`（`ainexus/server/server.go:333-355`）。
- 已用等价程序验证：

  ```
  cluster:自然灾害集群 → mcp_cluster__list_services
  cluster:应急指挥集群 → mcp_cluster__list_services   ← 完全同名！
  cluster:prod        → mcp_cluster_prod_list_services
  ```

- Registry 拒绝重名（`ainexus/tool/registry.go:24-33`），`RegisterAllTools` 对重名仅告警并跳过（`ainexus/mcp/manager.go:197-206`）→ **第二个及之后的中文集群，20 个工具全部丢失**。
- `AddMCPCluster` 幂等短路（`server.go:333-341`）：告警排查指定了 B 集群，若 B 的工具名已被 A 占用，连接"成功"但 registry 里还是 A 的工具。
- **后果**：目标集群工具不可见或名不副实；更危险的是 `restart_service / scale_service / remove_service` 这类变更工具可能**静默作用于错误集群**。
- 为什么漏测：清洗测试只有英文集群名（`mcp/manager_test.go:20-41` 用例 `cluster:prod/cluster:dev`）；设计文档示例同为英文（`docs/AiNexus-Skill与MCP-设计方案.md` §工具命名）。集群名 ASCII 校验（`cluster/service.go:20-21,137`）是近期提交 eb4f973 才引入的，**存量中文集群名不受再校验**，继续触发碰撞。

### R2（P0）自由提问链路没有 system prompt / 集群目录 / 目标集群解析

- `AINexusChat` 无 `alert_id` 时零处理透传；`OpenAIHandler.ChatCompletions` 按 `req.Messages` 原样建会话（`openai_handler.go:85-95`）；前端只发 user/assistant 历史，不发 system。
- 集群清单（`cluster.Service.ListStatic()`）服务端现成有，但从未注入模型上下文；消息里的"自然灾害集群"（无论对应注册名 Name 还是描述 Desc）没有任何匹配逻辑。
- 前端排查页只有告警选择器，没有集群选择器。

### R3（P1）请求层没有"目标集群"概念

`/api/v1/ainexus/chat` 与 `/investigate` 的集群只能由 `alert_id` 隐含；`use_mcp` 不带集群参数。自由提问想指定集群，协议上无表达方式。

### R4（P1）MCP 连接失败静默降级

`connectClusterMCP` 失败仅返回 false（`api/investigate.go:142-148`），system 提示词只是少一句话，不告知用户"采证通道未建立"。模型可能在没有实时数据的情形下输出看似确定的根因。

### R5（P1）集群工具快照不刷新、无注销与自愈

`refreshTools` 只在 `AddServer` 时执行一次；Manager 没有 `RemoveServer`，Registry 没有 `Unregister`（设计文档 P2 已规划未实现）。集群删除后工具残留（"幽灵工具"，调用必失败）；Worker 重启后旧连接失效，无重连；工具清单变更不感知。

### R6（P2）多轮会话丢失工具历史

前端 `apiHistory` 只存 user/assistant 文本（`troubleshoot/Index.vue:297`），`oaiMsg` 只有 role/content（`openai_handler.go:54-57`）——追问时此前的采证结果全部丢失，模型要么重新采证、要么结论漂移。（告警会话靠每轮重注入证据前缀兜底，代价是上下文重复膨胀。）

### R7（P2）内置工具与集群排查语义混淆、边界缺失

- 内置 `http_request` 在**管理端进程**发请求，且无任何目标限制（`ainexus/tool/http_request.go` 全文无 allowlist/SSRF 校验）——与设计文档 §4.6 对用户自定义 MCP URL 的私网约束不一致；模型可能拿它去打用户给的接口 URL（管理面根本不通集群业务网络），也可能被诱导外发数据。
- 内置 `command_executor` 在管理容器执行。两者都可能被模型当作"排查集群"的手段，产生误导性结果。

### R8（P2）提示词资产缺少平台拓扑上下文

`investigate_system` 内置模板只有一句话（`mlops/templates.go:50`），不含集群清单、目标集群声明、工具使用守则（先 `get_service` → 日志/复现 → 再下结论；变更类需确认）。Prompt Hub 的渲染数据（`InvestigateData`）也没有集群目录这类动态变量。

## 4. 优化方案

### 阶段一：止血（P0，让"这次排查"走对集群）

**A1. 工具名 slug 化 + 唯一性兜底（修 R1）**

- 在 `AddMCPCluster` 层把 server 名改为 ASCII 安全 slug：`cluster:<name>` → `cluster_<slug>`。slug 规则：保留 `[a-zA-Z0-9._-]`，发生信息损失（含非 ASCII 字符）时追加 `_<fnv32a(name) 前 8 位 hex>`，保证不同集群必不相同。
- 工具描述前缀保留原始集群名（现机制已做：`[MCP:cluster:自然灾害集群]`），供模型与人识别。
- `mcp/manager_test.go` 补中文名/同名清洗/双集群注册用例；新增回归：两个中文集群注册后 registry 工具数 = 2×20 且名字互异。
- 兼容性：工具名每轮全量下发给模型，改名无持久状态；investigations 落库只存工具名展示，无兼容问题。

**A2. 重名注册显性化（修 R1 的静默性）**

- `RegisterAllTools` 遇重名从"告警跳过"升级为：记入 server 状态并在「AI 排查网关 / MCP 接入」页展示工具数对账（每集群应为 20），缺失一眼可见；`/ainexus/api/tools` 增加按 server 分组统计。

**A3. /chat 增加目标集群解析（修 R2/R3）**

- 请求扩展字段 `cluster`；解析优先级：
  1. 请求显式 `cluster`（前端排查页加集群下拉，选中告警时自动带入 `alert.Cluster`）；
  2. 消息文本匹配：对 `clusters.ListStatic()` 的 `Name` 精确匹配（忽略大小写）→ `Desc`/别名包含匹配；
  3. 命中唯一 → 采用；命中多个或零个 → SSE 先发一条澄清话术（"平台有 X/Y 两个集群，你指的是哪个？"）并终止本轮，**不猜**。
- 匹配同时覆盖存量中文名集群（Name 直接匹配）与 ASCII 注册名 + 中文描述（Desc 匹配）两种现网形态。

**A4. 解析成功后的定向动作（修 R2）**

- 调 `connectClusterMCP(target)` 建立目标集群采证通道；
- 注入会话级 system 前缀（先内置于代码，阶段二 Prompt Hub 化为 `chat_system` 场景），内容包含：
  - 本次目标集群声明："用户提到的『自然灾害集群』已解析为集群 `<name>`，以下所有 MCP 工具均作用于该集群"；
  - 平台集群目录（名称/描述/节点数/在线状态，一行一个）；
  - 工具守则：只对目标集群采证；复现 HTTP 接口用 `check_http`/`check_flow`（从节点网络发起，URL 用 `get_service` 返回的 `Endpoint.Ports` 拼接），不要用管理面 `http_request`；变更类工具（restart/scale/update/remove）必须先征得用户确认。

**A5. 连接失败显性化（修 R4）**

- `connectClusterMCP` 失败时在 SSE 流中向用户输出明确提示（"未能连接 X 集群采证通道：原因；本次结论仅基于注入证据，未验证实时状态"），并在排查记录（investigation）落库标记。

### 阶段二：核心体验（P1）

**B1. chat_system 提示词场景化**：新增 `chat_system` 场景进 Prompt Hub（复用现有版本化/激活机制），渲染数据增加 `Clusters`（目录）、`TargetCluster`、`UseMCP` 变量；运营可按部署环境自定义守则。

**B2. 会话级工具 scoping（彻底解决跨集群工具混淆）**：`Agent` 增加可选工具过滤器（或 Registry 支持 `ToolDefinitions(scope)`），`/chat` 按目标集群只暴露该集群 + 通用只读工具。两条路线对比：

| 路线 | 做法 | 取舍 |
| --- | --- | --- |
| 会话 scoping（推荐先做） | registry 全量注册，按会话过滤下发 | 改动小、无新组件；工具名仍带集群 slug |
| 聚合 MCP | 单一 `cluster:*` 聚合 server，工具带 `cluster` 参数 | 工具数恒定 20，模型认知负担最小；需新聚合层 + 参数校验，作为多集群高频交叉排查的演进项 |

**B3. MCP 生命周期治理（修 R5，对齐设计文档 P2 规划）**：Manager 增加 `RemoveServer/ReplaceServer`、Registry 增加 `Unregister(prefix)`；集群删除/编辑时同步清理；心跳探测 + 工具清单变更刷新 + 连续失败指数退避重连（设计文档 §自愈已给出方案）。

**B4. 复现 500 的证据链路验证**：确认 `get_service` 的 `Endpoint.Ports` 足以拼出服务 URL；验证 Worker 容器网络到业务 service 的可达性（overlay 隔离时在守则中说明用已发布端口或 `exec_in_container` 从同网络容器内 curl）。案例接口（getPreviewURL）带 query，`check_http` 已支持完整 URL + `check_flow` 支持多步带 token 的链路，工具面无需新增。

### 阶段三：治理与质量（P2）

**C1. 多轮工具历史保留（修 R6）**：服务端持久化完整会话（含 tool_calls/tool 结果，investigations 表已存 tools 名字，扩展为全文）；前端续传走服务端会话 id 而非客户端文本历史。

**C2. 内置 http_request 边界（修 R7）**：对齐 MCP URL 的私网约束——默认仅允许私网段/已纳管集群地址，显式 `allow_public_url` 才放开；描述中注明"管理面出口，排查集群内接口请用集群 Worker 的 check_http"。

**C3. 工具总量治理**：`MaxTotalTools` 上限、`disabled_tools` 分组排除（设计文档已规划），防多集群 + 用户 MCP 叠加后工具定义挤占上下文。

**C4. 集群别名产品化**：集群配置增加"别名（中文名/口语名）"字段，参与 A3 匹配与目录展示，替代靠 Desc 猜测。

## 5. 测试与回归

- 单测：中文名 slug 唯一性；双（中文）集群注册 40 工具互不覆盖；集群解析器（精确/Desc/多命中/零命中）；重名注册上报。
- 集成：双集群冒烟——对 A 集群告警发起排查，断言 SSE 工具调用全部落在 A 的 MCP（按 server 前缀断言）；自由提问带中文名/描述/无匹配三种输入的路由行为。
- 提示词回归：chat_system 注入后模型首步必为对目标集群的工具调用（用 mock provider 断言首轮 tool_calls）。

## 6. 排期建议与风险

- **第一批（约 1~2 天）**：A1、A2、A3、A4、A5——完成后"自然灾害集群 500"这类自由提问即可走对集群并具备复现能力。
- **第二批**：B1~B4——体验与治理闭环。
- **第三批**：C1~C4。
- 风险提示：
  - 工具名变更对进行中的会话无影响（每轮全量下发），但需在发版说明中提示；
  - 存量中文集群名建议尽快迁移为 ASCII 注册名 + 中文 Desc/别名（A3 的 Desc 匹配可先行兜底），彻底绕开清洗类问题；
  - 会话 scoping 上线前，多集群部署仍存在跨集群变更工具误触风险，建议在 A4 守则中对变更类工具加"必须确认"硬约束，并考虑在 Worker 侧对 `remove_service` 保持双确认现状不变。

## 7. 落地记录（第一批，已完成）

- **A1 工具名唯一化**（`ainexus/mcp/mcp_tool.go`）：server 名含非 ASCII 字符时，工具名尾部追加 fnv32a 指纹前 8 位（如 `mcp_cluster__list_services_b786ef06`），不同中文集群不再同名；ASCII 集群名维持原名不变。测试补中文名清洗用例与双中文集群 6 工具共存回归（`mcp/manager_test.go`）。
- **A2 冲突显性化**（`mcp/manager.go`、`ainexus/server/server.go`）：重名注册升为 ERROR 级日志并带 server 名；`/ainexus/api/tools` 响应新增 `by_source` 分组计数（内置 / `[MCP:server]`），供接入页对账。
- **A3 集群解析**（新增 `api/ainexus_chat.go`）：`/api/v1/ainexus/chat` 新增 `cluster` 扩展字段；解析优先级为显式字段 > 消息文本按 Name 匹配（忽略大小写）> 按 Desc 匹配（整串包含 + 4 字滑窗近似，覆盖「用户说『自然灾害集群』而描述是『自然灾害应急处置集群』」的口语简称）；多命中返回候选、零命中不猜。
- **A4 system 前缀注入**（`api/ainexus.go`、`api/ainexus_chat.go`）：自由提问统一注入平台上下文 system 消息（纳管集群目录（经 `Public()` 脱敏）/ 目标集群声明 / 工具使用守则：只调目标集群工具、check_http 复现 HTTP 问题、变更类须用户确认、结论须引用证据）；解析到目标且 `use_mcp=true` 时定向连接该集群 Worker MCP。
- **A5 失败显性化**（`api/investigate.go`、`api/ainexus.go`）：`connectClusterMCP` 改为返回错误；告警排查与自由提问在采证通道连接失败时，SSE 流开始前先写一条 `【提示】未能连接…`（OpenAI chunk 格式，前端零改动即可见），同时注入提示词限制声明。
- **前端**（`web/src/views/troubleshoot/Index.vue`）：排查页新增目标集群下拉（关联告警时禁用，集群由告警决定），自由提问时随请求发送 `cluster` 字段。
- **与方案的偏差说明**：零命中/多命中未采用"硬终止本轮并反问"的实现，改为在 system 前缀中引导模型先澄清（不调用集群工具）——硬终止会误伤与集群无关的普通提问；歧义方向不变：宁可澄清，不猜集群。
- **验证**：`go test ./internal/...` 全部通过（含新增解析/前缀/防泄密用例）；`npm run build`（vue-tsc + vite）通过。

## 8. 落地记录（第二批，已完成）

- **B1 chat_system 提示词场景化**（`mlops/templates.go`、`mlops/service.go`、`api/ainexus_chat.go`）：新增 `chat_system` 场景进 Prompt Hub（版本化/激活/预览，渲染数据 `ChatData`：Clusters 目录 / HasTarget+Target / HasCandidates+Candidates / ConnectErr）；运营自定义后走模板渲染，渲染失败留痕回退代码内置；内置模板与代码内置组装逐字节等价（`TestChatSystemTemplateMatchesBuiltin` 守护，改任一侧必须同步）。
- **B2 会话级工具 scoping**（`agent/agent.go`、`mcp/mcp_tool.go`、`handler/*`、`api/*`）：`Agent.SetToolFilter` 过滤下发工具定义（执行面不受限）；request ctx 经 `agent.WithToolScope` 声明目标 server，OpenAI/Anthropic 两个 handler 装配 agent 时应用 `mcp.ToolScopeFilter`——模型只见内置工具 + 目标集群工具，杜绝跨集群误调用；`/chat`（告警与自由提问）与 `/investigate` 均已接线（仅 `use_mcp` 且连接成功时生效）。
- **B3 MCP 生命周期治理**（`tool/registry.go`、`mcp/manager.go`、`ainexus/server/server.go`、`api/cluster.go`）：
  - `Registry.Unregister(name)` 按工具名注销；
  - `Manager.RemoveServer(name)` 断开连接并注销该 server 全部工具（幂等）；集群删除接口（`DELETE /v1/clusters/:name`）已接线 `RemoveMCPCluster`；
  - **健康自愈循环**（30s 周期，探测超时 10s，`Initialize` 启动、`Close` 停止）：`ListTools` 探测成功 → 工具清单增量同步（Worker 侧新增注册/移除注销，防幽灵工具）；探测失败 → 整连接重建（关旧客户端 → 重新握手 → 重注册工具），失败保留条目下轮重试。覆盖 Worker 重启、网络抖动、工具变更三类场景。
- **测试**：新增 `TestToolScopeFilter`、`TestRemoveServerUnregistersTools`、`TestHealthCheckSyncsToolChanges`、`TestHealthCheckRebuildsOnFailure`、`TestChatSystemTemplateMatchesBuiltin`；`go test ./internal/...` 全部通过。
- **与方案的偏差说明**：设计文档 P2 规划的自愈"指数退避（5s→…→5min）"暂以固定 30s 周期重试替代——Worker 重启后最多 30s 恢复采证，复杂度低一个量级；如需更快恢复再演进退避策略。
