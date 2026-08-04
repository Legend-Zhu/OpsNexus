/**
 * 管理端领域类型（骨架）：与后端路由/模型对应，业务迭代时扩充字段。
 */

/** 集群摘要（列表项，与后端 store.Cluster.Public() 对应） */
export interface ClusterSummary {
  name: string
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
  worker_url: string
  mcp_url?: string
  token?: string
  desc?: string
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
