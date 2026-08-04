/**
 * 管理端领域类型（骨架）：与后端路由/模型对应，业务迭代时扩充字段。
 */

/** 集群摘要（列表项） */
export interface ClusterSummary {
  name: string
  desc?: string
  status: 'online' | 'offline' | 'unknown'
  workerUrl?: string
  nodeCount?: number
  serviceCount?: number
}

/** 集群详情 */
export interface Cluster {
  name: string
  desc?: string
  workerUrl: string
  mcpUrl?: string
  token?: string
  status: 'online' | 'offline' | 'unknown'
}

/** 工作负载（swarm service 视图） */
export interface Workload {
  name: string
  cluster: string
  image?: string
  replicas?: string
  status?: string
  ports?: string
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
