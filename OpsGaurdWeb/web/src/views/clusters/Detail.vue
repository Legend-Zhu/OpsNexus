<template>
  <div v-loading="loading" class="page">
    <div class="page-head">
      <div class="head-left">
        <el-button link :icon="Back" @click="$router.push('/clusters')">集群</el-button>
        <h2 class="page-title">{{ clusterName }}</h2>
        <el-tag v-if="cluster?.status" :type="cluster.status === 'online' ? 'success' : 'danger'" size="small">
          {{ cluster.status === 'online' ? '在线' : '离线' }}
        </el-tag>
        <span class="og-dim mono">{{ cluster?.worker_url }}</span>
      </div>
    </div>

    <el-tabs v-model="tab" class="tabs">
      <!-- 节点：集群 → 节点 → 容器/进程 -->
      <el-tab-pane :label="`节点 (${nodes.length})`" name="nodes">
        <div v-loading="nodesLoading" class="node-grid">
          <div v-for="n in nodes" :key="n.id" class="node-card" @click="openNode(n)">
            <div class="node-top">
              <span class="node-host">{{ n.hostname }}</span>
              <el-tag size="small" :type="n.role === 'manager' ? 'warning' : 'info'" effect="plain">
                {{ n.role }}{{ n.leader ? ' · leader' : '' }}
              </el-tag>
            </div>
            <div class="node-sub mono">{{ n.addr }}</div>
            <div class="node-stats">
              <div class="stat">
                <div class="stat-label">CPU</div>
                <el-progress :percentage="pct(n.cpuPercent)" :stroke-width="6" :show-text="false" />
                <div class="stat-val">{{ n.cpuPercent?.toFixed(1) ?? '—' }}%</div>
              </div>
              <div class="stat">
                <div class="stat-label">内存</div>
                <el-progress
                  :percentage="pct(n.memPercent)"
                  :stroke-width="6"
                  :show-text="false"
                  :status="n.memPercent > 85 ? 'exception' : undefined"
                />
                <div class="stat-val">{{ n.memPercent?.toFixed(1) ?? '—' }}%</div>
              </div>
            </div>
            <div class="node-foot">
              <span class="og-dim">容器 {{ n.containerCount ?? 0 }}</span>
              <el-tag size="small" :type="n.reachable ? 'success' : 'danger'" effect="plain">
                {{ n.reachable ? '可达' : '不可达' }}
              </el-tag>
            </div>
          </div>
          <el-empty v-if="!nodesLoading && !nodes.length" description="暂无节点（Worker 不可达或无 swarm 节点）" />
        </div>

        <!-- 节点抽屉：进程 + 容器 -->
        <el-drawer v-model="nodeVisible" :title="`${currentNode?.hostname ?? ''} · 节点详情`" size="60%">
          <template v-if="currentNode">
            <el-descriptions :column="3" border size="small" class="drawer-desc">
              <el-descriptions-item label="角色">
                {{ currentNode.role }}{{ currentNode.leader ? ' · leader' : '' }}
              </el-descriptions-item>
              <el-descriptions-item label="状态">{{ currentNode.state }} / {{ currentNode.availability }}</el-descriptions-item>
              <el-descriptions-item label="CPU 核数">{{ currentNode.cpuCores ?? '—' }}</el-descriptions-item>
              <el-descriptions-item label="内存总量">{{ fmtBytes(currentNode.memBytes) }}</el-descriptions-item>
              <el-descriptions-item label="容器数">{{ currentNode.containerCount ?? '—' }}</el-descriptions-item>
              <el-descriptions-item label="地址">{{ currentNode.addr }}</el-descriptions-item>
            </el-descriptions>

            <el-tabs v-model="nodeDrawerTab" class="drawer-tabs">
              <!-- 宿主机进程 -->
              <el-tab-pane label="宿主机进程" name="procs">
                <div class="drawer-toolbar">
                  <span class="og-dim">宿主机进程（Top N，按 CPU）</span>
                  <el-input
                    v-model="procFilter"
                    size="small"
                    clearable
                    placeholder="过滤名称/命令行，如 java、redis-server"
                    style="width: 230px; margin-left: auto; margin-right: 8px"
                    @keyup.enter="loadProcesses('cpu')"
                    @clear="loadProcesses('cpu')"
                  />
                  <el-button size="small" :icon="Refresh" @click="loadProcesses('cpu')">刷新</el-button>
                </div>
                <el-table :data="processes" size="small" v-loading="procsLoading" max-height="440">
                  <el-table-column prop="pid" label="PID" width="80" />
                  <el-table-column prop="name" label="进程" min-width="140" show-overflow-tooltip />
                  <el-table-column prop="cmdline" label="命令行" min-width="220" show-overflow-tooltip class-name="mono" />
                  <el-table-column label="CPU %" width="90" align="right">
                    <template #default="{ row }">{{ row.cpuPercent?.toFixed(1) }}</template>
                  </el-table-column>
                  <el-table-column label="内存" width="100" align="right">
                    <template #default="{ row }">{{ fmtKB(row.memKb) }}</template>
                  </el-table-column>
                </el-table>
              </el-tab-pane>

              <!-- 节点全部容器（含 standalone，如 r-nacos） -->
              <el-tab-pane :label="`容器 (${containers.length})`" name="containers">
                <div class="drawer-toolbar">
                  <span class="og-dim">节点上的全部容器（swarm 任务 + 直接 docker run 的 standalone）</span>
                  <el-button size="small" :icon="Refresh" @click="loadContainers">刷新</el-button>
                </div>
                <el-table :data="containers" size="small" v-loading="containersLoading" max-height="440">
                  <el-table-column label="名称" min-width="200" show-overflow-tooltip>
                    <template #default="{ row }">
                      <span class="mono">{{ row.name }}</span>
                      <el-tag v-if="row.type === 'standalone'" size="small" type="warning" effect="plain" class="ct-tag">standalone</el-tag>
                      <el-tag v-else size="small" type="info" effect="plain" class="ct-tag">{{ row.service }}</el-tag>
                    </template>
                  </el-table-column>
                  <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip class-name="mono" />
                  <el-table-column label="状态" width="100">
                    <template #default="{ row }">
                      <el-tag size="small" :type="row.state === 'running' ? 'success' : 'info'">{{ row.state }}</el-tag>
                    </template>
                  </el-table-column>
                  <el-table-column label="端口" prop="ports" min-width="160" show-overflow-tooltip class-name="mono">
                    <template #default="{ row }">{{ row.ports || '—' }}</template>
                  </el-table-column>
                </el-table>
              </el-tab-pane>
            </el-tabs>
          </template>
        </el-drawer>
      </el-tab-pane>

      <!-- 容器与服务 -->
      <el-tab-pane :label="`容器与服务 (${workloads.length})`" name="workloads">
        <div class="tab-toolbar">
          <el-tag size="small" effect="plain" class="og-dim">
            按部署配置 labels.category 归类（service / middleware）
          </el-tag>
          <el-button type="primary" size="small" :icon="Plus" :disabled="!clusterName" @click="openDeploy">
            部署服务
          </el-button>
        </div>
        <el-table v-loading="wLoading" :data="workloads" empty-text="该集群暂无服务，点击「部署服务」创建">
          <el-table-column label="名称" min-width="150">
            <template #default="{ row }">
              <el-link type="primary" @click="openDetail(row)">{{ row.name }}</el-link>
            </template>
          </el-table-column>
          <el-table-column label="类别" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="categoryOf(row) === 'middleware' ? 'warning' : 'info'" effect="plain">
                {{ categoryOf(row) }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip />
          <el-table-column label="模式" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.mode === 'global' ? 'warning' : 'info'" effect="plain">
                {{ row.mode ?? '—' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="副本" prop="replica" width="80" />
          <el-table-column label="端口" min-width="150">
            <template #default="{ row }">
              <span v-for="p in row.ports ?? []" :key="`${p.publishedPort}:${p.targetPort}`" class="port-chip mono">
                {{ p.publishedPort }}→{{ p.targetPort }}
              </span>
              <span v-if="!row.ports?.length">—</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="210" fixed="right">
            <template #default="{ row }">
              <el-button link type="primary" @click="openDetail(row)">详情</el-button>
              <el-button link type="primary" @click="openScale(row)">缩放</el-button>
              <el-button link type="warning" @click="restart(row)">重启</el-button>
              <el-button link type="danger" @click="remove(row)">移除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 中间件：category=middleware 的服务 -->
      <el-tab-pane :label="`中间件 (${middlewares.length})`" name="middleware">
        <el-table v-loading="wLoading" :data="middlewares" empty-text="暂无中间件（部署时给服务加 labels.category=middleware 归类）">
          <el-table-column label="名称" prop="name" min-width="150" />
          <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip />
          <el-table-column label="端口" min-width="150">
            <template #default="{ row }">
              <span v-for="p in row.ports ?? []" :key="p.publishedPort" class="port-chip mono">
                {{ p.publishedPort }}→{{ p.targetPort }}
              </span>
              <span v-if="!row.ports?.length">—</span>
            </template>
          </el-table-column>
          <el-table-column label="副本" prop="replica" width="80" />
          <el-table-column label="操作" width="110">
            <template #default="{ row }">
              <el-button link type="primary" @click="openDetail(row)">详情</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 监控：集群全节点资源 -->
      <el-tab-pane label="监控" name="monitor">
        <div class="tab-toolbar">
          <el-button size="small" :icon="Refresh" @click="loadNodes">刷新</el-button>
        </div>
        <el-table :data="nodes" v-loading="nodesLoading" size="small" empty-text="暂无节点数据">
          <el-table-column label="节点" prop="hostname" min-width="140" />
          <el-table-column label="角色" prop="role" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.role === 'manager' ? 'warning' : 'info'">{{ row.role }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="80">
            <template #default="{ row }">
              <el-tag size="small" :type="row.reachable ? 'success' : 'danger'">{{ row.reachable ? '可达' : '离线' }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="CPU 核心" prop="cpuCores" width="90" />
          <el-table-column label="CPU 占用" width="130">
            <template #default="{ row }">
              <el-progress :percentage="Math.min(100, row.cpuPercent)" :stroke-width="8" />
            </template>
          </el-table-column>
          <el-table-column label="内存占用" width="130">
            <template #default="{ row }">
              <el-progress
                :percentage="Math.min(100, row.memPercent)"
                :stroke-width="8"
                :status="row.memPercent > 85 ? 'exception' : undefined"
              />
            </template>
          </el-table-column>
          <el-table-column label="内存" width="120">
            <template #default="{ row }">{{ fmtBytes(row.memBytes) }}</template>
          </el-table-column>
          <el-table-column label="容器数" prop="containerCount" width="80" />
        </el-table>
      </el-tab-pane>

      <!-- 事件 -->
      <el-tab-pane :label="`事件 (${events.length})`" name="events">
        <div class="tab-toolbar">
          <el-button size="small" :icon="Refresh" @click="loadEvents">刷新</el-button>
        </div>
        <el-table :data="events" size="small" empty-text="暂无事件">
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ new Date(row.ts).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="服务" prop="service" width="120" />
          <el-table-column label="级别" width="80">
            <template #default="{ row }">
              <el-tag size="small" :type="row.level === 'error' ? 'danger' : row.level === 'warn' ? 'warning' : 'info'">
                {{ row.level }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="内容" prop="msg" min-width="260" show-overflow-tooltip />
        </el-table>
      </el-tab-pane>
    </el-tabs>

    <!-- 部署对话框 -->
    <el-dialog v-model="deployVisible" title="部署服务" width="680px">
      <el-form label-width="80px">
        <el-form-item label="集群">
          <el-tag>{{ clusterName }}</el-tag>
        </el-form-item>
        <el-form-item label="类别">
          <el-select v-model="deployCategory" style="width: 100%">
            <el-option label="服务" value="service" />
            <el-option label="中间件" value="middleware" />
          </el-select>
        </el-form-item>
        <el-form-item label="配置" required>
          <el-input
            v-model="deployConfig"
            type="textarea"
            :rows="14"
            class="mono"
            placeholder="Worker 服务配置（YAML/JSON），示例：
service:
  name: web
  image: nginx:alpine
  replicas: 2
  labels:
    category: service
  ports:
    - { target: 80, published: 8080 }
monitoring:
  enabled: true
  portChecks:
    - { port: &quot;8080&quot; }"
          />
        </el-form-item>
        <el-form-item>
          <el-collapse class="cfg-doc">
            <el-collapse-item title="配置字段说明（service / monitoring）" name="doc">
              <h5>service（必填：name、image）</h5>
              <ul>
                <li><code>mode</code>：<code>replicated</code>（默认，配 <code>replicas</code>）/ <code>global</code>（每节点一个，勿配 replicas）</li>
                <li><code>env</code> / <code>command</code> / <code>args</code> / <code>workdir</code> / <code>user</code>：容器运行参数</li>
                <li><code>ports</code>：<code>{ target, published, protocol: tcp|udp, mode: ingress|host }</code>；ingress 模式集群任意节点可访问</li>
                <li><code>mounts</code>：<code>{ type: volume|bind|tmpfs, source, target, readonly }</code></li>
                <li><code>resources</code>：<code>limits</code> / <code>reservations</code>，如 <code>{ cpu: "1.0", memory: "512Mi" }</code></li>
                <li><code>registryAuth</code>：私有仓库凭据，<code>secretRef</code>（swarm secret 名）或 <code>inline</code>（docker config.json 的 base64）</li>
                <li><code>healthcheck</code>：<code>{ test: ["CMD-SHELL","curl -f ..."], interval, timeout, retries, startPeriod }</code>，部署收敛会等待健康</li>
                <li><code>placement</code>：<code>{ constraints: ["node.role==worker", ...], preferences: [{spread}] }</code></li>
                <li><code>update</code> / <code>rollback</code>：<code>{ parallelism, delay, failureAction: pause|continue|rollback, monitor, maxFailureRatio }</code></li>
                <li><code>restart</code>：<code>{ condition: any|on-failure|none, delay, maxAttempts, window }</code></li>
                <li><code>networks</code> / <code>secrets</code> / <code>configs</code> / <code>labels</code> / <code>logDriver</code> / <code>imagePullPolicy</code>(always|missing|never)</li>
              </ul>
              <h5>monitoring（可选，enabled: true 生效；异常进告警中心并按级别策略通知）</h5>
              <ul>
                <li><code>portChecks</code>：<code>{ port, protocol, interval, timeout, retries }</code>——TCP 探测已发布端口，连续失败发 port_down</li>
                <li><code>httpChecks</code>：<code>{ url, method, headers, expectedStatus, expectedBody(正则), interval, timeout }</code>——发 http_unhealthy；url 用 localhost 会自动重写为任务节点 IP</li>
                <li><code>logChecks</code>：<code>{ pattern(Go 正则), level, ignore: [...], action: alert|restart }</code>——日志匹配告警或自动重启</li>
                <li><code>resourceThresholds</code>：<code>{ metric: cpu|memory, threshold: 百分比, action: alert|restart }</code></li>
              </ul>
              <div class="muted">完整契约见仓库 docs/Worker-设计方案.md §4.2；部署为异步操作，提交后自动轮询收敛结果。</div>
            </el-collapse-item>
          </el-collapse>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="deployVisible = false">取消</el-button>
        <el-button type="primary" :loading="deploying" @click="deploy">部署</el-button>
      </template>
    </el-dialog>

    <!-- 缩放对话框 -->
    <el-dialog v-model="scaleVisible" title="缩放副本" width="360px">
      <el-form label-width="80px">
        <el-form-item label="服务">
          <el-tag>{{ current?.name }}</el-tag>
        </el-form-item>
        <el-form-item label="副本数" required>
          <el-input-number v-model="scaleReplicas" :min="0" :max="999" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="scaleVisible = false">取消</el-button>
        <el-button type="primary" :loading="scaling" @click="scale">应用</el-button>
      </template>
    </el-dialog>

    <!-- 详情抽屉（任务 + 日志） -->
    <el-drawer v-model="detailVisible" :title="`${current?.name ?? ''} 详情`" size="55%">
      <template v-if="detail">
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="镜像">{{ detail.image || '—' }}</el-descriptions-item>
          <el-descriptions-item label="模式">{{ detail.mode || '—' }}</el-descriptions-item>
          <el-descriptions-item label="副本">{{ detail.replica || '—' }}</el-descriptions-item>
          <el-descriptions-item label="健康">{{ detail.healthy }} 个</el-descriptions-item>
        </el-descriptions>

        <h4>任务</h4>
        <el-table :data="activeTasks" size="small" style="width: 100%">
          <el-table-column prop="id" label="任务 ID" min-width="110" show-overflow-tooltip>
            <template #default="{ row }"><span class="mono" :title="row.id">{{ shortId(row.id) }}</span></template>
          </el-table-column>
          <el-table-column prop="slot" label="Slot" width="60" />
          <el-table-column prop="nodeId" label="节点" min-width="140" show-overflow-tooltip>
            <template #default="{ row }">{{ nodeNameOf(row.nodeId) }}</template>
          </el-table-column>
          <el-table-column prop="state" label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="row.state === 'running' ? 'success' : row.state === 'failed' ? 'danger' : 'info'">{{ row.state }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="containerId" label="容器" min-width="120" show-overflow-tooltip>
            <template #default="{ row }"><span class="mono" :title="row.containerId">{{ shortId(row.containerId) }}</span></template>
          </el-table-column>
        </el-table>

        <div class="log-header">
          <h4>日志<span class="log-hint">（聚合自 {{ detail.desired || detail.tasks?.length || 0 }} 个副本）</span></h4>
          <div>
            <el-button size="small" @click="toggleLogFollow">{{ logFollow ? '停止跟随' : '跟随最新' }}</el-button>
            <el-button size="small" @click="loadLogs(false)">刷新</el-button>
          </div>
        </div>
        <div ref="logBoxRef" class="log-box">
          <div v-for="(l, i) in logLines" :key="i" :class="['log-line', l.stream]">
            <span class="log-ts">{{ l.ts }}</span> {{ l.line }}
          </div>
          <el-empty v-if="!logLines.length" description="暂无日志" :image-size="40" />
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { Back, Plus, Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox, ElNotification } from 'element-plus'
import { clusterApi, eventApi, nodeApi, workloadApi } from '@/api'
import type { ClusterNode, ClusterSummary, ContainerInfo, EventItem, LogLine, Operation, ProcessInfo, Workload, WorkloadDetail } from '@/types'

const route = useRoute()
const clusterName = computed(() => route.params.name as string)

const tab = ref('nodes')
const loading = ref(false)
const cluster = ref<ClusterSummary | null>(null)

// ---- 本地缓存（按集群名，sessionStorage 会话级） ----
// 先渲染缓存避免白屏，后台异步刷新并回写缓存。
const CACHE_TTL = 10 * 1000 // 10s（短于 server 端 30s，避免前端显示过旧数据）

interface CacheEntry<T> {
  ts: number
  data: T
}

function cacheKey(kind: string) {
  return `opsguard:${kind}:${clusterName.value}`
}

function readCache<T>(kind: string): T | null {
  try {
    const raw = sessionStorage.getItem(cacheKey(kind))
    if (!raw) return null
    const entry: CacheEntry<T> = JSON.parse(raw)
    if (Date.now() - entry.ts > CACHE_TTL) return null
    return entry.data
  } catch {
    return null
  }
}

function writeCache<T>(kind: string, data: T) {
  try {
    const entry: CacheEntry<T> = { ts: Date.now(), data }
    sessionStorage.setItem(cacheKey(kind), JSON.stringify(entry))
  } catch {
    // 忽略存储失败（隐私模式等）
  }
}

// ---- 节点 ----
const nodesLoading = ref(false)
const nodes = ref<ClusterNode[]>([])
const nodeVisible = ref(false)
const currentNode = ref<ClusterNode | null>(null)
const nodeDrawerTab = ref('procs')
const processes = ref<ProcessInfo[]>([])
const procFilter = ref('')
const containers = ref<ContainerInfo[]>([])
const containersLoading = ref(false)
const procsLoading = ref(false)

// 工作负载 / 中间件
const wLoading = ref(false)
const workloads = ref<Workload[]>([])
const middlewares = computed(() => workloads.value.filter((w) => categoryOf(w) === 'middleware'))

// 事件
const events = ref<EventItem[]>([])

// 部署/缩放/详情
const deployVisible = ref(false)
const deployConfig = ref('')
const deployCategory = ref('service')
const deploying = ref(false)
const scaleVisible = ref(false)
const scaleReplicas = ref(1)
const scaling = ref(false)
const detailVisible = ref(false)
const detail = ref<WorkloadDetail | null>(null)
const current = ref<Workload | null>(null)

// 活跃任务：过滤 failed/shutdown 等历史任务，只保留当前在跑/待跑的。
// Docker 服务每次重启/更新都会产生历史任务记录，全量展示会刷屏。
const activeTasks = computed(() => {
  const tasks = detail.value?.tasks ?? []
  return tasks.filter((t) => t.state !== 'failed' && t.state !== 'shutdown' && t.state !== 'complete' && t.state !== 'orphaned')
})

const logLines = ref<LogLine[]>([])
const logFollow = ref(false)
const logBoxRef = ref<HTMLElement>()
let logAbort: AbortController | null = null

function pct(v?: number) {
  return Math.max(0, Math.min(100, v ?? 0))
}
// nodeNameOf 把 swarm node ID 映射为主机名（任务表格用 nodes 列表反查）。
function nodeNameOf(nodeId?: string) {
  if (!nodeId) return '—'
  const n = nodes.value.find((x) => x.id === nodeId)
  return n ? n.hostname : nodeId.slice(0, 12)
}
// shortId 截短 Docker/swarm 内部 ID（任务/容器），取前 12 位 + 完整值 tooltip。
function shortId(id?: string) {
  if (!id) return '—'
  return id.length > 12 ? id.slice(0, 12) : id
}
function fmtBytes(n?: number) {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}
function fmtKB(kb?: number) {
  if (!kb) return '—'
  return fmtBytes(kb * 1024)
}
function categoryOf(w: Workload): string {
  return w.labels?.['category'] ?? 'service'
}

// ---- 集群 ----
async function fetchCluster() {
  try {
    cluster.value = await clusterApi.get(clusterName.value)
  } catch {
    cluster.value = null
  }
}

// ---- 节点 ----
async function loadNodes() {
  // 先用缓存渲染（避免白屏），再异步刷新。
  const cached = readCache<ClusterNode[]>('nodes')
  if (cached) nodes.value = cached

  nodesLoading.value = !cached // 有缓存时不显示 loading 遮罩
  try {
    const resp = await nodeApi.list(clusterName.value)
    nodes.value = resp.items ?? []
    writeCache('nodes', nodes.value)
  } catch {
    if (!cached) nodes.value = []
  } finally {
    nodesLoading.value = false
  }
}

function openNode(n: ClusterNode) {
  currentNode.value = n
  processes.value = []
  procFilter.value = ''
  containers.value = []
  nodeDrawerTab.value = 'procs'
  nodeVisible.value = true
  void loadProcesses('cpu')
  void loadContainers()
}

async function loadContainers() {
  if (!currentNode.value) return
  containersLoading.value = true
  try {
    const resp = await nodeApi.containers(clusterName.value, currentNode.value.id)
    containers.value = resp.items ?? []
  } catch {
    containers.value = []
  } finally {
    containersLoading.value = false
  }
}

async function loadProcesses(top: string) {
  if (!currentNode.value) return
  procsLoading.value = true
  try {
    const resp = await nodeApi.processes(clusterName.value, currentNode.value.id, {
      top,
      limit: 100,
      filter: procFilter.value.trim() || undefined,
    })
    processes.value = resp.processes ?? []
  } catch {
    processes.value = []
  } finally {
    procsLoading.value = false
  }
}

// ---- 工作负载 ----
async function loadWorkloads() {
  wLoading.value = true
  try {
    const resp = await workloadApi.list(clusterName.value)
    workloads.value = resp.items ?? []
  } catch {
    workloads.value = []
  } finally {
    wLoading.value = false
  }
}

// ---- 事件 ----
async function loadEvents() {
  try {
    const resp = await eventApi.list(clusterName.value, { limit: 100 })
    events.value = resp.items ?? []
  } catch {
    events.value = []
  }
}

// ---- 部署 ----
function openDeploy() {
  deployConfig.value = `service:\n  name: web\n  image: nginx:alpine\n  replicas: 1\n  labels:\n    category: ${deployCategory.value}\n  ports:\n    - { target: 80, published: 8080 }\n# monitoring:              # 可选：监控（端口/HTTP/日志/资源阈值）\n#   enabled: true\n#   portChecks:\n#     - { port: "8080" }\n#   httpChecks:\n#     - { url: "http://localhost:8080/health", expectedStatus: [200] }`
  deployVisible.value = true
}

async function deploy() {
  if (!deployConfig.value.trim()) {
    ElMessage.warning('请填写服务配置')
    return
  }
  deploying.value = true
  try {
    const op = await workloadApi.deploy(clusterName.value, { config: deployConfig.value })
    deployVisible.value = false
    ElNotification.success({ title: '部署已提交', message: `操作 ${op.id}` })
    pollOperation(op)
  } finally {
    deploying.value = false
  }
}

// ---- 缩放 ----
function openScale(row: Workload) {
  current.value = row
  scaleReplicas.value = row.desired ?? 1
  scaleVisible.value = true
}

async function scale() {
  if (!current.value) return
  scaling.value = true
  try {
    const op = await workloadApi.scale(clusterName.value, current.value.name, scaleReplicas.value)
    scaleVisible.value = false
    ElNotification.success({ title: '缩放已提交', message: `操作 ${op.id}` })
    pollOperation(op)
  } finally {
    scaling.value = false
  }
}

// ---- 重启 / 移除 ----
async function restart(row: Workload) {
  await ElMessageBox.confirm(`确定重启服务「${row.name}」？`, '重启服务', { type: 'warning' })
  const op = await workloadApi.restart(clusterName.value, row.name)
  ElNotification.success({ title: '重启已提交', message: `操作 ${op.id}` })
  pollOperation(op)
}

async function remove(row: Workload) {
  await ElMessageBox.confirm(`确定移除服务「${row.name}」？此操作不可恢复。`, '移除服务', { type: 'error' })
  const op = await workloadApi.remove(clusterName.value, row.name)
  ElNotification.success({ title: '移除已提交', message: `操作 ${op.id}` })
  await loadWorkloads()
}

// ---- 操作轮询 ----
const TERMINAL = new Set(['healthy', 'done', 'failed', 'partial', 'canceled'])

async function pollOperation(op: Operation) {
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 1500))
    try {
      const cur = await workloadApi.operation(clusterName.value, op.id)
      if (TERMINAL.has(cur.status)) {
        if (cur.status === 'healthy' || cur.status === 'done') {
          ElNotification.success({ title: `操作 ${op.type} 完成`, message: cur.error || '成功' })
        } else {
          ElNotification.error({ title: `操作 ${op.type} ${cur.status}`, message: cur.error || '未完全成功' })
        }
        await loadWorkloads()
        return
      }
    } catch {
      return
    }
  }
}

// ---- 详情 + 日志 ----
async function openDetail(row: Workload) {
  current.value = row
  detailVisible.value = true
  logLines.value = []
  try {
    detail.value = await workloadApi.get(clusterName.value, row.name)
  } catch {
    detail.value = null
  }
  await loadLogs(false)
}

async function loadLogs(follow: boolean) {
  if (!current.value) return
  logAbort?.abort()
  logAbort = new AbortController()
  if (!follow) logLines.value = []

  try {
    const resp = await fetch(workloadApi.logsUrl(clusterName.value, current.value.name, follow), {
      signal: logAbort.signal,
      headers: authHeaders(),
    })
    if (!resp.ok || !resp.body) {
      ElMessage.error(`日志拉取失败：${resp.status}`)
      return
    }
    const reader = resp.body.getReader()
    const decoder = new TextDecoder()
    let buf = ''
    // eslint-disable-next-line no-constant-condition
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      let idx: number
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx).trim()
        buf = buf.slice(idx + 1)
        if (line.startsWith('data:')) {
          const payload = line.slice(5).trim()
          if (!payload || payload === '[DONE]') continue
          try {
            logLines.value.push(JSON.parse(payload) as LogLine)
          } catch {
            /* 忽略无法解析的行 */
          }
        }
      }
      if (logLines.value.length > 2000) {
        logLines.value.splice(0, logLines.value.length - 2000)
      }
      scrollLogToBottom()
    }
  } catch (e) {
    if ((e as Error).name !== 'AbortError') {
      ElMessage.error(`日志流中断：${(e as Error).message}`)
    }
  }
}

async function toggleLogFollow() {
  logFollow.value = !logFollow.value
  if (logFollow.value) {
    await loadLogs(true)
  } else {
    logAbort?.abort()
  }
}

function scrollLogToBottom() {
  void nextTick(() => {
    const el = logBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

function authHeaders(): Record<string, string> {
  const token = localStorage.getItem('opsguard_token')
  return token ? { Authorization: `Bearer ${token}` } : {}
}

// 切 Tab 时按需加载
watch(tab, (t) => {
  if (t === 'workloads' && !workloads.value.length) void loadWorkloads()
  if (t === 'middleware' && !workloads.value.length) void loadWorkloads()
  if (t === 'events' && !events.value.length) void loadEvents()
})

onMounted(async () => {
  loading.value = true
  await fetchCluster()
  // 并行加载节点 + 工作负载，让 tab 标签数字（容器与服务 N）进页面即显示。
  await Promise.all([loadNodes(), loadWorkloads()])
  loading.value = false
})

onBeforeUnmount(() => logAbort?.abort())
</script>

<style scoped>
.page-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}
.head-left {
  display: flex;
  align-items: center;
  gap: 10px;
}
.page-title {
  margin: 0;
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.tabs :deep(.el-tabs__header) {
  margin-bottom: 14px;
}
.tab-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
}

/* 节点卡片 */
.node-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 14px;
}
.node-card {
  background: var(--og-bg-surface);
  border: 1px solid var(--el-border-color-light);
  border-radius: 12px;
  padding: 14px 16px;
  cursor: pointer;
  transition: border-color 0.15s, transform 0.15s, box-shadow 0.15s;
}
.node-card:hover {
  border-color: var(--og-accent);
  transform: translateY(-2px);
  box-shadow: 0 10px 28px rgba(0, 0, 0, 0.35);
}
.node-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.node-host {
  font-weight: 650;
  font-size: 14px;
}
.node-sub {
  color: var(--og-text-dim);
  font-size: 12px;
  margin: 2px 0 12px;
}
.node-stats {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
}
.stat-label {
  font-size: 11px;
  color: var(--og-text-dim);
  margin-bottom: 4px;
}
.stat-val {
  font-size: 12px;
  margin-top: 4px;
  font-variant-numeric: tabular-nums;
}
.node-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px solid var(--el-border-color-lighter);
}
.drawer-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 14px 0 8px;
}

.port-chip {
  display: inline-block;
  margin-right: 6px;
  padding: 1px 6px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  font-size: 12px;
}
.log-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}
.log-hint {
  font-size: 12px;
  font-weight: normal;
  color: #909399;
  margin-left: 8px;
}
.log-box {
  height: 320px;
  overflow: auto;
  background: #0d1117;
  color: #e6edf3;
  border-radius: 6px;
  padding: 10px;
  font-family: var(--og-mono);
  font-size: 12px;
  line-height: 1.6;
}
.log-line.stderr {
  color: #ff7b72;
}
.log-ts {
  color: #8b949e;
  margin-right: 8px;
}
.cfg-doc {
  width: 100%;
}
.cfg-doc h5 {
  margin: 8px 0 4px;
  font-size: 13px;
}
.cfg-doc ul {
  margin: 0;
  padding-left: 18px;
}
.cfg-doc li {
  font-size: 12px;
  line-height: 1.8;
  color: var(--el-text-color-regular);
}
.cfg-doc code {
  font-family: var(--og-mono);
  font-size: 11px;
  padding: 1px 4px;
  background: var(--el-fill-color-light);
  border-radius: 3px;
}
</style>
