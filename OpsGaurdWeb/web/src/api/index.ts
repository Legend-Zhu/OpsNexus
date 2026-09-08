import { get, post, del, put, getToken } from './http'
import type { AxiosProgressEvent } from 'axios'
import type {
  AddClusterPayload,
  AINexusConfig,
  AINexusModelInfo,
  Alert,
  AlertRule,
  AuditItem,
  BuildTask,
  Cluster,
  ClusterNode,
  ContainerInfo,
  ClusterSummary,
  EventItem,
  IdpClient,
  InventoryConfig,
  InventoryView,
  Investigation,
  MCPAuditItem,
  MCPTokenView,
  MCPUsageSummary,
  MLOpsBinding,
  MLOpsBudgetView,
  MLOpsCostsOverview,
  MLOpsCostTrendRow,
  MLOpsHealthResult,
  MLOpsModelsView,
  MLOpsPrompt,
  MLOpsPromptMessage,
  MLOpsPricing,
  MLOpsRenderedMessage,
  MLOpsUsageDetail,
  Me,
  NodeStats,
  NotifyChannel,
  NotifyPolicy,
  NotifyRecord,
  Operation,
  Patrol,
  PatrolReport,
  PatrolReportSetting,
  PatrolRun,
  ProcessInfo,
  Project,
  ProjectView,
  RegistryInfo,
  RegistryRepo,
  SSOStatus,
  Secret,
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
  update: (name: string, body: AddClusterPayload) => put<Cluster>(`/v1/clusters/${name}`, body),
  remove: (name: string) => del<{ removed: string }>(`/v1/clusters/${name}`),
}

// ---- 集群节点（管理层级：集群 → 节点 → 容器/进程） ----
export const nodeApi = {
  list: (cluster: string) => get<{ items: ClusterNode[] }>(`/v1/clusters/${cluster}/nodes`),
  // SSE stream URL for /nodes/stream (EventSource can't set headers, so the
  // token goes in the query). Emits an "init" event (base node list) then one
  // "node" event per node as its stats arrive.
  streamUrl: (cluster: string) => {
    const token = getToken()
    const base = import.meta.env.VITE_API_BASE ?? '/api'
    const q = token ? `?token=${encodeURIComponent(token)}` : ''
    return `${base}/v1/clusters/${cluster}/nodes/stream${q}`
  },
  processes: (cluster: string, nodeId: string, params?: { top?: string; limit?: number; filter?: string }) =>
    get<{ node: string; total: number; processes: ProcessInfo[] }>(
      `/v1/clusters/${cluster}/nodes/${nodeId}/processes`,
      { params },
    ),
  containers: (cluster: string, nodeId: string) =>
    get<{ items: ContainerInfo[] }>(`/v1/clusters/${cluster}/nodes/${nodeId}/containers`),
  restartContainer: (cluster: string, nodeId: string, container: string) =>
    post<{ restarted: string }>(`/v1/clusters/${cluster}/nodes/${nodeId}/containers/restart`, { container }),
}

// ---- 纳管清单（集群纳管的外部对象：standalone 容器 / 宿主机服务） ----
export const inventoryApi = {
  get: (cluster: string) => get<{ items: InventoryView[] }>(`/v1/clusters/${cluster}/inventory`),
  update: (cluster: string, body: InventoryConfig) =>
    put<InventoryConfig>(`/v1/clusters/${cluster}/inventory`, body),
}

// ---- 工作负载（经 Worker） ----
export const workloadApi = {
  list: (cluster: string) => get<{ items: Workload[] }>(`/v1/clusters/${cluster}/workloads`),
  get: (cluster: string, service: string) =>
    get<WorkloadDetail>(`/v1/clusters/${cluster}/workloads/${service}`),
  deploy: (cluster: string, body: { config: string }) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads`, body),
  update: (cluster: string, service: string, body: { config: string }) =>
    put<Operation>(`/v1/clusters/${cluster}/workloads/${service}`, body),
  scale: (cluster: string, service: string, replicas: number) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads/${service}/scale`, { replicas }),
  restart: (cluster: string, service: string) =>
    post<Operation>(`/v1/clusters/${cluster}/workloads/${service}/restart`),
  remove: (cluster: string, service: string) =>
    del<Operation>(`/v1/clusters/${cluster}/workloads/${service}`),
  operation: (cluster: string, id: string) =>
    get<Operation>(`/v1/clusters/${cluster}/workloads/ops/${id}`),
  logsUrl: (cluster: string, service: string, follow = false, tail = 0) =>
    `/api/v1/clusters/${cluster}/workloads/${service}/logs?follow=${follow}${tail > 0 ? `&tail=${tail}` : ''}`,
}

// ---- 监控事件 / 审计 ----
export const eventApi = {
  list: (cluster: string, params?: { type?: string; limit?: number; after_seq?: number }) =>
    get<{ items: EventItem[] }>(`/v1/clusters/${cluster}/events`, { params }),
  audit: (cluster: string, params?: { action?: string; limit?: number }) =>
    get<{ items: AuditItem[] }>(`/v1/clusters/${cluster}/audit`, { params }),
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

// ---- 告警规则（P6；list 可按集群过滤） ----

/** 删除结果：删除会级联停止已生效的监控；停止失败时默认保留规则，force 强删则如实报告未停止 */
export interface AlertRuleDeleteResult {
  deleted: string
  stopped: boolean
  stopError?: string
}

export const alertRuleApi = {
  list: (cluster?: string) => get<{ items: AlertRule[] }>('/v1/alertrules', { params: cluster ? { cluster } : {} }),
  upsert: (body: Partial<AlertRule>) => put<AlertRule>('/v1/alertrules', body),
  apply: (body: Partial<AlertRule>) => post<AlertRule>('/v1/alertrules/apply', body),
  remove: (cluster: string, service: string, force = false) =>
    del<AlertRuleDeleteResult>(`/v1/alertrules/${cluster}/${service}`, force ? { params: { force: true } } : undefined),
}

// ---- 认证 / 用户（P6：本地登录 + SSO/OIDC） ----
export const authApi = {
  login: (username: string, password: string) => post<{ token: string }>('/v1/auth/login', { username, password }),
  me: () => get<Me>('/v1/auth/me'),
  // 修改自己的本地密码（SSO 账号无本地口令，后端返回 400 提示）
  changePassword: (body: { old_password: string; new_password: string }) =>
    put<{ changed: string }>('/v1/auth/password', body),
  ssoStatus: () => get<SSOStatus>('/v1/auth/sso/status'),
  ssoLoginUrl: () => '/api/v1/auth/sso/login',
  // 用户管理（admin）：列表 / 新增 / 重置密码
  users: () => get<{ items: User[] }>('/v1/users'),
  createUser: (body: { username: string; password: string; role?: string }) => post<User>('/v1/users', body),
  resetPassword: (username: string, body: { new_password: string }) =>
    put<{ reset: string }>(`/v1/users/${username}/password`, body),
}

// ---- IdP（OpsGaurd 作为 OIDC 身份提供者）：client 管理（admin） ----
export const idpApi = {
  listClients: () => get<{ items: IdpClient[] }>('/v1/idp/clients'),
  createClient: (body: { name: string; redirect_uris: string[]; scopes?: string[]; public: boolean; token_ttl?: string }) =>
    post<{ client: IdpClient; secret: string }>('/v1/idp/clients', body),
  getClient: (id: string) => get<IdpClient>(`/v1/idp/clients/${id}`),
  updateClient: (id: string, body: Partial<Pick<IdpClient, 'name' | 'redirect_uris' | 'scopes' | 'token_ttl'>>) =>
    put<IdpClient>(`/v1/idp/clients/${id}`, body),
  deleteClient: (id: string) => del<{ deleted?: string }>(`/v1/idp/clients/${id}`),
  rotateSecret: (id: string) => post<{ secret: string }>(`/v1/idp/clients/${id}/rotate-secret`),
}

// ---- AiNexus 异常排查 ----
export const ainexusApi = {
  health: () => get<{ status: string }>('/v1/ainexus/health'),
  models: () => get<{ models: AINexusModelInfo[] }>('/v1/ainexus/models'),
  chat: (body: unknown) => post<unknown>('/v1/ainexus/chat', body),
  // 对话式排查（SSE 流式；body 支持 alert_id/use_mcp 扩展字段）
  chatUrl: () => '/api/v1/ainexus/chat',
  // 深度排查：告警 → 上下文注入 → 内嵌 Agent（SSE 流式）
  investigateUrl: () => '/api/v1/ainexus/investigate',
  // 网关运行时配置（系统设置 → AI 排查网关）
  config: () => get<AINexusConfig>('/v1/ainexus/config'),
  updateConfig: (body: Partial<AINexusConfig>) => put<AINexusConfig>('/v1/ainexus/config', body),
  testConfig: (body: { name?: string; type: string; base_url: string; api_key?: string; model: string }) =>
    post<{ ok: boolean; latency_ms: number; error?: string; model?: string }>('/v1/ainexus/config/test', body),
}

// ---- 排查会话（对话式 troubleshoot 落库） ----
export const investigationApi = {
  save: (body: { alert_id?: string; cluster?: string; title: string; messages: string; conclusion?: string; model?: string }) =>
    post<Investigation>('/v1/investigations', body),
  update: (id: string, body: { title?: string; messages: string; conclusion?: string; model?: string }) =>
    put<Investigation>(`/v1/investigations/${id}`, body),
  get: (id: string) => get<Investigation>(`/v1/investigations/${id}`),
  list: (alertId?: string) =>
    get<{ items: Investigation[] }>('/v1/investigations', { params: alertId ? { alert_id: alertId } : {} }),
}

// ---- 全局设置 ----
export const settingsApi = {
  // 巡检报告投递（智能巡检 → 报告投递）
  patrolReport: () => get<PatrolReportSetting>('/v1/settings/patrol-report'),
  updatePatrolReport: (body: PatrolReportSetting) => put<PatrolReportSetting>('/v1/settings/patrol-report', body),
}

// ---- 密钥（巡检 flow 拨测账号；列表不返回值） ----
export const secretApi = {
  list: () => get<{ items: Secret[] }>('/v1/secrets'),
  save: (name: string, value: string) => put<{ name: string }>(`/v1/secrets/${name}`, { value }),
  remove: (name: string) => del<{ deleted: string }>(`/v1/secrets/${name}`),
}

// ---- 内嵌镜像仓库 + 页面传包构建 ----
export const registryApi = {
  info: () => get<RegistryInfo>('/v1/registry/info'),
  images: () => get<{ items: RegistryRepo[] }>('/v1/registry/images'),
  deleteTag: (name: string, tag: string) =>
    del<{ deleted: string }>(`/v1/registry/images/${name}/tags/${tag}`),
  builds: () => get<{ items: BuildTask[] }>('/v1/registry/builds'),
  build: (id: string) => get<BuildTask>(`/v1/registry/builds/${id}`),
  // 上传构建包（multipart，大文件放宽超时到 10 分钟）
  submitBuild: (form: FormData, onUploadProgress?: (event: AxiosProgressEvent) => void) =>
    post<BuildTask>('/v1/registry/builds', form, { timeout: 600000, onUploadProgress }),
}

// ---- MLOps（提示词管理，P1；启用需 server 配置 mlops.enabled=true） ----
export const mlopsApi = {
  prompts: () => get<{ items: MLOpsPrompt[] }>('/v1/mlops/prompts'),
  prompt: (id: string) => get<MLOpsPrompt>(`/v1/mlops/prompts/${id}`),
  createPrompt: (body: { name: string; version: { messages: MLOpsPromptMessage[]; note?: string } }) =>
    post<MLOpsPrompt>('/v1/mlops/prompts', body),
  savePrompt: (
    id: string,
    body: { expected_active_version?: number; activate: boolean; version: { messages: MLOpsPromptMessage[]; note?: string } },
  ) => put<MLOpsPrompt>(`/v1/mlops/prompts/${id}`, body),
  renderPrompt: (id: string, body: { version?: number; data?: unknown }) =>
    post<{ messages: MLOpsRenderedMessage[] }>(`/v1/mlops/prompts/${id}/render`, body),
  activatePrompt: (id: string, body: { version: number; expected_active_version?: number }) =>
    post<MLOpsPrompt>(`/v1/mlops/prompts/${id}/activate`, body),
  resetPrompt: (id: string) => post<MLOpsPrompt>(`/v1/mlops/prompts/${id}/reset`),
  deletePrompt: (id: string) => del<{ deleted: string }>(`/v1/mlops/prompts/${id}`),

  // ---- 用量费用（P2） ----
  costsOverview: () => get<MLOpsCostsOverview>('/v1/mlops/costs/overview'),
  costsTrend: (params?: { from?: string; to?: string }) =>
    get<{ items: MLOpsCostTrendRow[] }>('/v1/mlops/costs/trend', { params }),
  costsDetail: (params?: {
    from?: string
    to?: string
    provider?: string
    model?: string
    scenario?: string
    status?: string
    operation_id?: string
    before_seq?: number
    limit?: number
    offset?: number
  }) => get<{ items: MLOpsUsageDetail[]; total: number; truncated: boolean }>('/v1/mlops/costs/detail', { params }),
  costsOperation: (id: string) =>
    get<{ items: MLOpsUsageDetail[]; operation_id: string }>(`/v1/mlops/costs/operations/${id}`),
  pricingList: () => get<{ items: MLOpsPricing[] }>('/v1/mlops/costs/pricing'),
  pricingSave: (body: {
    provider: string
    model: string
    price_in_per_m: string
    price_out_per_m: string
    note?: string
  }) => put<MLOpsPricing>('/v1/mlops/costs/pricing', body),
  pricingDelete: (provider: string, model: string) =>
    del<{ deleted: string }>(`/v1/mlops/costs/pricing`, { params: { provider, model } }),

  // ---- 模型运营（P3） ----
  models: () => get<MLOpsModelsView>('/v1/mlops/models'),
  modelHealth: () => get<{ items: MLOpsHealthResult[] }>('/v1/mlops/models/health'),
  testModelHealth: (body: { provider: string; model: string }) =>
    post<{ ok: boolean; latency_ms: number; model: string; error?: string }>('/v1/mlops/models/health', body),
  enableModel: (body: { provider: string; model: string }) =>
    post<{ enabled: boolean }>('/v1/mlops/models/enable', body),
  disableModel: (body: { provider: string; model: string }) =>
    post<{ enabled: boolean }>('/v1/mlops/models/disable', body),
  bindings: () => get<{ items: MLOpsBinding[] }>('/v1/mlops/bindings'),
  saveBinding: (scenario: string, model: string) =>
    put<MLOpsBinding>(`/v1/mlops/bindings/${scenario}`, { model }),
  deleteBinding: (scenario: string) => del<{ deleted: string }>(`/v1/mlops/bindings/${scenario}`),

  // ---- 预算（P3） ----
  budgets: () => get<{ items: MLOpsBudgetView[] }>('/v1/mlops/budgets'),
  saveBudget: (body: { month: string; limit_minor: number; warn_at: number }) =>
    post<MLOpsBudgetView>('/v1/mlops/budgets', body),
  deleteBudget: (month: string) => del<{ deleted: string }>(`/v1/mlops/budgets/${month}`),
}

// ---- 管理端 MCP Server（/mcp 自然语言运维入口；admin） ----
export const mcpApi = {
  info: () => get<{ enabled: boolean; tokens?: string[] }>('/v1/mcp/info'),
  tokens: () => get<{ items: MCPTokenView[] }>('/v1/mcp/tokens'),
  createToken: (body: { name: string; scope: string }) =>
    post<{ token: MCPTokenView; secret: string; note?: string }>('/v1/mcp/tokens', body),
  deleteToken: (name: string) => del<{ deleted: string }>(`/v1/mcp/tokens/${name}`),
  setTokenEnabled: (name: string, enabled: boolean) =>
    put<{ name: string; enabled: boolean }>(`/v1/mcp/tokens/${name}/enabled`, { enabled }),
  audit: (params?: { actor?: string; tool?: string; limit?: number }) =>
    get<{ items: MCPAuditItem[] }>('/v1/mcp/audit', { params }),
  usage: (days = 7) => get<MCPUsageSummary>('/v1/mcp/usage', { params: { days } }),
}
