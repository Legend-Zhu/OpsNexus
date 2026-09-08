# 管理端 MCP 使用手册

> 版本：v1.0
> 日期：2026-09-07
> 适用：OpsGaurd 管理端（server）开启 `mcp:` 配置后的版本
> 关联：`docs/管理端MCP-Server-设计方案.md`（设计定稿 v0.5，实现落点对照）、《OpsGaurd-场景操作手册.md》（页面操作侧）

---

## 一、这是什么

管理端 MCP Server 把 OpsGaurd 的全部管理能力（项目、集群纳管、服务部署、镜像构建、告警排查、巡检、告警规则、通知、命令执行）暴露为标准 **MCP（Model Context Protocol）** 端点。任何支持 MCP 的 AI 助手——**ZCode、Claude Desktop、Cursor 等**——配置一次后，即可用自然语言完成完整运维闭环：

> "新建项目 demo，把 10.0.1.5 纳管为 prod 集群（gRPC 9080 / HTTP 8080），把 gw.zip 构建成镜像 gw:v1，然后在 prod 上部署 gateway 服务跑 2 个副本、8080 对外。"
>
> "prod 的 gateway 一直 OOM 重启，是不是上周改的缓存代码引起的？你去集群拉日志和内存曲线确认一下。"
>
> "给 prod 和 staging 各建一个巡检任务，每天早上 9 点跑，CPU 超 85% 就报。"

端点一览：

| 端点 | 用途 | 鉴权 |
|---|---|---|
| `POST/GET /mcp` | MCP 协议端点（Streamable HTTP，stateless） | MCP token（独立于页面登录） |
| `GET /.well-known/oauth-protected-resource` | RFC 9728 资源元数据（助手自动发现用） | 公开 |
| `POST /api/v1/mcp/build-upload` | 构建包一次性票据直传 | 一次性 ticket |
| `/api/v1/mcp/*`（tokens/audit/usage/info） | 管理面 REST（页面「MCP 接入」tab 消费） | 平台登录 + admin |

---

## 二、启用与配置

编辑 server 的 `config.yaml`，加入 `mcp:` 段后重启：

```yaml
mcp:
  enabled: true
  max_investigations: 4        # 委托内嵌 AI 排查的并发上限（默认 4）
  exec_enabled: false          # exec 透传开关（默认关）
  cluster_url_allow_cidrs: []  # worker_url/worker_http_url 白名单网段（空 = 不限制）
  tokens:                      # 静态 token 种子（至少一个，否则启动失败）
    - name: zcode
      secret: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
      scope: write
    - name: viewer
      secret: "<openssl rand -hex 32 生成>"
      scope: read
```

要点：

1. **secret 生成**：`openssl rand -hex 32`。原文只在配置文件里，内存中只保留 SHA-256 摘要。
2. **scope 三档**（权限递增）：
   - `read`：查询/观测/拨测/排查会话读取——给报表类、观测类消费者；
   - `write`：含 read 的全部变更（部署、扩缩、删除、构建、规则变更）——给日常使用的助手；
   - `exec`：含 write 外加命令执行（`service_exec` / `node_exec`），**最高危，只发给完全信任的消费者**，且须同时打开 `exec_enabled`。
3. **fail-fast**：`enabled: true` 但没有任何可用 token 时，server 启动直接退出（配置错误启动即暴露）。
4. **平台认证无关**：即使部署处于无认证的内网 bootstrap 模式（未配 `auth.token_secret`），`/mcp` 也强制要求 MCP token——这是给 AI 自主调用的口子，永远上锁。
5. `cluster_url_allow_cidrs` 建议配置（如 `["10.0.0.0/8"]`）：配置后，助手纳管集群时只能填白名单网段内的 Worker 地址（gRPC 与 HTTP 端点都校验），收敛被诱导探测内网的 SSRF 面。

---

## 三、Token 管理（两种方式）

### 方式 A：config.yaml 静态种子（上文的 `tokens:` 列表）

- 不可通过页面/接口删除，只能改配置文件；
- 可在页面**禁用**（临时吊销，不必重启）。

### 方式 B：页面管理（推荐日常用）

**系统设置 → MCP 接入**（admin 可见）：

- **新建 Token**：填名称（小写字母开头，如 `zcode`、`patrol-bot`）+ 选 scope → 创建后 **secret 只显示这一次**，立即复制保存；
- **启停**：列表内开关，热生效（停用后该 token 的助手下一个请求即 401）；
- **删除**：仅限页面创建的 token；同时展示最近使用时间；
- **调用情况**：近 7 天 token × 工具的调用次数表；**最近 30 条写操作审计**（谁、什么工具、什么参数、成败、耗时）。

两种来源在列表中用「config / 页面」标签区分。等价 REST API（admin）：

```
GET    /api/v1/mcp/tokens                  列表（不含 secret）
POST   /api/v1/mcp/tokens                  {name, scope} → 返回 secret（仅此一次）
PUT    /api/v1/mcp/tokens/:name/enabled    {enabled: true|false} 热生效
DELETE /api/v1/mcp/tokens/:name            删除（config 来源拒绝）
GET    /api/v1/mcp/audit?actor=&tool=&limit=   写操作审计
GET    /api/v1/mcp/usage?days=7            调用统计
GET    /api/v1/mcp/info                    启用状态
```

### IdP 签发 token（OAuth，可选）

平台启用了内嵌 IdP 时，`/mcp` 还接受 IdP 签发的 access token（静态 token 未命中时自动回退校验，验签 + 吊销同口径）。token 的 scope 按声明映射：含 `mcp:exec` → exec、含 `mcp:write` → write、**其余一律只读**。审计中此类调用方显示为 `idp:<用户名>`。

---

## 四、助手接入（一次配置）

### ZCode

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

### Claude Desktop / Cursor

同构：`mcpServers.opsguard` + url + Bearer header（或经 mcp-remote 网关中转）。「MCP 接入」页有现成 JSON 模板，**端点 URL 和模板都可一键复制**（URL 按浏览器当前地址自动生成）。

### 接入后建议先跑一遍

> "列出所有集群和活跃告警"

能返回数据即接入成功。全部工具带自描述（参数、何时用、下一步提示），助手不需要额外提示词文件。

---

## 五、能做什么：工具清单（60 个）

所有集群类工具都需要 `cluster` 参数（先 `cluster_list` 查询）。**粗体** = write scope；※ = 需 `confirm=true`；**§** = 需 exec scope + `exec_enabled`。

| 域 | 工具 | 说明 |
|---|---|---|
| **项目**（5） | `project_list` / `project_get` / **`project_create`** / **`project_update`** / **`project_delete`** ※ | 项目 = 集群的分组层；删除只解绑集群，不删集群 |
| **集群纳管**（6） | `cluster_list` / `cluster_get` / **`cluster_add`** / **`cluster_update`** / **`cluster_remove`** ※ / `cluster_events` | add 会先探活 Worker（gRPC Self，须 swarm manager），给了 `worker_http_url` 再探 HTTP `/healthz`（MCP 同端口）；任一探活失败不落库并把错误原样返回 |
| **服务**（10） | `service_list` / `service_get` / **`service_deploy`** / **`service_update`** / **`service_scale`**（→0 ※）/ **`service_restart`** / **`service_remove`** ※ / `service_logs` / `service_operation` / `service_audit` | deploy/update 返回异步 operation id，用 `service_operation` 轮询到 done/failed；`service_get` 附带当前 config 快照，改服务 = 取快照 → 修改 → update |
| **镜像构建**（6） | `image_list` / **`image_delete`** ※ / **`build_upload_begin`** / **`build_submit`** / `build_get` / `build_list` | zip **不经 MCP 传输**，固定三步流程见下节 |
| **告警**（4） | `alert_list` / `alert_get` / **`alert_ack`** / **`alert_recover`** | ack 的认领人显示为 token 名 |
| **排查会话**（4） | `investigation_list` / **`investigation_start`** / `investigation_get` / **`investigation_continue`** | 委托内嵌 AI 深挖，异步（分钟级），见 §七 |
| **诊断**（6） | `node_list` / `node_stats` / `probe_port` / `probe_http` / `probe_flow` / `node_processes` | 从集群内发起的拨测与观测，全部只读、秒级、零 LLM 成本 |
| **巡检**（8） | `patrol_list` / `patrol_get` / **`patrol_create`** / **`patrol_update`** / **`patrol_delete`** ※ / **`patrol_run`** / `patrol_runs` / `patrol_report` | create 的工具描述内嵌 YAML 骨架，助手可代写巡检流程；cron 按北京时间 |
| **告警规则**（5） | `alertrule_list` / `alertrule_get` / **`alertrule_save`** / **`alertrule_apply`** / **`alertrule_delete`** ※ | save 默认保存并立即下发 Worker 生效 |
| **通知**（4） | `notify_channels` / **`notify_channel_save`** / `notify_policies` / `notify_records` | 更新渠道时空 config 沿用旧值，不会抹掉 webhook 凭据 |
| **命令执行**（2）§ | **`service_exec`** / **`node_exec`** | 容器内 / 宿主机（nsenter）命令；仍受 Worker 侧 commandPolicy 黑白名单约束 |

### 只读资源（resources）

支持 resources 的助手可订阅：`opsguard://clusters`、`opsguard://alerts/active`、`opsguard://patrols`、`opsguard://images`、`opsguard://clusters/{name}/services`。

---

## 六、典型流程

### 6.1 构建镜像（三步固定流程）

zip 包不走 MCP 协议（JSON 装不下几十 MB 的包），由助手自动完成：

```bash
# 1. 助手调 build_upload_begin{filename:"gw.zip", size_bytes:52428800}
#    → 返回 upload_id + ticket + 现成的 curl 命令

# 2. 助手在自己的 shell 里执行直传（票据一次性、1 小时有效、
#    上传字节数必须与声明的 size_bytes 完全一致）：
curl -sS -X POST "https://opsguard:8090/api/v1/mcp/build-upload?upload_id=<id>&ticket=<ticket>" \
     -H "Content-Type: application/octet-stream" --data-binary @gw.zip

# 3. 助手调 build_submit{upload_id, name:"gw", tag:"v1"}
#    → build id → build_get 轮询 EXTRACTING→BUILDING→PUSHING→SUCCESS
#    → 得到镜像 registry.opsguard/gw:v1
```

之后 `service_deploy` 的 config 里直接引用 `registry.opsguard/gw:v1`（集群经 Worker 中继拉取，免配置）。

### 6.2 部署与变更

> "把 prod 的 gateway 扩到 5 个副本"

助手流程：`cluster_list` 确认集群名 → `service_scale{cluster:"prod", service:"gateway", replicas:5}` → `service_operation` 轮询到 done → 向你汇报结果。

破坏性操作（删除类 7 个 + 扩容到 0）助手必须**先向你复述影响面并得到确认**才会传 `confirm=true`——如果它没问就执行了，那是助手的提示词问题，审计里可追责到 token。

### 6.3 排障：快查 + 委托深挖（双 agent 协同）

**快查（秒级、零成本）**：`alert_list` → `service_logs` / `node_stats` / `probe_http` 直接看证据。

**深挖（分钟级、带 AI）**——本 MCP 的核心价值：你的本机助手有**代码上下文**，平台内嵌的 AiNexus 有**集群实时证据**，两者互补：

> 你："gateway 一直 OOM 重启，是不是上周改的缓存代码引起的？"
>
> 本机助手：读代码 → 定位嫌疑 → 调 `investigation_start{cluster:"prod", question:"现象：gateway 周期性 OOMKilled。嫌疑：v1.4.2 新增的本地缓存无上限（代码摘要…）。请在集群侧确认内存曲线、OOM 前日志、首次出现时间"}`
>
> → 内嵌 AiNexus 按巡检技能在集群取证 → `investigation_get` 拿回「结论 + 工具调用证据」
>
> → 证据吻合 → 改代码 → 构建发版 → `investigation_continue` 让 AiNexus 持续观察验证

注意分工：**AiNexus 只取证不动集群**；所有变更（restart/发版）由你的助手显式执行，全程审计。

---

## 七、安全模型速查

| 机制 | 行为 |
|---|---|
| scope 三档 | read < write < exec；越权调用返回可解释错误（见 FAQ） |
| confirm 双保险 | 7 个破坏性工具要求 `confirm=true`，且助手须先向用户复述影响 |
| 审计 | 所有写操作落库（actor/tool/参数摘要/成败/耗时），页面可查；`node_exec`/`service_exec` 记录命令原文 |
| exec 三重防护 | `exec_enabled` 开关（默认关）+ scope=exec 专属授权 + Worker commandPolicy 黑白名单 |
| 上传票据 | 一次性、1 小时 TTL、上传字节数必须与声明严格相等；票据泄露也只能完成一次已声明大小的上传 |
| CIDR 白名单 | `cluster_url_allow_cidrs` 非空时，纳管/改端点只接受白名单网段 |
| 体积防线 | 所有工具结果 128KB 头尾保留截断；日志 tail≤1000；事件≤100 条 |
| 用量可见 | 每次调用计数（token×工具），写操作留审计——「谁在什么时候动了什么」永远可答 |

---

## 八、FAQ（助手看到的错误 → 处理办法）

| 助手报错 | 原因与处理 |
|---|---|
| `401 missing or invalid MCP token` | token 错/被禁用/被删除。管理页确认 token 状态；IdP 部署确认 token 未吊销 |
| `token "x" is read-only; tool "y" requires write scope` | 用的是 read token 在做变更。换 write token 或让 admin 调 scope |
| `exec passthrough is disabled (set mcp.exec_enabled=true ...)` | server 侧没开 exec 开关。这是**设计行为**——需要 admin 明确打开 |
| `... exec requires a dedicated scope=exec token` | write token 调 exec 工具。单独签发 exec scope 的 token，并确认消费者可信 |
| `worker_url host "x" is outside mcp.cluster_url_allow_cidrs` | 纳管地址不在白名单。admin 修改 `cluster_url_allow_cidrs` |
| `upload "..." has no file yet: run the curl command first` | 没做第 2 步直传就调了 `build_submit`。按 curl 模板重跑 |
| `uploaded N bytes but build_upload_begin declared M; sizes must match exactly` | 文件在两步之间变了。从 `build_upload_begin` 重新开始 |
| `investigation queue is full (max 4 concurrent)` | 委托排查并发满。稍等重试；长期打满可调大 `max_investigations` |
| `embedded AiNexus gateway is not enabled` | 没配内嵌 AI 网关。到 MLOps/模型接入配好模型后，investigation_* 与 exec 才可用（其余工具不受影响） |
| 助手说要先确认才能删 | 正常行为。复述的影响面没问题就明确回复"确认删除 xxx" |

---

## 九、运维建议

1. **按消费者发 token、按最小权限发 scope**：只读看板的机器人给 read；日常开发的助手给 write；exec scope 只在确有"助手代跑命令"刚需时签发，并配合 `exec_enabled`。
2. **跑一段时间后看「MCP 接入」页**：调用统计能发现没人用的 token（回收）；审计能复盘每一次变更。
3. **`cluster_url_allow_cidrs` 建议启用**，尤其管理端网络可达较大内网时。
4. **生产建议 TLS**：`/mcp` 与页面同域同证书，前置反代终结即可；助手 URL 用 https。
5. ** Worker 侧排查工具**：本 MCP 是"管理面聚合视图"（跨集群、带审计）；单集群内的深度采证（容器 exec、拨测矩阵）由集群 Worker 自己的 `/mcp`（20 工具）提供，二者配合使用。
