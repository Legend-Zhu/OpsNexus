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

/** 告警规则（P6：管理 Worker monitoring config） */
export interface AlertRule {
  cluster: string
  service: string
  monitoring: {
    enabled?: boolean
    portChecks?: { port: string; protocol?: string; interval?: string; timeout?: string; retries?: number }[]
    httpChecks?: { url: string; method?: string; expectedStatus?: number[]; expectedBody?: string; interval?: string; timeout?: string }[]
    logChecks?: { pattern: string; level?: string; ignore?: string[]; action?: string }[]
    resourceThresholds?: { metric: 'cpu' | 'memory'; threshold: number; action?: string }[]
  }
  updated_at: string
}

/** 用户（P6） */
export interface User {
  id: string
  username: string
  role: string
  enabled: boolean
  created_at: string
}

/** 监控事件（来自 Worker /api/v1/events） */
export interface EventItem {
  id: string
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
