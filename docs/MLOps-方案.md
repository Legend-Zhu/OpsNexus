# MLOps 方案（提示词 / 模型 / 费用管理）

> 版本：v0.4  
> 日期：2026-08-19  
> 状态：方案评审中（已按当前代码复核，待实施）  
> 修订说明：v0.3 将 MLOps 定位为围绕内嵌 AiNexus 网关的运营管理。本版进一步按 `OpsGaurdWeb/server` 当前实现校准调用边界、存储迁移、流式 usage、Agent 多轮计量、提示词结构、模型热重载、权限审计和通知机制。  
> 关联代码：`OpsGaurdWeb/server/internal/ainexus`、`internal/ainexusrt`、`internal/api/ainexusconfig.go`、`internal/api/ainexus.go`、`internal/api/investigate.go`、`internal/patrol/service.go`、`internal/store`、`internal/notify`。  

---

## 一、定位、范围与现状

### 1.1 定位

MLOps 是 OpsGaurd 管理面内嵌 AiNexus 网关的**运营层**，包含三块能力：

1. **Prompt Hub**：提示词场景化、版本化、变量化、预览、激活和回滚；
2. **模型运营**：模型启停、场景模型绑定、价格、健康和用量透视；
3. **费用管理**：按底层模型调用记录 token、价格快照、聚合、预算和通知。

MLOps 不引入训练平台、模型部署平台或新的推理服务。实际 LLM 请求仍由现有 AiNexus provider 发往配置的 OpenAI 兼容或 Anthropic 兼容服务；MLOps 不新增一套外部推理调用链。

### 1.2 当前真实架构

AiNexus 不是独立进程，而是管理端进程内的 Go 模块：

- 网关构建：`internal/ainexus/server/server.go`；
- provider：`internal/ainexus/provider/openai.go`、`anthropic.go`；
- ReAct Agent：`internal/ainexus/agent/agent.go`；
- 运行时配置和热重载：`internal/ainexusrt/service.go`；
- LevelDB 运行时配置：`store/ainexus.go` 的 `ainexus/runtime`；
- 网关配置管理：`internal/api/ainexusconfig.go`；
- 管理端启动装配：`cmd/server/main.go`。

当前存在多个 LLM 入口，不能简单视为一次统一的业务调用：

| 入口 | 当前路径 | 当前行为 | MLOps 场景归属 |
|---|---|---|---|
| 对话排查 | `POST /api/v1/ainexus/chat` | 管理端剥离 `alert_id`、`use_mcp` 后调用 OpenAI handler；普通请求可自带 system message | `chat` |
| 深度排查 | `POST /api/v1/ainexus/investigate` | 拉取告警、事件、审计、日志，组装 system/user 两条消息后流式调用 Agent | `investigate`，拆分为 system/user 模板 |
| 巡检报告 | `patrol.Service.buildReport` → `Server.Summarize` | 巡检代码生成事实 prompt；`Summarize` 另加一个固定 system message | `patrol_report`，拆分为 system 与业务输入 |
| 会话压缩 | `agent/llm_compressor.go` | 上下文超预算时由 Agent 使用同一 provider 额外调用 LLM | `compress` |
| 原生 OpenAI | `POST /ainexus/v1/chat/completions` | 直接挂载内嵌 OpenAI handler | `native_chat`，默认不改写客户端 system |
| 原生 Anthropic | `POST /ainexus/v1/messages` | 直接挂载内嵌 Anthropic handler | `native_chat`，默认不改写客户端 system |
| 连通性测试 | `POST /api/v1/ainexus/config/test` | 对指定 provider/model 发起真实非流式请求 | `health`，是否计费需单独口径 |

因此，Prompt Hub 只收编服务端拥有的固定提示词；普通 chat 客户端传入的 system message、原生兼容端点的客户端 prompt 和巡检 YAML 中用户填写的业务附加要求，不自动改写为全局模板。

### 1.3 当前代码中的三个重要事实

1. `BuildInvestigateMessages` 当前返回 system/user 两条消息，且 MCP 开关决定 system 内容，证据为空时会省略对应 user 段落；模板设计必须保留消息角色和条件结构。
2. Agent 一次业务请求可能执行多轮 provider 调用；工具轮次和上下文压缩均可能额外产生 LLM 请求，费用不能只在业务请求结束时读取最后一轮 usage。
3. 当前 `store.schemaVersion` 为 2，迁移表只有 1、2。新增 bucket 仍必须提供 migration 3，不能只把常量改成 3。

### 1.4 目标

1. 不改变未启用 MLOps 时的既有网关行为；
2. 让受控服务端提示词可以安全修改、预览和回滚；
3. 让每一次底层 provider 调用都可计量，明确区分已计量、未计量、失败和未计价；
4. 让模型运营配置与现有网关配置保持单一事实来源；
5. 让费用报表、预算通知、权限和审计在单实例 LevelDB 架构下可落地。

---

## 二、实施前置修复（P0）

以下事项必须在 Prompt Hub 和费用验收前完成，否则“行为不变”和“费用完整”均无法成立。

### 2.1 schema migration 必须完整

现有 `internal/store/store.go`：

- `schemaVersion = 2`；
- migration 1、2 已登记；
- `migrate()` 会逐版本查找迁移函数。

升级到 schema 3 时必须增加：

```go
3: func(s *Store) error {
    // 新增 prompt/mlpricing/mlusage/mlusage_day/mlbudget/mlops_audit bucket，
    // 仅推进版本号，无历史数据转换。
    return s.putVersion(3)
},
```

“新 bucket 不需要迁移历史数据”不等于“不需要 migration”。已有数据库缺少 migration 3 时会在启动阶段报 `no migration for version 3`。

### 2.2 旧 YAML 的模型启停兼容

当前 `ModelConfig` 使用普通 `bool`。新字段 `Enabled bool` 不能直接加入后就把缺失字段解释为 false，因为旧 YAML 反序列化后无法区分“字段缺失”和“明确禁用”。

必须采用以下任一方案：

- 使用 `*bool` 反序列化，缺失时归一化为 true；
- 自定义 YAML 反序列化，在缺失时写入 true；
- 在运行时配置加载和页面保存时统一做旧配置升级，缺失值转换为 true。

归一化必须同时覆盖：

- `config.yaml` 初始配置；
- `ainexus/runtime` 已保存 YAML；
- `PUT /api/v1/ainexus/config` 的旧前端请求；
- 热重载构建的新配置。

### 2.3 修复流式 usage 丢失

当前 OpenAI provider 已声明 `StreamOptions.IncludeUsage`，但 `processStream` 对独立 usage chunk 仅执行 `_ = chunk.Usage`，没有保存或发送，见 `internal/ainexus/provider/openai.go`。因此“流式 usage 已处理”不成立。

P0 要求：

- OpenAI provider 在收到 usage chunk 时更新当前调用 usage；
- finish 事件和 usage 事件无论先后都能合并；
- 最终关闭 channel 前发送一次包含最终 usage 的完成事件；
- Anthropic `message_delta`、`message_stop` 不得重复结算同一调用；
- provider 错误、HTTP 错误、客户端取消、上游流意外结束都必须产生可结算的结束状态。

### 2.4 修复压缩器模型和 context

当前 `llm_compressor.go` 将摘要会话模型写成 `summary`，但实际使用的是当前 provider，可能不在模型池中；调用还使用 `context.Background()`，无法继承请求取消和超时。

P0 要求：

- 压缩请求使用实际配置的模型名；
- 压缩器接口接收可取消 context；
- 压缩调用携带 `scenario=compress` 和父 operation_id；
- 压缩失败仍退化为硬删，但失败应被计量为一次失败或按产品口径明确排除；
- 压缩 prompt 的 active 模板由 Prompt Resolver 提供。

### 2.5 热重载生命周期

`ainexusrt.Service.Update` 会构建新 Server，持久化成功后关闭旧 Server 并替换指针。UsageSink、PromptResolver 和相关计量队列必须由管理端服务持有，而不是只绑定在某一个短生命周期 Server 上。

要求：

- 新 Server 构建时挂载同一个 sink/resolver；
- 旧 Server 已经开始的请求可以完成并上报；
- 关闭旧 Server 不关闭共享 sink；
- sink 停止时先排空有界队列，再允许进程退出；
- 热重载失败时旧 Server、旧配置和计量链路继续工作。

### 2.6 修复普通通知成功记录缺失

现有 `notify.Service.sendAndRecordGeneric` 在 `NextSeq` 成功时提前返回，导致成功的通用通知不保存 `NotifyRecord`。预算通知将复用 `notify.Send`，因此 P0 必须修复该逻辑并增加回归测试。

---

## 三、提示词管理（Prompt Hub）

### 3.1 场景定义

场景是服务端提示词的分类和接入点。按当前代码拆分为：

| 场景 key | 对应代码 | 管理范围 |
|---|---|---|
| `investigate_system` | `api/investigate.go` 的 system 消息 | 管理；MCP 条件段作为结构化可选段 |
| `investigate_user` | `api/investigate.go` 的告警/证据 user 消息 | 管理；证据字段请求时渲染 |
| `compress_system` | `agent/llm_compressor.go` | 管理；对话内容仍由压缩调用动态提供 |
| `patrol_system` | `ainexus/server/server.go` 的 `Summarize` 固定 system 消息 | 管理 |
| `custom` | 新增 | 管理；仅供实验/调试，不自动接入线上入口 |

巡检 YAML 的 `report.prompt` 是流程作者提供的业务附加要求，仍由 `patrol.Service` 作为数据输入拼入报告请求；它不是全局 Prompt Hub 模板，也不应覆盖 `patrol_system`。

普通聊天客户端传入的 system message，以及 `/ainexus/v1/chat/completions`、`/ainexus/v1/messages` 的原生客户端 system 字段，默认不由 Prompt Hub 接管。若未来需要强制平台 system prompt，必须另立“策略注入”设计并明确兼容性影响。

### 3.2 结构化模板模型

单个版本不能只保存一段 `Template` 文本，因为 investigate 当前有多个角色消息。建议模型如下：

```go
type Prompt struct {
    ID            string          `json:"id"`
    Scenario      string          `json:"scenario"`
    Name          string          `json:"name"`
    ActiveVersion int             `json:"active_version"`
    Builtin       bool            `json:"builtin"`
    Versions      []PromptVersion `json:"versions"`
    CreatedAt     time.Time       `json:"created_at"`
    UpdatedAt     time.Time       `json:"updated_at"`
}

type PromptVersion struct {
    Version   int                    `json:"version"`
    Messages  []PromptMessageTemplate `json:"messages"`
    Variables []PromptVariable       `json:"variables,omitempty"`
    Note      string                 `json:"note,omitempty"`
    CreatedAt time.Time              `json:"created_at"`
    CreatedBy string                 `json:"created_by,omitempty"`
}

type PromptMessageTemplate struct {
    Role     string `json:"role"`     // system | user
    Template string `json:"template"` // text/template 原文
    Optional string `json:"optional,omitempty"` // 条件段标识；非空时由 resolver 决定是否渲染
}

type PromptVariable struct {
    Name     string `json:"name"`
    Required bool   `json:"required"`
    Note     string `json:"note,omitempty"`
}
```

约束：

- 一个内置 scenario 只能存在一个 Prompt 记录；custom 可以多个，但必须有稳定 ID；
- 内置默认模板由代码提供，首次使用时可作为只读 v1 展示，也可在首次自定义时落库；
- active version 必须唯一；激活、回滚、恢复默认使用 Store 原子写；
- 保存新版本时携带 `expected_active_version`，版本冲突返回 409，避免并发编辑覆盖；
- 内置场景不可删除，只能恢复到代码默认；恢复默认不删除用户版本；
- custom 删除前必须确认不是线上绑定场景，删除和绑定更新使用同一事务；
- 模板版本只描述文本和变量，不绑定模型。场景模型绑定属于独立的模型运营配置。

### 3.3 渲染生命周期

启动或激活时只做：

1. 读取 active 版本；
2. 校验 message role、变量 schema 和模板语法；
3. 编译并缓存 `text/template`。

请求到达后才做最终渲染，因为告警、事件、审计、日志、MCP 是否可用都在请求阶段才确定。渲染输入由场景 resolver 生成：

- investigate：alert、events、audit、logs、use_mcp；
- compress：待压缩消息、最大摘要字数；
- patrol：巡检名称、检查结果、异常列表和 YAML report.prompt；
- custom：调用方明确提交的变量字典。

渲染失败处理：

- 保存阶段拒绝语法错误、未知变量和非法 role；
- 预览阶段返回结构化 `missing_vars` 和错误位置；
- 线上 active 模板渲染失败时记录错误并使用代码默认模板，不让坏模板阻断告警排查；
- 默认模板回退必须可观测，报表记录 `prompt_fallback=true`。

### 3.4 模板安全约束

保存和渲染必须限制：

- 变量白名单；支持显式定义的嵌套字段，不接受任意反射对象；
- 缺失变量默认是错误，只有变量 schema 标记 `required=false` 时允许为空；
- 模板最大字节数、单次渲染最大输出字节数和预览输入大小；
- 不向模板暴露可调用方法、文件、网络、Store、请求上下文或密钥对象；
- 用户输入作为数据传入，不拼接成新的模板再执行；
- HTML/Markdown/JSON 等展示内容不改变 provider 消息角色，不因预览而执行外部动作。

### 3.5 Prompt API

所有接口使用当前管理端标准响应包络：

```json
{"code": 200, "message": "ok", "data": {}}
```

建议接口：

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/mlops/prompts` | 创建 custom 模板；请求含 scenario/name/messages/variables |
| GET | `/api/v1/mlops/prompts` | 列表；返回 active version、builtin、updated_at |
| GET | `/api/v1/mlops/prompts/:id` | 详情和版本历史 |
| PUT | `/api/v1/mlops/prompts/:id` | 保存新版本；支持 expected_active_version 和 activate |
| POST | `/api/v1/mlops/prompts/:id/render` | 预览；限制输入和输出大小，返回 messages/missing_vars |
| POST | `/api/v1/mlops/prompts/:id/activate` | 激活指定版本；支持并发版本校验 |
| POST | `/api/v1/mlops/prompts/:id/reset` | 内置场景恢复代码默认，保留用户版本 |
| DELETE | `/api/v1/mlops/prompts/:id` | 仅 custom 可删除 |

写接口必须 admin；预览和列表按管理端认证策略开放。所有写操作记录管理审计，但不记录完整 prompt/API key，只记录版本号、hash、摘要和结果。

---

## 四、费用管理：按 provider call 计量

### 4.1 计量单位

一次用户业务请求不是一次 LLM 调用。Agent 工具循环、流式 ReAct 继续轮次和上下文压缩均可能调用 provider 多次。因此底层主记录单位必须是：

> 每次 `Provider.ChatCompletion` 或 `Provider.ChatCompletionStream` 调用对应一条 call 记录。

在 call 之上可按 `operation_id` 聚合一次聊天、一次 investigate、一次 patrol report 的总调用数、token、费用和错误。

### 4.2 场景 taxonomy

| scenario | 产生位置 | 是否默认计费 |
|---|---|---|
| `chat` | `/api/v1/ainexus/chat` | 是 |
| `investigate` | `/api/v1/ainexus/investigate` | 是 |
| `native_chat` | `/ainexus/v1/chat/completions`、`/v1/messages` | 是 |
| `patrol_report` | `Server.Summarize` | 是 |
| `compress` | Agent 上下文压缩器 | 是否计费需确认，技术上必须可单列 |
| `health` | `/api/v1/ainexus/config/test` | 是否计费需确认，默认不纳入业务费用 |

scenario 通过 request context 或显式 `CallMetadata` 传入 provider 调用链，不能使用全局变量。每次 Agent 请求生成一个 `operation_id`，每轮 provider 调用生成唯一 `call_id`，并记录 `parent_call_id` 或 `round`。

### 4.3 UsageSink 接口

UsageSink 属于 AiNexus server 层的可选依赖，server 不依赖 mlops 包：

```go
type UsageSink interface {
    Record(UsageRecord)
}

type UsageRecord struct {
    OperationID       string
    CallID            string
    ParentCallID      string
    Provider          string
    Model             string
    Scenario          string
    EntryPoint        string
    Round             int
    PromptTokens      int
    CompletionTokens  int
    TotalTokens       int
    UsagePresent      bool
    OK                bool
    Status            string // success | provider_error | canceled | usage_missing | internal_error
    Error             string
    StartedAt         time.Time
    FinishedAt        time.Time
    LatencyMs         int64
}
```

要求：

- 成功、provider 错误、HTTP 错误、取消、Agent 达到轮次上限都可上报；
- usage 缺失不伪造为 0 token 的正常成功，必须有 `usage_present=false` 或 `status=usage_missing`；
- sink 不得阻塞用户响应。实现采用有界异步队列，队列满时记录丢弃计数并告警，或使用明确的同步降级策略；
- `call_id` 幂等，重试和流关闭重复事件不能重复入账；
- sink 消费失败不影响原始 LLM 响应，但必须有日志和可观测指标；
- 旧 Server 与热重载后的新 Server 共享同一 sink 生命周期。

### 4.4 Agent 和流式计量

非流式 `Agent.Run` 必须累加每一轮 `ChatCompletion` 的 usage，而不是只返回最后一轮 response 的 usage。流式 `RunStream` 必须在每一轮结束时结算该轮，并最终按 operation 聚合。

P0 已明确修复 provider stream usage：

- OpenAI usage 独立 chunk 不能丢弃；
- finish reason 和 usage 的先后顺序不影响结算；
- Anthropic `message_delta` 的 usage 与 `message_stop` 不得重复结算；
- 上游异常结束、客户端取消和工具调用轮次也有明确状态。

上下文压缩是额外 provider call，必须拥有自己的 `call_id` 和 `scenario=compress`，是否计入业务费用由产品口径决定，但不能在技术层悄悄丢失。

### 4.5 明细数据模型

```go
type UsageRecord struct {
    ID               string    `json:"id"`
    Seq              uint64    `json:"seq"`
    OperationID      string    `json:"operation_id"`
    CallID           string    `json:"call_id"`
    ParentCallID     string    `json:"parent_call_id,omitempty"`
    Provider         string    `json:"provider"`
    Model            string    `json:"model"`
    Scenario         string    `json:"scenario"`
    EntryPoint       string    `json:"entry_point,omitempty"`
    Round            int       `json:"round,omitempty"`
    PromptTokens     int       `json:"prompt_tokens"`
    CompletionTokens int       `json:"completion_tokens"`
    TotalTokens      int       `json:"total_tokens"`
    UsagePresent     bool      `json:"usage_present"`
    OK               bool      `json:"ok"`
    Status           string    `json:"status"`
    Error            string    `json:"error,omitempty"`
    Priced           bool      `json:"priced"`
    Currency         string    `json:"currency,omitempty"`
    CostMinor        int64     `json:"cost_minor,omitempty"`
    PricingVersion   string    `json:"pricing_version,omitempty"`
    StartedAt        time.Time `json:"started_at"`
    FinishedAt       time.Time `json:"finished_at"`
    LatencyMs        int64     `json:"latency_ms"`
}
```

费用金额不直接依赖不可控的浮点累计。单价可以使用定点整数或明确的小数精度；历史明细保存价格版本和金额快照，修改单价不重算历史。

### 4.6 单价

```go
type Pricing struct {
    Model        string    `json:"model"`
    Currency     string    `json:"currency"`       // 默认 CNY
    PriceInPerM  string    `json:"price_in_per_m"`  // 元/百万 token，定点字符串
    PriceOutPerM string    `json:"price_out_per_m"` // 元/百万 token，定点字符串
    Version      string    `json:"version"`
    UpdatedAt    time.Time `json:"updated_at"`
}
```

需要区分：

- `priced=true` 且金额为 0：明确配置为免费；
- `priced=false`：没有单价，只统计 token，不统计费用。

单价键以编码后的 model 名称构造，不能直接把未经编码的模型名拼入 LevelDB 层级 key。

### 4.7 日聚合和 key 规则

建议 bucket：

```text
mlusage/<固定宽度 seq>             -> UsageRecord
mlusage_day/<date>/<encoded model>/<encoded scenario> -> UsageDay
mlpricing/<encoded model>           -> Pricing
mlbudget/<yyyy-mm>                  -> Budget
```

约束：

- 明细 seq 使用固定宽度十进制或反向时间序，保证 LevelDB key 排序与时间顺序一致；
- model、scenario 使用 URL-safe base64 或长度前缀编码，禁止 `/`、空字节等破坏 key 结构的字符；
- 日聚合读改写必须在 Store 串行写路径中完成，或由单独原子批处理接口保证不丢计数；
- `UsageDay` 至少包含 calls、success_calls、error_calls、unmetered_calls、prompt_tokens、completion_tokens、priced_calls、cost_minor；
- 所有“日/月/今日”使用配置的业务时区计算，存储时间仍统一使用 UTC；
- 明细按默认 90 天保留；启动时执行一次限量 GC，后台周期执行批量 GC；
- 明细删除使用批量操作并设置单次上限，避免长时间阻塞业务写入；
- 日聚合永久保留会无限增长，必须在产品确认后增加归档、年度汇总或保留策略，不能默认认为永久存储没有成本。

### 4.8 预算

```go
type Budget struct {
    Month        string    `json:"month"`         // 按业务时区的 yyyy-mm
    Currency     string    `json:"currency"`
    LimitMinor   int64     `json:"limit_minor"`
    WarnAt       float64   `json:"warn_at"`       // 0~1
    Notified     []float64 `json:"notified"`       // 例如 [0.8, 1.0]
    UpdatedAt    time.Time `json:"updated_at"`
}
```

写入计量后按当月聚合金额检查预算：

- `limit_minor > 0`；
- `0 < warn_at < 1`；
- 80% 和 100% 等档位按稳定键只通知一次；
- 预算超限只告警，不拦截 LLM 请求；
- 月份切换时创建/读取新预算，旧月通知状态不复用；
- 未计价调用不计入金额，但在报表显示；
- 聚合写失败不影响用户请求，但必须记录待补偿错误。

预算通知复用现有 `notify.Service.Send` 通用发送接口，并保存成功/失败的通知记录。不能假设当前系统已有管理端 audit/alert 通道，也不能用不稳定的时间戳 ID 实现档位去重。通知标题/正文应包含月份、当前金额、预算、触发档位和未计价调用数。

---

## 五、模型运营

### 5.1 单一事实来源

现有 `/api/v1/ainexus/config` 及 `ainexus/runtime` 是 provider/model 配置的唯一事实来源。MLOps 不再维护另一份 provider 表，也不允许旧的“模型配置”页面和 MLOps 页面各自提交一份 providers 后互相覆盖。

建议：

- provider、模型名称、Base URL、API key、模型参数仍由现有网关配置维护；
- MLOps 读取同一运行时配置，增加 enabled 状态、单价、绑定、健康和 usage 运营视图；
- 旧配置页面的 DTO 增加并保留 `enabled`，或改为服务端局部更新模型的接口；
- 所有 JSON 字段遵循当前 `snake_case` 契约，例如 `default_model`、`max_tool_rounds`、`api_key_set`。

### 5.2 模型启停

模型启停不是只修改一条 LevelDB 记录。因为 `Server.initProviders` 会在构建时生成 `modelRoutes`，启停必须：

1. 读取当前配置并做兼容归一化；
2. 修改目标模型 enabled；
3. 校验默认模型、场景绑定和至少一个可用模型规则；
4. 构建新 Server；
5. 持久化成功后原子替换运行时 Server；
6. 旧 Server 按热重载规则关闭。

行为必须明确：

- 显式请求禁用模型：返回稳定的 409/400 业务错误，不发起 provider 请求；
- 默认模型被禁用：若存在可用模型，按配置声明顺序选择稳定 fallback；否则拒绝保存；
- 场景绑定模型被禁用：绑定状态显示异常，运行时回退到默认可用模型并记录 fallback；
- 禁止禁用最后一个可用模型；
- 所有模型禁用时不允许网关保持“可用 enabled”状态，或由产品明确允许进入无模型状态；
- fallback 不依赖 Go map 遍历顺序，必须使用配置顺序或显式排序。

`Enabled` 缺失按 true 处理，显式 false 才表示禁用。

### 5.3 场景模型绑定

场景模型绑定不应继续塞入 Prompt 版本记录。建议单独保存：

```go
type ScenarioBinding struct {
    Scenario  string    `json:"scenario"`
    Model     string    `json:"model"`
    UpdatedAt time.Time `json:"updated_at"`
    UpdatedBy string    `json:"updated_by,omitempty"`
}
```

绑定变更只影响之后的新 operation；历史 usage 以实际调用模型为准。绑定模型不存在或禁用时，API 返回可见状态，运行时按默认可用模型回退并记录 `model_fallback`。

### 5.4 健康

现有 `/api/v1/ainexus/config/test` 会发送一次真实 LLM 请求，不是纯 TCP 探活。`GET /mlops/models/health` 默认只返回最近缓存的测试状态，不隐式遍历所有模型产生费用。

另提供显式测试操作：

- 单 provider/model 测试；
- 超时和并发限制；
- 记录 `last_test_at`、耗时、结果、错误摘要；
- 明确测试是否计入 `scenario=health`，默认不纳入业务费用但可以记录；
- API key、响应正文和完整错误中的敏感信息不得落库。

---

## 六、管理权限、审计与配置

### 6.1 总开关

管理端配置增加：

```yaml
mlops:
  enabled: false
  usage_retain_days: 90
  timezone: Asia/Shanghai
  usage_queue_size: 2048
  usage_gc_interval: 24h
  currency: CNY
```

`mlops.enabled=false` 时：

- 不注册 `/api/v1/mlops/*` 路由；
- 不挂载 UsageSink；
- 不加载用户 active Prompt，所有入口使用代码默认提示词；
- 不启动 usage 聚合、预算和 GC；
- 已存在 MLOps bucket 保留但不写入，重新启用后继续可读；
- AiNexus 原有行为不因 MLOps 关闭而改变。

### 6.2 管理端审计

当前 Worker audit 是从集群查询的操作审计，不等于管理端自身的审计通道。MLOps 需要新增管理审计模型，例如：

```go
type MLOpsAudit struct {
    ID          string    `json:"id"`
    Seq         uint64    `json:"seq"`
    Operator    string    `json:"operator"`
    Action      string    `json:"action"`
    ObjectType  string    `json:"object_type"`
    ObjectID    string    `json:"object_id"`
    BeforeHash  string    `json:"before_hash,omitempty"`
    AfterHash   string    `json:"after_hash,omitempty"`
    Result      string    `json:"result"`
    Error       string    `json:"error,omitempty"`
    CreatedAt   time.Time `json:"created_at"`
}
```

Prompt 正文、API key、provider headers、通知凭据不得直接写入审计；只记录版本、hash、脱敏摘要和结果。

以下写操作必须 `RequireRole("admin")`：

- Prompt 创建、保存、激活、恢复默认、删除；
- 模型启停、场景绑定、单价修改；
- 预算创建、修改、删除；
- 触发健康测试（只读 GET 不触发真实请求）。

报表和明细可按管理端现有认证策略开放，但必须限制时间范围、分页大小和敏感字段。

### 6.3 配置并发与热重载

Prompt active、模型启停、场景绑定和价格更新使用 Store 原子写，并在 API 层携带 expected version 或 updated_at 防止并发覆盖。网关配置更新仍通过 `ainexusrt.Service.Update`，不得绕过运行时服务直接写 `ainexus/runtime`。

---

## 七、存储变更

### 7.1 Bucket

| Bucket | Key | 说明 |
|---|---|---|
| `prompt` | `prompt/<id>` | 结构化模板及版本历史 |
| `mlpricing` | `mlpricing/<encoded-model>` | 单价、币种、价格版本 |
| `mlusage` | `mlusage/<fixed-seq>` | provider call 明细，按保留策略清理 |
| `mlusage_day` | `mlusage_day/<date>/<encoded-model>/<encoded-scenario>` | 日聚合 |
| `mlbudget` | `mlbudget/<yyyy-mm>` | 月预算和通知档位 |
| `mlbinding` | `mlbinding/<scenario>` | 场景模型绑定 |
| `mlops_audit` | `mlops_audit/<fixed-seq>` | MLOps 管理操作审计 |
| `meta/version` | binary uint64 | schema 版本，v3 必须有 migration |

### 7.2 Store API 要求

新增 Store 操作必须遵循现有单实例 mutex 和 WriteBatch 约定：

- 序列号递增与明细写入尽量在同一串行路径；
- 明细、日聚合、预算档位状态在一次结算中要么全部成功，要么有可重试状态；
- List API 不把全量明细无上限读入内存；
- 明细查询使用固定宽度 key + 游标，支持按时间、model、scenario、status 过滤；
- GC 使用批量删除，不阻塞长时间的普通写入；
- 价格读取和历史 cost 快照不互相覆盖。

---

## 八、REST API 一览

全部 API 前缀为 `/api/v1/mlops`，使用当前管理端标准响应包络和 `snake_case`。

### 8.1 Prompt

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/mlops/prompts` | 创建 custom 模板 |
| GET | `/mlops/prompts` | 列表 |
| GET | `/mlops/prompts/:id` | 详情和版本历史 |
| PUT | `/mlops/prompts/:id` | 保存新版本 |
| POST | `/mlops/prompts/:id/render` | 受限变量预览 |
| POST | `/mlops/prompts/:id/activate` | 激活/回滚 |
| POST | `/mlops/prompts/:id/reset` | 内置场景恢复默认 |
| DELETE | `/mlops/prompts/:id` | 仅 custom |

### 8.2 模型

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/mlops/models` | 当前网关模型池、provider、enabled、默认标记、价格和本月 usage |
| PUT | `/mlops/models/:name/enable` | 通过 runtime service 热重载启用 |
| PUT | `/mlops/models/:name/disable` | 通过 runtime service 热重载禁用 |
| PUT | `/mlops/models/:name/pricing` | 修改价格快照配置 |
| GET | `/mlops/models/health` | 最近健康缓存，不隐式调用 provider |
| POST | `/mlops/models/:name/health` | 显式真实连通性测试 |
| GET | `/mlops/bindings` | 场景模型绑定 |
| PUT | `/mlops/bindings/:scenario` | 修改场景绑定 |

模型名需要 URL 编码；列表按 provider 配置顺序稳定返回。

### 8.3 费用

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/mlops/costs/overview` | 今日/本月 operation、call、token、费用、未计量、未计价和错误 |
| GET | `/mlops/costs/trend` | 按业务时区的日序列；限制 `days` 上限 |
| GET | `/mlops/costs/detail` | call 明细分页；支持 model/scenario/status/from/to/cursor |
| GET | `/mlops/costs/operations/:id` | 一次业务 operation 下的所有 provider call |
| POST | `/mlops/budgets` | 新增或覆盖指定月份预算 |
| GET | `/mlops/budgets` | 预算、实际金额、使用率、通知档位 |
| DELETE | `/mlops/budgets/:month` | 删除预算 |
| GET | `/mlops/audit` | MLOps 管理审计分页 |

费用查询必须限制最大时间范围和分页大小；缺少价格时返回 token 和 `priced=false`，不能把金额 0 当成已计价。

---

## 九、前端

新增导航入口“ MLOps ”，三个 tab：

1. **提示词**：场景列表、结构化 system/user 编辑器、变量 schema、语法校验、预览、版本历史、激活/回滚、恢复默认；
2. **模型**：读取现有 AiNexus 配置的 provider/model，展示启停、默认模型、场景绑定、单价、usage、费用和最近健康结果；
3. **费用**：今日/月度摘要、按模型/场景统计、日趋势、call 明细、operation 下钻、未计量/未计价筛选、预算。

前端约束：

- provider/model 本体仍只有一个编辑来源；MLOps 模型 tab 不再提交整份 providers；
- 现有“系统设置 → 模型配置”和“AI 排查网关”入口要么保留为唯一配置页，要么改为只读/跳转，不能双向覆盖；
- 前后端沿用现有 `snake_case` JSON 契约和标准 response envelope；
- 写操作前端根据当前用户角色隐藏或禁用，但后端 admin 守卫必须保留；
- 费用明细显示 `usage_present`、`priced`、status、provider、model、scenario 和 operation_id；
- 健康按钮明确显示“会发送一次真实测试请求”，不会由列表 GET 自动触发。

---

## 十、实施分期与验收

### P0：兼容性和计量基础

- [ ] 增加 schema migration 3；
- [ ] 旧 YAML 的 `Enabled` 缺失值归一化为 true；
- [ ] 修复 OpenAI stream usage 丢弃和跨 finish 合并；
- [ ] 统一 Anthropic/OpenAI stream 完成事件和错误状态；
- [ ] 修复压缩器模型名、context 和 scenario；
- [ ] 设计 operation/call/call_id、context metadata 和共享 UsageSink；
- [ ] 修复热重载时 sink/resolver 生命周期；
- [ ] 修复通用通知成功记录缺失；
- [ ] 增加 P0 回归测试。

验收：旧 runtime YAML 可正常启动；旧模型默认可用；OpenAI/Anthropic 流式 usage 可读取；Agent 多轮和压缩调用各有 call 记录；热重载前后计量不中断。

### P1：结构化 Prompt Hub

- [ ] `prompt` bucket、结构化消息版本模型和 Store 原子操作；
- [ ] Prompt Resolver、模板编译缓存、变量白名单和输出限制；
- [ ] investigate system/user 接入，保证 MCP 开关和空证据行为不变；
- [ ] compress system 接入；
- [ ] patrol system 接入，保留 YAML `report.prompt` 作为业务输入；
- [ ] Prompt API、admin 权限和管理审计；
- [ ] 前端提示词 tab。

验收：

- 修改 investigate v2 后 system/user 角色不变；
- 证据有/无、MCP 开/关均按预期渲染；
- 语法错误、未知变量、超限模板拒绝保存；
- v1/v2 激活、回滚、并发冲突和恢复默认可验证；
- 坏模板线上回退代码默认并产生可观测记录。

### P2：provider-level usage 和费用

- [ ] `mlusage`、`mlusage_day`、`mlpricing` Store 模型和迁移；
- [ ] provider call 级 UsageSink；
- [ ] operation 聚合和日聚合；
- [ ] 价格版本、计价标记和金额舍入；
- [ ] 明细分页、趋势、汇总和 operation 下钻；
- [ ] 启动/周期 GC；
- [ ] 费用 API 和前端费用 tab。

验收：

- 真实 chat、investigate、patrol、Agent 多轮分别可查；
- OpenAI/Anthropic stream usage 不丢；
- 失败、取消、压缩、usage 缺失和未计价均有明确状态；
- 重复完成事件不重复入账；
- 并发写日聚合不丢计数；
- 价格修改不影响历史 cost 快照；
- 明细滚动清理生效且不删除日聚合。

### P3：模型运营和预算

- [ ] `Enabled` 兼容字段、统一配置 DTO 和局部更新；
- [ ] 模型启停经 runtime service 热重载；
- [ ] 默认模型、场景绑定、禁用回退和稳定排序；
- [ ] 健康缓存和显式测试；
- [ ] `mlbinding`、`mlbudget`、`mlops_audit`；
- [ ] 预算计算、档位去重、通知记录和费用页面模型 tab；
- [ ] admin 权限和审计端点。

验收：

- 禁用模型不会继续发请求；
- 默认/绑定模型禁用时按规则稳定回退；
- 禁止禁用最后可用模型；
- 健康 GET 不产生隐藏请求；
- 80%/100% 预算档位各通知一次，成功通知有记录；
- 热重载失败不影响旧网关；
- 旧模型配置页面保存不会重置 enabled。

### P4：前端和文档收尾

- [ ] MLOps 导航和三 tab；
- [ ] 清理重复配置入口或改为只读；
- [ ] 标准 response envelope、分页、权限和错误提示；
- [ ] 更新操作手册和本方案状态；
- [ ] 前端 `npm run build`、服务端 `go test ./...` 和端到端回归。

---

## 十一、风险与边界

| 项 | 说明 | 对策 |
|---|---|---|
| 现有流式 usage 丢失 | OpenAI stream 当前明确丢弃独立 usage chunk；其他 provider 也可能缺失 | P0 先统一 provider stream usage 语义；缺失标记为未计量 |
| Agent 多轮低估 | 一次业务请求包含多次 provider call，最后一轮 usage 不能代表总量 | provider call 级记录，operation 级聚合 |
| 压缩额外费用 | 上下文压缩会额外调用模型，且当前模型/context 实现有问题 | 单列 compress call，产品确认是否计入业务费用 |
| Prompt 结构改变 | investigate 依赖 system/user 角色、MCP 条件和证据段落 | 结构化消息模板、默认模板快照和回归测试 |
| 旧 YAML 兼容 | bool 缺失值反序列化为 false | 兼容归一化，缺失等价 true |
| 热重载竞态 | 旧/新 Server 切换时请求和 sink 可能交叉 | sink/resolver 由外层服务持有，旧请求排空后关闭 |
| 价格失真 | 单价可能错误或上游调价 | 费用仅为估算；保存价格版本和历史快照 |
| 未计价/未计量 | 没有 usage 或没有价格时金额不可用 | 分别展示 usage_present、priced、调用次数和 token |
| 明细膨胀 | LevelDB 不适合无限明细和永久高频聚合 | 明细 GC；聚合归档/长期保留策略待确认 |
| 预算通知重复 | 多线程、重启或补偿可能重复触发 | 月份+档位稳定键、原子 Notified 更新、幂等发送记录 |
| 管理审计缺失 | Worker audit 不是管理端操作审计 | 新增 MLOps audit bucket，敏感内容只存 hash/摘要 |
| 配置入口重复 | 旧模型配置页与 MLOps 页可能互相覆盖 | 单一事实来源，旧页保留/只读/局部更新三选一 |
| 健康测试有费用 | `/config/test` 会发送真实 LLM 请求 | GET 只读缓存，显式 POST 测试并单列 health |
| 时区口径 | 今日/月度/预算边界受时区影响 | 业务时区显式配置，存储时间统一 UTC |
| 不做训练/部署 | MLOps 只管理内嵌网关运营数据 | 不新增训练、部署或外部推理服务 |

---

## 十二、待产品确认事项

以下事项需要产品口径确认，技术实现不应再依赖隐含假设：

1. 费用币种是否固定 CNY，金额展示保留几位小数；
2. 业务时区是否固定 `Asia/Shanghai`，还是允许部署级配置；
3. `compress` 和 `health` 是否进入费用总额，是否只在 usage 报表单列；
4. 费用明细是否需要 CSV 导出；
5. 日聚合永久保留采用年度汇总、归档还是有限保留；
6. 预算通知使用哪些已配置渠道，是否允许独立于告警等级策略；
7. native chat 是否未来需要平台 system prompt 强制注入；
8. MLOps 模型 tab 与现有“系统设置 → 模型配置/AI 排查网关”的最终页面归属。

当前服务端基线测试：

```text
go test ./...
```

现有测试通过只说明当前代码基线正常，不覆盖本方案新增的 stream usage、多轮 Agent、热重载、旧 YAML、migration 3、权限审计、预算通知、日聚合并发和 GC。上述测试必须纳入对应阶段验收。
