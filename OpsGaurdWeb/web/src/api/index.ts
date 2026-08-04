import { get, post, del } from './http'
import type { AddClusterPayload, Cluster, ClusterSummary, Workload, EventItem, AuditItem, AINexusModel } from '@/types'

/**
 * API 模块骨架：函数签名已按后端路由定义，业务实现待迭代。
 */

// ---- 集群管理（多集群，类 Rancher） ----
export const clusterApi = {
  list: () => get<{ items: ClusterSummary[] }>('/v1/clusters'),
  get: (name: string) => get<Cluster>(`/v1/clusters/${name}`),
  add: (body: AddClusterPayload) => post<Cluster>('/v1/clusters', body),
  remove: (name: string) => del<{ removed: string }>(`/v1/clusters/${name}`),
}

// ---- 工作负载（经 Worker） ----
export const workloadApi = {
  list: (cluster: string) => get<Workload[]>(`/v1/clusters/${cluster}/workloads`),
  get: (cluster: string, service: string) =>
    get<Workload>(`/v1/clusters/${cluster}/workloads/${service}`),
}

// ---- 监控事件 / 审计 ----
export const eventApi = {
  list: (cluster: string, params?: { type?: string; limit?: number }) =>
    get<EventItem[]>(`/v1/clusters/${cluster}/events`, { params }),
  audit: (cluster: string, params?: { action?: string; limit?: number }) =>
    get<AuditItem[]>(`/v1/clusters/${cluster}/audit`, { params }),
}

// ---- AiNexus 异常排查 ----
export const ainexusApi = {
  health: () => get<{ status: string }>('/v1/ainexus/health'),
  models: () => get<AINexusModel[]>('/v1/ainexus/models'),
  chat: (body: unknown) => post<unknown>('/v1/ainexus/chat', body),
}
