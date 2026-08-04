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
