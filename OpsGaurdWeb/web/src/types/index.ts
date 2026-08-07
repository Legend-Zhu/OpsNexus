/**
 * 管理端领域类型：与后端路由/模型一一对应。
 */

/** 集群摘要（列表项，与后端 store.Cluster.Public() 对应） */
export interface ClusterSummary {
  name: string
  project_id?: string
  worker_url: string
  mcp_url?: string
  desc?: string
  status: 'online' | 'offline' | 'unknown'
  last_seen: string
  has_token?: boolean
  /** 最近一次健康探测错误（离线原因，Public() 保留） */
  err?: string
  /** 纳管清单（外部对象声明） */
  inventory?: InventoryConfig
}

/** 集群详情 */
export interface Cluster extends ClusterSummary {
  // 后端返回即 Public() 视图：无 token 字段
}

/** 接入集群请求体 */
export interface AddClusterPayload {
  name: string
  project_id?: string
  worker_url: string
  /** 缺省由后端推导为 {worker_url}/mcp（MCP 端点与 Manager Worker 同址）；仅独立部署网关时显式覆盖 */
  mcp_url?: string
  token?: string
  desc?: string
  /** 纳管清单（接入时可选配置） */
  inventory?: InventoryConfig
}

/** 项目（管理层级第一层：项目 → 集群） */
export interface Project {
  id: string
  name: string
  desc?: string
  created_at: string
}

/** 项目视图（含成员集群统计） */
export interface ProjectView {
  project: Project
  cluster_count: number
  clusters?: { name: string; status: string; last_seen?: string; worker_url?: string }[]
}

/** 集群节点（管理层级：集群 → 节点，对应 Worker /api/v1/nodes） */
export interface ClusterNode {
  id: string
  hostname: string
  role: 'manager' | 'worker'
  state: string
  availability: string
  addr: string
  leader: boolean
  managerReachability?: string
  reachable: boolean
  cpuCores: number
  memBytes: number
  cpuPercent: number
  memPercent: number
  containerCount: number
}

/** 节点宿主机进程（对应 Worker /api/v1/local/processes） */
export interface ProcessInfo {
  pid: number
  name: string
  cmdline?: string
  state: string
  memKb: number
  cpuPercent: number
}

/** 节点上的容器（swarm 任务 + standalone docker run，如 r-nacos） */
export interface ContainerInfo {
  id: string
  name: string
  image: string
  state: string
  type: 'service' | 'standalone'
  service?: string
  ports?: string
}

/** SSO 状态（公开端点 /api/v1/auth/sso/status） */
export interface SSOStatus {
  local: boolean
  sso: { enabled: boolean; issuer?: string; frontendUrl?: string }
}

/** 登录用户 */
export interface Me {
  username: string
  role: string
}

/** 工作负载（swarm service 视图，与后端 workerproxy.Workload 对应） */
export interface Workload {
  id: string
  name: string
  image?: string
  mode?: 'replicated' | 'global'
  replica?: string
  running?: number
  desired?: number
  ports?: PortMapping[]
  labels?: Record<string, string>
}

/** 端口映射 */
export interface PortMapping {
  publishedPort: number
  targetPort: number
  protocol?: string
  mode?: string
}

/** 工作负载详情（服务 + tasks + 健康） */
export interface WorkloadDetail extends Workload {
  tasks: WorkloadTask[]
  healthy: number
  /** 最近一次部署/更新的配置快照（svccfg；编辑服务时预填，无快照则省略） */
  config?: string
}

/** 任务视图 */
export interface WorkloadTask {
  id: string
  slot?: number
  nodeId?: string
  state: string
  desiredState: string
  message?: string
  err?: string
  containerId?: string
  exitCode?: number
}

/** 异步编排操作（部署/缩放/重启/删除，对应 Worker Operation） */
export interface Operation {
  id: string
  type: string
  service: string
  status: 'pending' | 'running' | 'healthy' | 'done' | 'failed' | 'partial' | 'canceled'
  startedAt: string
  finishedAt?: string
  serviceId?: string
  error?: string
  steps?: string[]
  replicas?: number
  mode?: string
}

/** 日志行（SSE 事件体） */
export interface LogLine {
  ts: string
  stream: 'stdout' | 'stderr'
  line: string
}

/** 监控事件（来自 Worker webhook ingest，P3） */
export interface IngestEvent {
  id: string
  cluster?: string
  ts: string
  service: string
  type: string
  level: 'info' | 'warn' | 'error'
  msg: string
  detail?: string
}

/** 告警（按 cluster+service+type 聚合，P3） */
export interface Alert {
  id: string
  cluster: string
  service: string
  type: string
  level: 'info' | 'warn' | 'error'
  title: string
  status: 'active' | 'acked' | 'recovered'
  count: number
  first_ts: string
  last_ts: string
  acked_by?: string
  acked_at?: string
  recovered_at?: string
  /** 排查回写：关联排查会话次数 / 最近一次排查 id */
  investigations?: number
  last_investigation_id?: string
}

/** 排查会话（对话式 troubleshoot 落库） */
export interface Investigation {
  id: string
  alert_id?: string
  cluster?: string
  title: string
  messages: string // [{role, content, tools?}] JSON
  conclusion?: string
  model?: string
  created_at: string
  updated_at: string
}

/** 巡检报告投递设置（系统设置 → 巡检报告） */
export interface PatrolReportSetting {
  mode: 'always' | 'anomaly' | 'off'
  channel_ids: string[]
}

/** 密钥（巡检 flow 拨测账号等；列表不返回 value） */
export interface Secret {
  name: string
  value?: string // 仅写入时提交，读取/列表不返回
  created_at: string
  updated_at: string
}

/** 镜像仓库服务信息 */
export interface RegistryInfo {
  enabled: boolean
  hostname: string
  port: string
  docker_available: boolean
  retention_per_repo: number
  max_upload_mb: number
  auth_enabled: boolean
}

/** 镜像 tag 视图 */
export interface RegistryTag {
  tag: string
  digest: string
  size: number
  updated: string
}

/** 镜像仓库（repo）视图 */
export interface RegistryRepo {
  name: string
  tags: RegistryTag[]
}

/** 构建任务（页面传包构建） */
export interface BuildTask {
  id: string
  status: 'PENDING' | 'EXTRACTING' | 'BUILDING' | 'PUSHING' | 'CLEANING' | 'SUCCESS' | 'FAILED'
  progress: number // -1 = 失败
  image: string
  name: string
  tag: string
  logs?: string[]
  error?: string
  created_at: string
  finished_at?: string
}

/** 节点资源统计（Worker /local/stats） */
export interface NodeStats {
  node: string
  containers: {
    containerId: string
    service?: string
    taskId?: string
    cpuPercent: number
    memPercent: number
    memUsageBytes: number
    memLimitBytes: number
  }[]
}

/** 巡检流程（P5） */
export interface Patrol {
  id: string
  name: string
  description?: string
  cron: string
  enabled: boolean
  yaml: string
  created_at: string
  updated_at: string
}

/** 巡检执行记录 */
export interface PatrolRun {
  id: string
  patrol_id: string
  seq: number
  started_at: string
  finished_at?: string
  status: 'running' | 'success' | 'failed'
  error?: string
  anomalies?: { check: string; cluster?: string; service?: string; ok: boolean; message?: string; data?: string }[]
  report_id?: string
}

/** 巡检 AI 报告 */
export interface PatrolReport {
  id: string
  patrol_run_id: string
  patrol_id?: string
  ai_summary: string
  model?: string
  created_at: string
}

/** 通知渠道（P6） */
export interface NotifyChannel {
  id: string
  type: 'feishu' | 'sms' | 'webhook'
  name: string
  config: Record<string, unknown>
  via_proxy: boolean
  proxy_url?: string
  enabled: boolean
  created_at: string
}

/** 通知策略（P6） */
export interface NotifyPolicy {
  level: string
  channel_ids: string[]
  receivers?: string[]
}

/** 发送记录 */
export interface NotifyRecord {
  id: string
  seq: number
  ts: string
  channel_id: string
  alert_id?: string
  title?: string
  target?: string
  status: 'success' | 'failed'
  error?: string
}

/** 监控配置（对应 store.Monitoring / config.Monitoring，AlertRule 与 InventoryItem 共用） */
export interface Monitoring {
  enabled?: boolean
  portChecks?: PortCheck[]
  httpChecks?: HTTPCheck[]
  logChecks?: LogCheck[]
  resourceThresholds?: ResourceThreshold[]
}

export interface PortCheck {
  port: string
  protocol?: string
  interval?: string
  timeout?: string
  retries?: number
}

export interface HTTPCheck {
  url: string
  method?: string
  headers?: Record<string, string>
  expectedStatus?: number[]
  expectedBody?: string
  interval?: string
  timeout?: string
}

export interface LogCheck {
  pattern: string
  level?: string
  ignore?: string[]
  action?: string
}

export interface ResourceThreshold {
  metric: 'cpu' | 'memory'
  threshold: number
  action?: string
}

/** 告警规则（P6：管理 Worker monitoring config） */
export interface AlertRule {
  cluster: string
  /** 纳管对象 name（swarm service name 或 inventory item name） */
  service: string
  monitoring: Monitoring
  updated_at: string
}

/** 纳管对象声明（对应 store.InventoryItem） */
export interface InventoryItem {
  name: string
  type: 'standalone-container' | 'host-service'
  ref: string
  node?: string
  category: string
  desc?: string
  monitoring?: Monitoring
}

/** 纳管清单配置（对应 store.InventoryConfig） */
export interface InventoryConfig {
  items: InventoryItem[]
}

/** 纳管对象视图（GET /inventory 返回，含实时状态） */
export interface InventoryView {
  name: string
  type: 'swarm-service' | 'standalone-container' | 'host-service'
  category?: string
  source: 'swarm' | 'inventory'
  node?: string
  status: string
  image?: string
  ports?: string
  desc?: string
  /** 仅 inventory 条目：standalone-container → 容器名；host-service → host:port */
  ref?: string
}

/** 用户（P6） */
export interface User {
  id: string
  username: string
  role: string
  enabled: boolean
  created_at: string
  /** IdP 扩展字段（OIDC profile claims 下发用） */
  email?: string
  display_name?: string
  groups?: string[]
}

/** IdP Client（注册到 OpsGaurd IdP 的 OIDC 客户端 / 依赖方） */
export interface IdpClient {
  id: string
  name: string
  redirect_uris: string[]
  grant_types?: string[]
  response_types?: string[]
  scopes?: string[]
  token_ttl?: string
  /** true = PKCE-only 公共客户端（无 secret，Worker/MCP 用） */
  public: boolean
  created_at: string
  updated_at?: string
}

/** 监控事件（来自 Worker /api/v1/events） */
export interface EventItem {
  id: string
  /** worker 侧 SQLite 自增 seq（倒序分页游标：加载更多传上一页最小 seq） */
  seq?: number
  ts: string
  service: string
  type: string
  level: string
  msg: string
  detail?: string
}

/** 审计条目（来自 Worker /api/v1/audit） */
export interface AuditItem {
  id: string
  ts: string
  actor: string
  action: string
  service?: string
  command?: string
  ok: boolean
  detail?: string
}

/** AiNexus 模型 */
export interface AINexusModel {
  id: string
  object: string
  owned_by: string
}

/** 模型选择器数据（/ainexus/api/models：name/provider/type） */
export interface AINexusModelInfo {
  name: string
  provider: string
  type: string
}

// ---- AiNexus 网关运行时配置（系统设置 → AI 排查网关，P7） ----
// 与后端 internal/api/ainexusconfig.go 的脱敏视图对应（api_key 仅布尔标记）。

/** 网关配置（脱敏视图） */
export interface AINexusConfig {
  enabled: boolean
  /** 网关是否已加载（enabled 且构建成功） */
  active: boolean
  providers: AINexusProvider[]
  /** AI 排查网关默认模型（模型池之一；空 = 用首个可用） */
  default_model?: string
  tools: AINexusTools
  agent: AINexusAgent
  mcp_servers: AINexusMCPServer[]
}

/** Provider（api_key 留空 = 沿用已保存值） */
export interface AINexusProvider {
  name: string
  type: 'openai_compatible' | 'anthropic_compatible'
  base_url: string
  api_key?: string
  api_key_set?: boolean
  models: AINexusModelSpec[]
}

/** 模型配置项（区别于 AINexusModel 的网关原生模型条目） */
export interface AINexusModelSpec {
  name: string
  display_name?: string
  max_tokens?: number
  temperature?: number
}

/** 内置工具配置 */
export interface AINexusTools {
  command: { enabled: boolean; allowed_commands?: string[]; timeout: string; work_dir?: string }
  http_request: { enabled: boolean; timeout: string }
  file_read: { enabled: boolean; max_size: number }
}

/** Agent 参数 */
export interface AINexusAgent {
  max_tool_rounds: number
  parallel_tool_calls: boolean
  max_context_tokens?: number
  keep_tool_rounds?: number
}

/** MCP Server 配置（headers/env 值留空 = 沿用已保存值） */
export interface AINexusMCPServer {
  name: string
  transport: string
  url?: string
  command?: string
  args?: string[]
  env?: Record<string, string>
  env_keys?: string[]
  headers?: Record<string, string>
  headers_keys?: string[]
  /** 集群 manager 的自动 MCP（服务端自动连接，页面只读） */
  cluster?: boolean
}
