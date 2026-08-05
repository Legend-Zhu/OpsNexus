import { get, post, del, put } from './http'
import type {
  AddClusterPayload,
  AINexusModelInfo,
  Alert,
  AlertRule,
  AuditItem,
  Cluster,
  ClusterNode,
  ClusterSummary,
  EventItem,
  Me,
  NodeStats,
  NotifyChannel,
  NotifyPolicy,
  NotifyRecord,
  Operation,
  Patrol,
  PatrolReport,
  PatrolRun,
  ProcessInfo,
  Project,
  ProjectView,
  SSOStatus,
  User,
  Workload,
  WorkloadDetail,
} from '@/types'

// ---- 项目（管理层级第一层：项目 → 集群） ----
export const projectApi = {
  list: () => get<{ items: ProjectView[] }>('/v1/projects'),
  get: (id: string) => get<ProjectView>(`/v1/projects/${id}`),
  create: (body: { name: string; desc?: string }) => post<{ project: Project }>('/v1/projects', body),
  update: (id: string, body: { name?: string; desc?: string }) =>
    put<{ project: Project }>(`/v1/projects/${id}`, body),
  remove: (id: string) => del<{ deleted: string }>(`/v1/projects/${id}`),
}

// ---- 集群管理 ----
export const clusterApi = {
  list: () => get<{ items: ClusterSummary[] }>('/v1/clusters'),
  get: (name: string) => get<Cluster>(`/v1/clusters/${name}`),
  add: (body: AddClusterPayload) => post<Cluster>('/v1/clusters', body),
  remove: (name: string) => del<{ removed: string }>(`/v1/clusters/${name}`),
}

// ---- 集群节点（管理层级：集群 → 节点 → 容器/进程） ----
export const nodeApi = {
  list: (cluster: string) => get<{ items: ClusterNode[] }>(`/v1/clusters/${cluster}/nodes`),
  processes: (cluster: string, nodeId: string, params?: { top?: string; limit?: number }) =>
    get<{ node: string; total: number; processes: ProcessInfo[] }>(
      `/v1/clusters/${cluster}/nodes/${nodeId}/processes`,
      { params },
    ),
}

// ---- 工作负载（经 Worker） ----
export const workloadApi = {
  list: (cluster: string) => get<{ items: Workload[] }>(`/v1/clusters/${cluster}/workloads`),
  get: (cluster: string, service: string) =>
    get<WorkloadDetail>(`/v1/clusters/${cluster}/workloads/${service}`),
  deploy: (cluster: string, body: { config: string }) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads`, body),
  scale: (cluster: string, service: string, replicas: number) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads/${service}/scale`, { replicas }),
  restart: (cluster: string, service: string) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads/${service}/restart`),
  remove: (cluster: string, service: string) =>
    del<Operation>(`/v1/clusters/${cluster}/workloads/${service}`),
  operation: (cluster: string, id: string) =>
    get<Operation>(`/v1/clusters/${cluster}/workloads/ops/${id}`),
  logsUrl: (cluster: string, service: string, follow = false) =>
    `/api/v1/clusters/${cluster}/workloads/${service}/logs?follow=${follow}`,
}

// ---- 监控事件 / 审计 ----
export const eventApi = {
  list: (cluster: string, params?: { type?: string; limit?: number }) =>
    get<EventItem[]>(`/v1/clusters/${cluster}/events`, { params }),
  audit: (cluster: string, params?: { action?: string; limit?: number }) =>
    get<AuditItem[]>(`/v1/clusters/${cluster}/audit`, { params }),
}

// ---- 告警中心（P3） ----
export const alertApi = {
  list: (params?: { cluster?: string; status?: string }) =>
    get<{ items: Alert[] }>('/v1/alerts', { params }),
  ack: (id: string) => post<Alert>(`/v1/alerts/${id}/ack`),
  recover: (id: string) => post<Alert>(`/v1/alerts/${id}/recover`),
}

// ---- 集群监控（节点资源，P3） ----
export const monitorApi = {
  metrics: (cluster: string) => get<NodeStats>(`/v1/clusters/${cluster}/metrics`),
}

// ---- 智能巡检（P5） ----
export const patrolApi = {
  list: () => get<{ items: Patrol[] }>('/v1/patrols'),
  get: (id: string) => get<Patrol>(`/v1/patrols/${id}`),
  create: (body: Partial<Patrol>) => post<Patrol>('/v1/patrols', body),
  update: (id: string, body: Partial<Patrol>) => put<Patrol>(`/v1/patrols/${id}`, body),
  remove: (id: string) => del<{ deleted: string }>(`/v1/patrols/${id}`),
  run: (id: string) => post<PatrolRun>(`/v1/patrols/${id}/run`),
  runs: (id: string, limit = 20) => get<{ items: PatrolRun[] }>(`/v1/patrols/${id}/runs`, { params: { limit } }),
  reports: (id: string, limit = 20) => get<{ items: PatrolReport[] }>(`/v1/patrols/${id}/reports`, { params: { limit } }),
}

// ---- 通知中心（P6） ----
export const notifyApi = {
  channels: () => get<{ items: NotifyChannel[] }>('/v1/notify/channels'),
  createChannel: (body: Partial<NotifyChannel>) => post<NotifyChannel>('/v1/notify/channels', body),
  updateChannel: (id: string, body: Partial<NotifyChannel>) => put<NotifyChannel>(`/v1/notify/channels/${id}`, body),
  removeChannel: (id: string) => del<{ deleted: string }>(`/v1/notify/channels/${id}`),
  policies: () => get<{ items: NotifyPolicy[] }>('/v1/notify/policies'),
  upsertPolicy: (level: string, body: Partial<NotifyPolicy>) => put<NotifyPolicy>(`/v1/notify/policies/${level}`, { level, ...body }),
  records: (limit = 50) => get<{ items: NotifyRecord[] }>('/v1/notify/records', { params: { limit } }),
}

// ---- 告警规则（P6） ----
export const alertRuleApi = {
  list: () => get<{ items: AlertRule[] }>('/v1/alertrules'),
  upsert: (body: Partial<AlertRule>) => put<AlertRule>('/v1/alertrules', body),
  apply: (body: Partial<AlertRule>) => post<AlertRule>('/v1/alertrules/apply', body),
  remove: (cluster: string, service: string) => del<{ deleted: string }>(`/v1/alertrules/${cluster}/${service}`),
}

// ---- 认证 / 用户（P6：本地登录 + SSO/OIDC） ----
export const authApi = {
  login: (username: string, password: string) => post<{ token: string }>('/v1/auth/login', { username, password }),
  me: () => get<Me>('/v1/auth/me'),
  ssoStatus: () => get<SSOStatus>('/v1/auth/sso/status'),
  ssoLoginUrl: () => '/api/v1/auth/sso/login',
  users: () => get<{ items: User[] }>('/v1/users'),
  createUser: (body: { username: string; password: string; role?: string }) => post<User>('/v1/users', body),
}

// ---- AiNexus 异常排查 ----
export const ainexusApi = {
  health: () => get<{ status: string }>('/v1/ainexus/health'),
  models: () => get<{ models: AINexusModelInfo[] }>('/v1/ainexus/models'),
  chat: (body: unknown) => post<unknown>('/v1/ainexus/chat', body),
  // 深度排查：告警 → 上下文注入 → 内嵌 Agent（SSE 流式）
  investigateUrl: () => '/api/v1/ainexus/investigate',
}
