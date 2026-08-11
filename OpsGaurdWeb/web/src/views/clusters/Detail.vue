<template>
  <div v-loading="loading" class="page">
    <div class="page-head">
      <div class="head-left">
        <el-button link :icon="Back" @click="$router.push('/clusters')">集群</el-button>
        <h2 class="page-title">{{ clusterName }}</h2>
        <el-tooltip v-if="cluster?.err" :content="cluster.err" placement="bottom">
          <el-tag v-if="cluster?.status" :type="cluster.status === 'online' ? 'success' : 'danger'" size="small">
            {{ cluster.status === 'online' ? '在线' : '离线' }}
          </el-tag>
        </el-tooltip>
        <el-tag v-else-if="cluster?.status" :type="cluster.status === 'online' ? 'success' : 'danger'" size="small">
          {{ cluster.status === 'online' ? '在线' : '离线' }}
        </el-tag>
        <span class="og-dim mono">{{ cluster?.worker_url }}</span>
      </div>
      <div class="head-actions">
        <el-tooltip :disabled="cluster?.status !== 'offline'" :content="cluster?.err ?? '集群离线，无法部署'" placement="bottom">
          <el-button type="primary" size="small" :icon="Plus" :disabled="cluster?.status === 'offline'" @click="openDeploy">
            部署
          </el-button>
        </el-tooltip>
        <el-button size="small" :icon="Setting" @click="openInvConfig">纳管配置</el-button>
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
            <template v-if="nodeStatsReady[n.id]">
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
            </template>
            <div v-else class="node-stats og-dim">加载中…</div>
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
                  <span class="og-dim">宿主机进程（共 {{ processes.length }}，按 CPU 排序）</span>
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
                <el-table :data="pagedProcesses" size="small" v-loading="procsLoading" max-height="400">
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
                <el-pagination
                  v-if="processes.length > procPageSize"
                  v-model:current-page="procPage"
                  :page-size="procPageSize"
                  :total="processes.length"
                  layout="prev, pager, next, total"
                  size="small"
                  style="margin-top: 8px; justify-content: flex-end"
                />
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

      <!-- 服务：所有纳管对象（swarm service + standalone 容器 + 裸进程） -->
      <el-tab-pane :label="`服务 (${inventoryViews.length})`" name="workloads">
        <div class="tab-toolbar">
          <el-button size="small" :icon="Refresh" @click="loadInventory">刷新</el-button>
        </div>
        <el-table v-loading="invLoading" :data="inventoryViews" :empty-text="clusterOffline ? '集群离线（' + (cluster?.err ?? 'Worker 不可达') + '），无法读取服务' : '该集群暂无纳管对象'">
          <el-table-column label="名称" min-width="130">
            <template #default="{ row }">
              <el-link v-if="row.source === 'swarm'" type="primary" @click="openDetailByName(row.name)">{{ row.name }}</el-link>
              <span v-else>{{ row.name }}</span>
            </template>
          </el-table-column>
          <el-table-column label="来源" width="70">
            <template #default="{ row }">
              <el-tag size="small" :type="sourceTagType(row.source)">{{ sourceLabel(row.source) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="分类" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.category === 'middleware' ? 'warning' : 'info'" effect="plain">{{ row.category || '—' }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip />
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="statusTagType(row.status)">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="节点" prop="node" width="130" />
          <el-table-column label="端口" prop="ports" min-width="120" show-overflow-tooltip />
          <el-table-column label="操作" width="250" fixed="right">
            <template #default="{ row }">
              <template v-if="row.source === 'swarm'">
                <el-button link type="primary" @click="openDetailByName(row.name)">详情</el-button>
                <el-button link type="primary" @click="openEditService(row)">编辑</el-button>
                <el-button v-if="!isGlobalWorkload(row.name)" link type="primary" @click="swarmAction(row.name, openScale)">缩放</el-button>
                <el-button link type="warning" @click="swarmAction(row.name, restart)">重启</el-button>
                <el-button link type="danger" @click="swarmAction(row.name, remove)">移除</el-button>
              </template>
              <template v-else-if="row.source === 'inventory' && row.type === 'standalone-container'">
                <el-button link type="primary" @click="restartStandalone(row)">重启</el-button>
              </template>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 中间件：swarm middleware + inventory 里 category=middleware 的 -->
      <el-tab-pane :label="`中间件 (${middlewareViews.length})`" name="middleware">
        <div class="tab-toolbar">
          <el-button size="small" :icon="Refresh" @click="loadInventory">刷新</el-button>
        </div>
        <el-table v-loading="invLoading" :data="middlewareViews" empty-text="暂无中间件（swarm 部署加 labels.category=middleware，或在纳管配置里声明 standalone/host-service）">
          <el-table-column label="名称" prop="name" min-width="120" />
          <el-table-column label="来源" width="80">
            <template #default="{ row }">
              <el-tag size="small" :type="sourceTagType(row.source)">{{ sourceLabel(row.source) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip />
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="statusTagType(row.status)">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="节点" prop="node" width="140" />
          <el-table-column label="端口" prop="ports" min-width="120" show-overflow-tooltip />
          <el-table-column label="操作" width="80">
            <template #default="{ row }">
              <el-button v-if="row.source === 'swarm'" link type="primary" @click="openDetailByName(row.name)">详情</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 事件 -->
      <el-tab-pane :label="`事件 (${events.length})`" name="events">
        <div class="tab-toolbar">
          <span class="og-dim">最近 100 条（按时间倒序；「加载更多」翻更早）</span>
          <el-button size="small" :icon="Refresh" @click="loadEvents()">刷新</el-button>
        </div>
        <el-table :data="events" size="small" v-loading="eventsLoading" empty-text="暂无事件">
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
        <div v-if="eventsHasMore" class="events-more">
          <el-button :loading="eventsLoading" @click="loadEvents(true)">加载更多</el-button>
        </div>
      </el-tab-pane>
    </el-tabs>

    <!-- 部署/编辑对话框 -->
    <el-dialog v-model="deployVisible" :title="deployMode === 'edit' ? `编辑服务 ${deployTarget ?? ''}` : '部署'" width="680px">
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
          <div v-if="deployMode === 'edit' && !deployHasSnapshot" class="edit-no-snapshot">
            无历史配置快照（服务可能由外部创建）——保存将以当前输入整体替换服务配置，请谨慎填写
          </div>
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
        <el-button type="primary" :loading="deploying" @click="deploy">
          {{ deployMode === 'edit' ? '保存' : '部署' }}
        </el-button>
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
    <el-drawer v-model="detailVisible" :title="`${current?.name ?? ''} 详情`" size="55%" @closed="onDetailClosed">
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

    <!-- 纳管配置编辑器（表单 + YAML 双模式） -->
    <el-dialog v-model="invConfigVisible" title="纳管配置" width="720px" :close-on-click-modal="false">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 12px">
        声明集群纳管的外部对象（非 OpsGaurd 部署的 swarm service）：standalone-container
        （docker run 容器，容器名 + 所在节点 hostname + 端口）或 host-service
        （宿主机服务，IP 地址 + 端口探活）。保存会整体替换当前清单。
      </el-alert>
      <!-- key 每次打开递增 → 组件重建，表单始终反映当前已保存的清单（杜绝草稿残留/引用不变不刷新的问题） -->
      <InventoryEditor ref="invEditorRef" :key="invEditorKey" :config="invConfigSnapshot" @save="saveInvConfig" />
      <template #footer>
        <el-button @click="invConfigVisible = false">取消</el-button>
        <el-button type="primary" :loading="invSaving" @click="submitInvConfig">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { Back, Plus, Refresh, Setting } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox, ElNotification } from 'element-plus'
import InventoryEditor from '@/components/InventoryEditor.vue'
import { clusterApi, eventApi, inventoryApi, nodeApi, workloadApi } from '@/api'
import type { ClusterNode, ClusterSummary, ContainerInfo, EventItem, InventoryConfig, InventoryView, LogLine, Operation, ProcessInfo, Workload, WorkloadDetail } from '@/types'

const route = useRoute()
const clusterName = computed(() => route.params.name as string)
const clusterOffline = computed(() => cluster.value?.status === 'offline')

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
// nodeStatsReady[id] = true once that node's stats sample has arrived over the
// SSE stream (before that the card shows a "加载中…" placeholder — the base
// list arrives instantly but stats stream in per-node).
const nodeStatsReady = reactive<Record<string, boolean>>({})
let nodeStream: EventSource | null = null
const nodeVisible = ref(false)
const currentNode = ref<ClusterNode | null>(null)
const nodeDrawerTab = ref('procs')
const processes = ref<ProcessInfo[]>([])
const procFilter = ref('')
const procPage = ref(1)
const procPageSize = ref(20)
// 分页后的进程列表（客户端分页）
const pagedProcesses = computed(() => {
  const start = (procPage.value - 1) * procPageSize.value
  return processes.value.slice(start, start + procPageSize.value)
})
const containers = ref<ContainerInfo[]>([])
const containersLoading = ref(false)
const procsLoading = ref(false)

// 工作负载 / 中间件
const workloads = ref<Workload[]>([])

// 纳管清单（合并 swarm + inventory，含实时状态）
const invLoading = ref(false)
const inventoryViews = ref<InventoryView[]>([])
// 中间件 tab = 纳管清单里 category=middleware 的条目（swarm + standalone + host-service）
const middlewareViews = computed(() => inventoryViews.value.filter((v) => v.category === 'middleware'))

// 纳管配置编辑器（InventoryEditor 组件，表单 + YAML 双模式）
const invConfigVisible = ref(false)
const invConfigSnapshot = ref<InventoryConfig | null>(null)
const invEditorRef = ref<InstanceType<typeof InventoryEditor>>()
const invSaving = ref(false)
/** 每次打开对话框递增，强制重建编辑器组件（表单始终反映当前已保存清单） */
const invEditorKey = ref(0)

// 事件
const events = ref<EventItem[]>([])
const eventsLoading = ref(false)
const eventsHasMore = ref(false)
/** 上一页最小 seq（倒序分页游标）；0 = 拉最新一页 */
const eventsAfterSeq = ref(0)
/** 事件 tab 最近一次加载时间（懒加载节流） */
const eventsLoadedAt = ref(0)

// 部署/编辑/缩放/详情
const deployVisible = ref(false)
const deployConfig = ref('')
const deployCategory = ref('service')
const deploying = ref(false)
/** 对话框模式：deploy=部署新服务；edit=更新已有服务（复用同一表单） */
const deployMode = ref<'deploy' | 'edit'>('deploy')
const deployTarget = ref('')
const deployHasSnapshot = ref(false)
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

// ---- 纳管清单 ----
async function loadInventory() {
  invLoading.value = true
  try {
    const resp = await inventoryApi.get(clusterName.value)
    inventoryViews.value = resp.items ?? []
  } catch {
    inventoryViews.value = []
  } finally {
    invLoading.value = false
  }
}

// swarm 中间件条目点"详情"→ 查 workloads 找到原始 Workload 对象
async function openDetailByName(name: string) {
  let w = workloads.value.find((x) => x.name === name)
  if (!w) {
    // workloads 可能因挂载时集群不可达而缺失：重载一次再试
    await loadWorkloads()
    w = workloads.value.find((x) => x.name === name)
    if (!w) {
      ElMessage.warning(`服务「${name}」不在当前工作负载列表中，请刷新后重试`)
      return
    }
  }
  openDetail(w)
}
// swarm service 操作：通过名字查 workloads 找到原始 Workload 再调用
async function swarmAction(name: string, fn: (w: Workload) => void) {
  let w = workloads.value.find((x) => x.name === name)
  if (!w) {
    await loadWorkloads()
    w = workloads.value.find((x) => x.name === name)
    if (!w) {
      ElMessage.warning(`服务「${name}」不在当前工作负载列表中，请刷新后重试`)
      return
    }
  }
  fn(w)
}
// global 模式服务不可缩放（模板里用它隐藏「缩放」按钮）
function isGlobalWorkload(name: string): boolean {
  return workloads.value.find((x) => x.name === name)?.mode === 'global'
}

function sourceTagType(source: string) {
  return source === 'swarm' ? 'info' : 'warning'
}
function sourceLabel(source: string) {
  return source === 'swarm' ? 'swarm' : '纳管'
}
// 状态标签：inventory 状态可能是 "2/3"（副本串）或 "3 running"，解析出健康色
function statusTagType(status: string) {
  if (status === 'running' || status === 'ok') return 'success'
  if (status === 'down' || status === 'not-found' || status === 'unreachable' || status === 'exited') return 'danger'
  if (status === 'node-not-found' || status === 'invalid-ref') return 'warning'
  const m = /^(\d+)\/(\d+)$/.exec(status)
  if (m) {
    const [cur, want] = [Number(m[1]), Number(m[2])]
    if (want > 0 && cur >= want) return 'success'
    if (cur > 0) return 'warning'
    return 'danger'
  }
  return 'info'
}

// 纳管配置编辑器：快照当前清单交给组件（组件内表单/YAML 编辑与校验）；
// key 递增强制重建，避免组件复用旧状态
function openInvConfig() {
  invConfigSnapshot.value = cluster.value?.inventory ?? null
  invEditorKey.value++
  invConfigVisible.value = true
}

// 组件校验通过后回调（含空清单二次确认）
async function saveInvConfig(config: InventoryConfig) {
  invSaving.value = true
  try {
    await inventoryApi.update(clusterName.value, config)
    ElMessage.success('纳管清单已保存')
    invConfigVisible.value = false
    await Promise.all([loadInventory(), fetchCluster()])
  } catch {
    // 校验/保存错误已由组件与 http.ts 提示，不再重复弹
  } finally {
    invSaving.value = false
  }
}

// 对话框「保存」→ 触发组件内部校验（通过后 emit save）
function submitInvConfig() {
  invEditorRef.value?.doSave()
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

function closeNodeStream() {
  nodeStream?.close()
  nodeStream = null
}

// 订阅节点 stats 的 SSE 流：base 节点列表由 "init" 事件立即带出，随后每个
// 节点的 stats 采样完成时发一条 "node" 事件。逐节点把 reachable/cpu/mem/
// 容器数填回并标记为 ready；慢节点不阻塞其它。流以 [DONE] 结束；出错或组件
// 卸载时主动关闭，避免 EventSource 自动重连。
function watchNodeStats() {
  closeNodeStream()
  for (const k of Object.keys(nodeStatsReady)) delete nodeStatsReady[k]
  const url = nodeApi.streamUrl(clusterName.value)
  if (!url.includes('token=')) return // 未登录，无 token 可订阅
  let es: EventSource
  try {
    es = new EventSource(url)
  } catch {
    return
  }
  nodeStream = es
  es.addEventListener('init', (ev: MessageEvent) => {
    try {
      const list = JSON.parse(ev.data) as ClusterNode[]
      if (Array.isArray(list)) nodes.value = list
    } catch {
      /* ignore malformed */
    }
  })
  es.addEventListener('node', (ev: MessageEvent) => {
    try {
      const s = JSON.parse(ev.data) as {
        nodeId: string
        reachable?: boolean
        cpuPercent?: number
        memPercent?: number
        containerCount?: number
      }
      const idx = nodes.value.findIndex((n) => n.id === s.nodeId)
      if (idx < 0) return
      nodes.value[idx] = {
        ...nodes.value[idx],
        reachable: s.reachable ?? false,
        cpuPercent: s.cpuPercent ?? 0,
        memPercent: s.memPercent ?? 0,
        containerCount: s.containerCount ?? 0,
      }
      nodeStatsReady[s.nodeId] = true
    } catch {
      /* ignore */
    }
  })
  // [DONE] 无 event 名，落到 onmessage：关闭以免 EventSource 自动重连。
  es.onmessage = (ev: MessageEvent) => {
    if (ev.data === '[DONE]') closeNodeStream()
  }
  es.onerror = () => closeNodeStream()
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
  procPage.value = 1
  try {
    const resp = await nodeApi.processes(clusterName.value, currentNode.value.id, {
      top,
      limit: 500,
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
  try {
    const resp = await workloadApi.list(clusterName.value)
    workloads.value = resp.items ?? []
  } catch {
    workloads.value = []
  }
}

// ---- 事件（倒序分页：after_seq = 上一页最小 seq） ----
const EVENT_PAGE = 50

async function loadEvents(more = false) {
  eventsLoading.value = true
  try {
    const params: { limit: number; after_seq?: number } = { limit: EVENT_PAGE }
    if (more && eventsAfterSeq.value > 0) params.after_seq = eventsAfterSeq.value
    const resp = await eventApi.list(clusterName.value, params)
    const items = resp.items ?? []
    if (more) {
      events.value.push(...items)
    } else {
      events.value = items
    }
    // 返回条数等于页大小 → 可能还有更早的；且以本页最小 seq 作为下一页游标
    const minSeq = items.length ? Math.min(...items.map((e) => e.seq ?? 0)) : 0
    eventsHasMore.value = items.length >= EVENT_PAGE && minSeq > 0
    if (minSeq > 0) eventsAfterSeq.value = minSeq
    eventsLoadedAt.value = Date.now()
  } catch {
    events.value = []
    eventsHasMore.value = false
  } finally {
    eventsLoading.value = false
  }
}

// ---- 部署 / 编辑 ----
function openDeploy() {
  deployMode.value = 'deploy'
  deployTarget.value = ''
  deployHasSnapshot.value = false
  deployConfig.value = `service:\n  name: web\n  image: nginx:alpine\n  replicas: 1\n  labels:\n    category: ${deployCategory.value}\n  ports:\n    - { target: 80, published: 8080 }\n# monitoring:              # 可选：监控（端口/HTTP/日志/资源阈值）\n#   enabled: true\n#   portChecks:\n#     - { port: "8080" }\n#   httpChecks:\n#     - { url: "http://localhost:8080/health", expectedStatus: [200] }`
  deployVisible.value = true
}

// 编辑已有服务：预填最近一次部署/更新的配置快照（svccfg）
async function openEditService(row: InventoryView) {
  deployMode.value = 'edit'
  deployTarget.value = row.name
  deployVisible.value = true
  deploying.value = true
  try {
    const detail = await workloadApi.get(clusterName.value, row.name)
    deployHasSnapshot.value = !!detail.config
    deployConfig.value =
      detail.config ??
      `service:\n  name: ${row.name}\n  image: ${row.image || ''}\n  replicas: 1\n# 无历史配置快照——将整体替换服务配置`
    if (row.category) deployCategory.value = row.category
  } catch {
    deployHasSnapshot.value = false
    deployConfig.value = ''
    ElMessage.error('读取服务配置失败，请稍后重试')
  } finally {
    deploying.value = false
  }
}

async function deploy() {
  if (!deployConfig.value.trim()) {
    ElMessage.warning('请填写服务配置')
    return
  }
  deploying.value = true
  try {
    const body = { config: deployConfig.value }
    const op =
      deployMode.value === 'edit' && deployTarget.value
        ? await workloadApi.update(clusterName.value, deployTarget.value, body)
        : await workloadApi.deploy(clusterName.value, body)
    deployVisible.value = false
    ElNotification.success({
      title: deployMode.value === 'edit' ? '更新已提交' : '部署已提交',
      message: `操作 ${op.id}`,
    })
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
  await Promise.all([loadWorkloads(), loadInventory()])
}

// ---- 重启纳管清单里的 standalone 容器 ----
async function restartStandalone(row: InventoryView) {
  const container = row.ref || row.name
  await ElMessageBox.confirm(
    `确定重启容器「${container}」（节点 ${row.node || '未知'}）？`,
    '重启容器',
    { type: 'warning' },
  )
  await nodeApi.restartContainer(clusterName.value, row.node ?? '', container)
  ElMessage.success('重启已提交')
  await loadInventory()
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
        await Promise.all([loadWorkloads(), loadInventory()])
        return
      }
    } catch {
      return
    }
  }
  ElMessage.warning(`操作 ${op.type} 长时间未收敛，请稍后到服务列表刷新查看`)
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
    // 非 follow 只取最近 300 行；follow 先取最近 50 行再跟随——避免全量历史
    const tail = follow ? 50 : 300
    const resp = await fetch(workloadApi.logsUrl(clusterName.value, current.value.name, follow, tail), {
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

// 关闭详情抽屉：断开日志流，避免后台持续拉取
function onDetailClosed() {
  logAbort?.abort()
  logAbort = null
  logFollow.value = false
  detail.value = null
  current.value = null
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

// 切 Tab 时按需加载：事件懒加载（30s 内不重复拉）；服务/中间件在 onMounted 预载
watch(tab, (t) => {
  if (t === 'events' && Date.now() - eventsLoadedAt.value > 30_000) void loadEvents()
})

// 详情→详情直跳（路由参数变化）：重置全部数据并重载
watch(clusterName, async () => {
  logAbort?.abort()
  events.value = []
  eventsAfterSeq.value = 0
  eventsHasMore.value = false
  eventsLoadedAt.value = 0
  inventoryViews.value = []
  workloads.value = []
  nodes.value = []
  await Promise.all([fetchCluster(), loadNodes(), loadWorkloads(), loadInventory()])
  watchNodeStats()
})

onMounted(async () => {
  loading.value = true
  // 并行加载集群信息 + 节点 + 工作负载 + 纳管清单（fetchCluster 含一次探测，
  // 不再串行阻塞首屏；离线时其余数据照常渲染）
  await Promise.all([fetchCluster(), loadNodes(), loadWorkloads(), loadInventory()])
  // base 节点列表已秒回（/nodes 不再 fan-out stats）；订阅 SSE 让各节点 stats
  // 逐个流入，慢节点不阻塞首屏。
  watchNodeStats()
  loading.value = false
})

onBeforeUnmount(() => {
  logAbort?.abort()
  closeNodeStream()
})
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
.head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
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

.events-more {
  display: flex;
  justify-content: center;
  margin-top: 12px;
}
.edit-no-snapshot {
  margin-top: 6px;
  font-size: 12px;
  color: var(--el-color-warning);
  line-height: 1.6;
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
  background: var(--og-bg-code);
  color: var(--og-text-code);
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
  color: var(--og-text-dim);
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
