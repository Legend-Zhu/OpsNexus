import { get, post, del } from './http'
import type {
  AddClusterPayload,
  Alert,
  AINexusModel,
  AuditItem,
  Cluster,
  ClusterSummary,
  EventItem,
  NodeStats,
  Operation,
  Workload,
  WorkloadDetail,
} from '@/types'

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

// ---- AiNexus 异常排查 ----
export const ainexusApi = {
  health: () => get<{ status: string }>('/v1/ainexus/health'),
  models: () => get<AINexusModel[]>('/v1/ainexus/models'),
  chat: (body: unknown) => post<unknown>('/v1/ainexus/chat', body),
}
